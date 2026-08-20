// Package domain 為 ledger-service 的領域層：科目表、帳戶、日記帳 / 分錄、手續費模型、分錄範本與帳本不變條件。
//
// 規則（docs/01 §7 import 規則）：本套件只 import 標準庫與 pkg/（以及 google/uuid），
// 不得 import app / adapter，也不得 import 其他 internal/*。
package domain

import (
	"errors"

	"github.com/tenghongzou/paymentgateway/pkg/apperr"
)

// 領域錯誤（type / code 沿用 docs/01 §8 與 docs/02 §10 的格式；ledger 專屬 code 見 api/proto ledger.proto 註解）。
var (
	// ErrJournalUnbalanced 借貸合計不相等。
	ErrJournalUnbalanced = apperr.New(apperr.TypeInvalidRequest, "journal_unbalanced", "Journal debits and credits must be equal.").WithParam("entries")
	// ErrJournalTooFewEntries 分錄少於 2 筆。
	ErrJournalTooFewEntries = apperr.New(apperr.TypeInvalidRequest, "journal_too_few_entries", "A journal needs at least two entries.").WithParam("entries")
	// ErrJournalCurrencyMismatch 分錄之間、或分錄與帳戶幣別不一致。
	ErrJournalCurrencyMismatch = apperr.New(apperr.TypeInvalidRequest, "currency_mismatch", "All entries in a journal must share the same currency as their accounts.").WithParam("entries")
	// ErrEntryAmountInvalid 分錄金額必須 > 0。
	ErrEntryAmountInvalid = apperr.New(apperr.TypeInvalidRequest, "amount_too_small", "Entry amount must be greater than zero.").WithParam("entries.amount")
	// ErrEntryDirectionInvalid 分錄方向必須為 debit / credit。
	ErrEntryDirectionInvalid = apperr.New(apperr.TypeInvalidRequest, "parameter_invalid", "Entry direction must be debit or credit.").WithParam("entries.direction")
	// ErrInvalidAccountCode 科目代碼不在 Chart of Accounts 中、或系統 / 商戶層級不符。
	ErrInvalidAccountCode = apperr.New(apperr.TypeInvalidRequest, "parameter_invalid", "Account code is not part of the chart of accounts.").WithParam("code")
	// ErrInvalidCurrency 幣別不受支援。
	ErrInvalidCurrency = apperr.New(apperr.TypeInvalidRequest, "invalid_currency", "Currency must be a supported ISO 4217 code.").WithParam("currency")
	// ErrAccountNotFound 帳戶不存在。
	ErrAccountNotFound = apperr.New(apperr.TypeInvalidRequest, "resource_missing", "No such ledger account.").WithParam("account_id")
	// ErrAccountInactive 帳戶非 active，不可過帳。
	ErrAccountInactive = apperr.New(apperr.TypeInvalidRequest, "invalid_state_transition", "The ledger account is not active.").WithParam("account_id")
	// ErrAccountMerchantMismatch 商戶帳戶不屬於 journal 的商戶。
	ErrAccountMerchantMismatch = apperr.New(apperr.TypeInvalidRequest, "parameter_invalid", "Entry account belongs to a different merchant than the journal.").WithParam("entries.account_id")
	// ErrAccountLivemodeMismatch 帳戶 livemode 與 journal 不一致。
	ErrAccountLivemodeMismatch = apperr.New(apperr.TypeInvalidRequest, "livemode_mismatch", "Entry account livemode does not match the journal.").WithParam("entries.account_id")
	// ErrJournalNotFound journal 不存在。
	ErrJournalNotFound = apperr.New(apperr.TypeInvalidRequest, "resource_missing", "No such journal.").WithParam("journal_id")
	// ErrJournalAlreadyReversed 一個 journal 只能被沖銷一次。
	ErrJournalAlreadyReversed = apperr.New(apperr.TypeInvalidRequest, "invalid_state_transition", "The journal has already been reversed.").WithParam("reverses_journal_id")
	// ErrReversalMismatch 沖銷分錄必須與原 journal 金額相同、方向相反。
	ErrReversalMismatch = apperr.New(apperr.TypeInvalidRequest, "parameter_invalid", "Reversal entries must mirror the original journal (same accounts and amounts, opposite direction).").WithParam("entries")
	// ErrEventIDMissing journal 缺少冪等鍵（event_id）。
	ErrEventIDMissing = apperr.New(apperr.TypeInvalidRequest, "parameter_missing", "idempotency_key (event_id) is required.").WithParam("idempotency_key")
	// ErrReferenceTypeInvalid reference_type 不在允許清單。
	ErrReferenceTypeInvalid = apperr.New(apperr.TypeInvalidRequest, "parameter_invalid", "reference_type is invalid.").WithParam("reference_type")
	// ErrFeeExceedsAmount 手續費不可超過金額。
	ErrFeeExceedsAmount = apperr.New(apperr.TypeInvalidRequest, "parameter_invalid", "Fee must not exceed the transaction amount.").WithParam("fee")
	// ErrEventInvalid 事件內容不完整（缺金額 / 商戶 / provider 等）。
	ErrEventInvalid = apperr.New(apperr.TypeInvalidRequest, "parameter_invalid", "Payment event is missing data required for posting.")
	// ErrDuplicateEvent 同一 event_id 已過帳（呼叫端視為冪等重放，不是錯誤）。
	ErrDuplicateEvent = apperr.New(apperr.TypeIdempotency, "idempotency_key_in_use", "A journal for this event_id has already been posted.")
)

// ErrNoTemplate 表示此事件類型不需記帳（授權、失敗、證據提交等）；消費者應直接 ack。
var ErrNoTemplate = errors.New("ledger: event type has no journal template")
