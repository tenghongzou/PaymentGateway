// Package providermock 實作 provider-mock：以 card token 決定情境的模擬 PSP（docs/09 §3）。
//
// 目錄結構與真實 adapter 相同（adapter 無 DB，因此沒有 domain / app 分層）：
//   - store.go     記憶體交易狀態（provider_reference → 狀態）與 PSP 冪等鍵
//   - scenarios.go token → 行為對照
//   - service.go   ProviderAdapter gRPC 實作
package providermock

import (
	"sync"
	"time"

	"github.com/tenghongzou/paymentgateway/pkg/money"
)

// TxnStatus 為 mock 端的交易狀態。
type TxnStatus string

// 交易狀態列舉。
const (
	TxnRequiresAction TxnStatus = "requires_action"
	TxnAuthorized     TxnStatus = "authorized"
	TxnCaptured       TxnStatus = "captured"
	TxnVoided         TxnStatus = "voided"
	TxnRefunded       TxnStatus = "refunded"
	TxnPartialRefund  TxnStatus = "partially_refunded"
	TxnFailed         TxnStatus = "failed"
)

// Txn 為 mock 內部交易紀錄。
type Txn struct {
	Reference  string
	PaymentID  string
	Token      string
	Status     TxnStatus
	Amount     money.Money
	Captured   money.Money
	Refunded   money.Money
	AuthExpiry time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
	Refunds    map[string]string // refund_id → provider refund reference
}

// Store 為執行緒安全的記憶體儲存。
type Store struct {
	mu         sync.Mutex
	txns       map[string]*Txn
	idem       map[string]string // psp idempotency key → provider reference
	seenTokens map[string]int    // payment_id|token → 次數（tok_unavailable_once 用）
}

// NewStore 建立 Store。
func NewStore() *Store {
	return &Store{txns: map[string]*Txn{}, idem: map[string]string{}, seenTokens: map[string]int{}}
}

// Get 取得交易（複本）。
func (s *Store) Get(ref string) (Txn, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.txns[ref]
	if !ok {
		return Txn{}, false
	}
	return *t, true
}

// Put 寫入交易。
func (s *Store) Put(t *Txn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.txns[t.Reference] = t
}

// Update 在鎖內修改交易；找不到回 false。
func (s *Store) Update(ref string, fn func(t *Txn)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.txns[ref]
	if !ok {
		return false
	}
	fn(t)
	t.UpdatedAt = time.Now()
	return true
}

// IdemGet 查詢 PSP 冪等鍵對應的 reference。
func (s *Store) IdemGet(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ref, ok := s.idem[key]
	return ref, ok
}

// IdemPut 記錄 PSP 冪等鍵。
func (s *Store) IdemPut(key, ref string) {
	if key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.idem[key] = ref
}

// CountToken 回傳並遞增某 (payment_id, token) 被看到的次數（從 1 開始）。
func (s *Store) CountToken(paymentID, token string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := paymentID + "|" + token
	s.seenTokens[k]++
	return s.seenTokens[k]
}

// Len 回傳交易數（測試用）。
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.txns)
}
