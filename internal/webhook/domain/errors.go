package domain

import (
	"errors"

	"github.com/tenghongzou/paymentgateway/pkg/apperr"
)

// 對外（gRPC）可見的業務錯誤，pkg/grpcx 會依 (type, code) 轉成對應的 status code。
var (
	// ErrDeliveryNotFound 查無 delivery（含跨商戶）。→ NOT_FOUND
	ErrDeliveryNotFound = apperr.ErrResourceMissing.WithMessage("No such webhook delivery.")
	// ErrEventNotFound 查無事件。→ NOT_FOUND
	ErrEventNotFound = apperr.ErrResourceMissing.WithMessage("No such webhook event.")
	// ErrDeliveryNotRetryable delivery 為 pending / in_flight / canceled，不可手動重送。→ FAILED_PRECONDITION
	ErrDeliveryNotRetryable = apperr.New(apperr.TypeInvalidRequest, "invalid_state_transition",
		"Delivery can only be retried when it is failed, dead_letter or succeeded.")
	// ErrEndpointUnavailable 端點已停用 / 刪除 / 不存在，不能重送。→ FAILED_PRECONDITION
	ErrEndpointUnavailable = apperr.New(apperr.TypeInvalidRequest, "invalid_state_transition",
		"Webhook endpoint is disabled or deleted.")
	// ErrIdempotencyKeyMissing 手動重送缺少冪等鍵。→ INVALID_ARGUMENT
	ErrIdempotencyKeyMissing = apperr.ErrIdempotencyMissing
)

// 內部錯誤（不直接對外）。
var (
	// ErrInvalidTransition 表示 delivery 狀態轉移不合法（程式錯誤或並發競爭）。
	ErrInvalidTransition = errors.New("webhook: invalid delivery state transition")
	// ErrUnsupportedEvent 表示來源事件型別未知或 payload 缺失。
	ErrUnsupportedEvent = errors.New("webhook: unsupported payment event")
	// ErrInvalidID 表示 ID 格式不正確。
	ErrInvalidID = errors.New("webhook: invalid id")
	// ErrNoSecrets 表示端點沒有任何可用的簽章 secret。
	ErrNoSecrets = errors.New("webhook: endpoint has no signing secret")
	// ErrURLNotAllowed 表示端點 URL 違反 SSRF 政策（scheme / host / port）。
	ErrURLNotAllowed = errors.New("webhook: endpoint url not allowed")
	// ErrIPNotAllowed 表示解析出的 IP 落在禁止範圍（loopback / private / link-local / metadata）。
	ErrIPNotAllowed = errors.New("webhook: destination ip not allowed")
)
