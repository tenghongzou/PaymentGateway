package domain

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// DeliveryStatus 為 webhook_deliveries.status（migrations/webhook/0001 的 CHECK）。
type DeliveryStatus string

// delivery 狀態全集。
const (
	StatusPending    DeliveryStatus = "pending"
	StatusInFlight   DeliveryStatus = "in_flight"
	StatusSucceeded  DeliveryStatus = "succeeded"
	StatusFailed     DeliveryStatus = "failed"
	StatusDeadLetter DeliveryStatus = "dead_letter"
	StatusCanceled   DeliveryStatus = "canceled"
)

// IsTerminal 回傳狀態是否為終態（不會再被 dispatcher 取件）。
func (s DeliveryStatus) IsTerminal() bool {
	return s == StatusSucceeded || s == StatusDeadLetter || s == StatusCanceled
}

// 投遞常數（docs/06 §4.4）。
const (
	// MaxAttempts 為自動重試上限；第 MaxAttempts 次仍失敗 → dead_letter。
	MaxAttempts = 10
	// MaxResponseBodyBytes 為記錄的回應 body 上限（webhook_deliveries.last_response_body CHECK ≤ 4096）。
	MaxResponseBodyBytes = 4096
	// MaxRetryAfter 為 429 Retry-After 可延後的上限。
	MaxRetryAfter = time.Hour
	// JitterRatio 為每次退避間隔的 ±jitter 比例。
	JitterRatio = 0.2
	// InFlightTimeout 為 in_flight 超過多久未收斂視為 worker 崩潰、由 reaper 轉回 failed。
	InFlightTimeout = 2 * time.Minute
)

// schedule[n] 為「第 n 次嘗試」距上一次的間隔（n 從 1 起算；index 0 未使用）。
var schedule = [...]time.Duration{
	0, // index 0（未使用）
	0, // 1：立即
	1 * time.Minute,
	5 * time.Minute,
	30 * time.Minute,
	2 * time.Hour,
	6 * time.Hour,
	12 * time.Hour,
	24 * time.Hour,
	24 * time.Hour,
	24 * time.Hour, // 10
}

// Backoff 回傳第 attemptNo 次嘗試距上一次的基礎間隔（不含 jitter）。超出時程表一律 24h。
func Backoff(attemptNo int) time.Duration {
	if attemptNo <= 1 {
		return 0
	}
	if attemptNo >= len(schedule) {
		return schedule[len(schedule)-1]
	}
	return schedule[attemptNo]
}

// NextAttemptAt 計算第 failedAttemptNo 次失敗後的下次嘗試時間：now + Backoff(failedAttemptNo+1) ± 20% jitter。
// rnd 須為 [0,1) 的亂數（0.5 代表無 jitter），由呼叫端注入以利測試。
func NextAttemptAt(now time.Time, failedAttemptNo int, rnd float64) time.Time {
	base := Backoff(failedAttemptNo + 1)
	if base == 0 {
		return now
	}
	if rnd < 0 {
		rnd = 0
	}
	if rnd >= 1 {
		rnd = 0.999999
	}
	// factor ∈ [1-Jitter, 1+Jitter)
	factor := 1 - JitterRatio + 2*JitterRatio*rnd
	return now.Add(time.Duration(float64(base) * factor))
}

// Delivery 為 (event, endpoint) 的投遞狀態機（webhook_deliveries 一列）。
type Delivery struct {
	ID         uuid.UUID
	EventID    uuid.UUID
	EndpointID uuid.UUID
	MerchantID uuid.UUID
	// AttemptNo 為已（開始）嘗試的次數；取件時 +1，因此 in_flight 期間即為本次嘗試序號。
	AttemptNo          int
	Status             DeliveryStatus
	NextAttemptAt      time.Time
	LastAttemptAt      *time.Time
	LastResponseStatus *int
	LastResponseBody   *string
	LastError          *string
	DeliveredAt        *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
	Version            int

	// 以下為查詢時由 webhook_events 帶出的投影欄位（非 webhook_deliveries 欄位），供 gRPC 回應使用。
	EventType    string
	EventPayload []byte
	Livemode     bool
	OccurredAt   time.Time
}

// PublicID 回傳 whd_ 形式的 delivery ID。
func (d *Delivery) PublicID() string { return DeliveryPublicID(d.ID) }

// NewDelivery 建立 pending delivery（next_attempt_at = now，第一次立即投遞）。
func NewDelivery(ev *Event, ep *Endpoint, now time.Time) *Delivery {
	return &Delivery{
		ID:            uuid.Must(uuid.NewV7()),
		EventID:       ev.ID,
		EndpointID:    ep.ID,
		MerchantID:    ev.MerchantID,
		Status:        StatusPending,
		NextAttemptAt: now.UTC(),
		CreatedAt:     now.UTC(),
		UpdatedAt:     now.UTC(),
		EventType:     ev.Type,
		EventPayload:  ev.Payload,
		Livemode:      ev.Livemode,
		OccurredAt:    ev.OccurredAt,
	}
}

// IsDue 回傳是否可被取件（pending / failed 且 next_attempt_at <= now）。
func (d *Delivery) IsDue(now time.Time) bool {
	return (d.Status == StatusPending || d.Status == StatusFailed) && !d.NextAttemptAt.After(now)
}

// Claim 取件：pending / failed → in_flight，attempt_no + 1（對應 repo 的 UPDATE ... RETURNING）。
func (d *Delivery) Claim(now time.Time) error {
	if !d.IsDue(now) {
		return fmt.Errorf("%w: claim from %s (due at %s)", ErrInvalidTransition, d.Status, d.NextAttemptAt)
	}
	d.Status = StatusInFlight
	d.AttemptNo++
	d.Version++
	d.UpdatedAt = now.UTC()
	return nil
}

// Outcome 為一次 HTTP 嘗試的結果。
type Outcome struct {
	// StatusCode 為 HTTP 狀態碼；連線失敗 / 逾時為 0。
	StatusCode int
	// Body 為回應 body（呼叫端可先截斷；此處仍會保證 ≤ MaxResponseBodyBytes）。
	Body string
	// Err 為連線 / 逾時 / SSRF 錯誤（無 HTTP 回應時）。
	Err error
	// RetryAfter 為 429 的 Retry-After（已解析；0 表示未提供）。
	RetryAfter time.Duration
	// Duration 為耗時。
	Duration time.Duration
}

// Succeeded 回傳是否為 2xx。
func (o Outcome) Succeeded() bool { return o.StatusCode >= 200 && o.StatusCode < 300 }

// Transition 為 ApplyOutcome 的結果分類。
type Transition int

// ApplyOutcome 的可能結果。
const (
	TransitionSucceeded  Transition = iota + 1 // → succeeded
	TransitionRetry                            // → failed，已排定 next_attempt_at
	TransitionDeadLetter                       // → dead_letter（第 MaxAttempts 次失敗）
	TransitionGone                             // 410 → canceled，端點應停用
)

// ApplyOutcome 依 HTTP 結果推進狀態機（必須處於 in_flight），並回傳本次的 Attempt 紀錄。
// rnd 為 [0,1) 亂數供 jitter 使用。
func (d *Delivery) ApplyOutcome(now time.Time, o Outcome, rnd float64) (Transition, *Attempt, error) {
	if d.Status != StatusInFlight {
		return 0, nil, fmt.Errorf("%w: apply outcome in %s", ErrInvalidTransition, d.Status)
	}
	now = now.UTC()
	body := TruncateBody(o.Body)
	att := &Attempt{
		ID:          uuid.Must(uuid.NewV7()),
		DeliveryID:  d.ID,
		AttemptNo:   d.AttemptNo,
		DurationMS:  int(o.Duration / time.Millisecond),
		AttemptedAt: now,
	}
	d.LastAttemptAt = &now
	d.LastResponseStatus, d.LastResponseBody, d.LastError = nil, nil, nil
	if o.StatusCode > 0 {
		code := o.StatusCode
		d.LastResponseStatus = &code
		att.ResponseStatus = &code
		if body != "" {
			b := body
			d.LastResponseBody = &b
			att.ResponseBody = &b
		}
	}
	if o.Err != nil {
		msg := truncate(o.Err.Error(), 1024)
		d.LastError = &msg
		att.Error = &msg
	}
	d.Version++
	d.UpdatedAt = now

	switch {
	case o.Succeeded():
		d.Status = StatusSucceeded
		d.DeliveredAt = &now
		return TransitionSucceeded, att, nil
	case o.StatusCode == 410:
		d.Status = StatusCanceled
		msg := "endpoint returned 410 Gone; endpoint disabled"
		d.LastError = &msg
		return TransitionGone, att, nil
	case d.AttemptNo >= MaxAttempts:
		d.Status = StatusDeadLetter
		return TransitionDeadLetter, att, nil
	default:
		d.Status = StatusFailed
		if o.StatusCode == 429 && o.RetryAfter > 0 {
			d.NextAttemptAt = now.Add(min(o.RetryAfter, MaxRetryAfter))
		} else {
			d.NextAttemptAt = NextAttemptAt(now, d.AttemptNo, rnd)
		}
		return TransitionRetry, att, nil
	}
}

// Reap 把卡住的 in_flight 轉回 failed 並立即可重送（reaper / 5 §7.2 第 5 點）。
func (d *Delivery) Reap(now time.Time) error {
	if d.Status != StatusInFlight {
		return fmt.Errorf("%w: reap in %s", ErrInvalidTransition, d.Status)
	}
	now = now.UTC()
	msg := "in_flight timeout; reclaimed by reaper"
	d.LastError = &msg
	d.NextAttemptAt = now
	if d.AttemptNo >= MaxAttempts {
		d.Status = StatusDeadLetter
	} else {
		d.Status = StatusFailed
	}
	d.Version++
	d.UpdatedAt = now
	return nil
}

// Cancel 於端點停用 / 刪除時取消尚未成功的投遞；終態不變。回傳是否有變更。
func (d *Delivery) Cancel(now time.Time, reason string) bool {
	if d.Status.IsTerminal() {
		return false
	}
	d.Status = StatusCanceled
	if reason != "" {
		d.LastError = &reason
	}
	d.Version++
	d.UpdatedAt = now.UTC()
	return true
}

// CanRetryManually 回傳是否允許手動重送（proto：FAILED / DEAD_LETTER / SUCCEEDED）。
func (d *Delivery) CanRetryManually() bool {
	return d.Status == StatusFailed || d.Status == StatusDeadLetter || d.Status == StatusSucceeded
}

// ResetForRetry 手動重送：重置嘗試視窗（attempt_no=0、pending、立即投遞），保留歷史 last_* 欄位。
func (d *Delivery) ResetForRetry(now time.Time) error {
	if !d.CanRetryManually() {
		return ErrDeliveryNotRetryable
	}
	now = now.UTC()
	d.Status = StatusPending
	d.AttemptNo = 0
	d.NextAttemptAt = now
	d.DeliveredAt = nil
	d.Version++
	d.UpdatedAt = now
	return nil
}

// Attempt 為 webhook_delivery_attempts 一列（append-only）。
type Attempt struct {
	ID             uuid.UUID
	DeliveryID     uuid.UUID
	AttemptNo      int
	ResponseStatus *int
	ResponseBody   *string
	Error          *string
	DurationMS     int
	AttemptedAt    time.Time
}

// Succeeded 回傳此次嘗試是否為 2xx。
func (a *Attempt) Succeeded() bool {
	return a.ResponseStatus != nil && *a.ResponseStatus >= 200 && *a.ResponseStatus < 300
}

// TruncateBody 把回應 body 截斷到 MaxResponseBodyBytes（以 byte 計，並修掉尾端不完整的 UTF-8）。
func TruncateBody(s string) string { return truncate(s, MaxResponseBodyBytes) }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	b := []byte(s[:n])
	// 去掉截斷造成的不完整多位元組字元（最多回退 3 bytes）。
	for i := 0; i < 3 && len(b) > 0; i++ {
		if b[len(b)-1]&0xC0 != 0x80 {
			if b[len(b)-1] >= 0xC0 {
				b = b[:len(b)-1]
			}
			break
		}
		b = b[:len(b)-1]
	}
	return string(b)
}

// Release 在「尚未真正發出 HTTP 請求」就必須放棄本次嘗試時（例如端點資料暫時取不到）把 delivery 放回佇列：
// in_flight → failed、attempt_no 還原、next_attempt_at = now + retryIn；不產生 Attempt 紀錄。
func (d *Delivery) Release(now time.Time, retryIn time.Duration, reason string) error {
	if d.Status != StatusInFlight {
		return fmt.Errorf("%w: release in %s", ErrInvalidTransition, d.Status)
	}
	now = now.UTC()
	if d.AttemptNo > 0 {
		d.AttemptNo--
	}
	d.Status = StatusFailed
	d.NextAttemptAt = now.Add(retryIn)
	if reason != "" {
		d.LastError = &reason
	}
	d.Version++
	d.UpdatedAt = now
	return nil
}
