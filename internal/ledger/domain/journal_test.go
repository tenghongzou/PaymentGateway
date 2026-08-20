package domain

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tenghongzou/paymentgateway/pkg/money"
)

func twd(n int64) money.Money { return money.Money{AmountMinor: n, Currency: "TWD"} }

func validJournal(m uuid.UUID) *Journal {
	return &Journal{
		EventID:       uuid.New(),
		MerchantID:    m,
		Livemode:      true,
		ReferenceType: RefPayment,
		ReferenceID:   "pay_x",
		Entries: []Entry{
			{Account: PSPReceivable("stripe", "TWD", true), Direction: Debit, Amount: twd(1000)},
			{Account: MerchantPayable(m, "TWD", true), Direction: Credit, Amount: twd(967)},
			{Account: FeeRevenue("TWD", true), Direction: Credit, Amount: twd(33)},
		},
	}
}

func TestJournal_Validate(t *testing.T) {
	m := uuid.New()
	other := uuid.New()
	tests := []struct {
		name    string
		mutate  func(j *Journal)
		wantErr error
	}{
		{"ok", func(*Journal) {}, nil},
		{"missing event id", func(j *Journal) { j.EventID = uuid.Nil }, ErrEventIDMissing},
		{"bad reference type", func(j *Journal) { j.ReferenceType = "bogus" }, ErrReferenceTypeInvalid},
		{"one entry", func(j *Journal) { j.Entries = j.Entries[:1] }, ErrJournalTooFewEntries},
		{"unbalanced", func(j *Journal) { j.Entries[1].Amount = twd(900) }, ErrJournalUnbalanced},
		{"zero amount", func(j *Journal) { j.Entries[2].Amount = twd(0); j.Entries[1].Amount = twd(1000) }, ErrEntryAmountInvalid},
		{"negative amount", func(j *Journal) { j.Entries[2].Amount = twd(-33) }, ErrInvalidCurrency},
		{"bad direction", func(j *Journal) { j.Entries[0].Direction = "sideways" }, ErrEntryDirectionInvalid},
		{"mixed entry currency", func(j *Journal) {
			j.Entries[2].Amount = money.Money{AmountMinor: 33, Currency: "USD"}
		}, ErrJournalCurrencyMismatch},
		{"account currency mismatch", func(j *Journal) { j.Entries[2].Account = FeeRevenue("USD", true) }, ErrJournalCurrencyMismatch},
		{"account livemode mismatch", func(j *Journal) { j.Entries[2].Account = FeeRevenue("TWD", false) }, ErrAccountLivemodeMismatch},
		{"other merchant account", func(j *Journal) { j.Entries[1].Account = MerchantPayable(other, "TWD", true) }, ErrAccountMerchantMismatch},
		{"bad account code", func(j *Journal) { j.Entries[0].Account.Code = "psp_receivable" }, ErrInvalidAccountCode},
		{"overflow", func(j *Journal) {
			j.Entries = []Entry{
				{Account: PSPReceivable("stripe", "TWD", true), Direction: Debit, Amount: twd(maxInt64)},
				{Account: PSPReceivable("stripe", "TWD", true), Direction: Debit, Amount: twd(1)},
				{Account: FeeRevenue("TWD", true), Direction: Credit, Amount: twd(1)},
			}
		}, ErrEntryAmountInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			j := validJournal(m)
			tt.mutate(j)
			err := j.Validate()
			if tt.wantErr == nil {
				require.NoError(t, err)
				assert.Equal(t, int64(1000), j.TotalDebit())
				assert.Equal(t, int64(1000), j.TotalCredit())
				assert.Equal(t, "TWD", j.Currency())
				return
			}
			assert.ErrorIs(t, err, tt.wantErr)
		})
	}
}

func TestJournal_SystemJournalAllowsAnyMerchantAccounts(t *testing.T) {
	// MerchantID = Nil 的系統 journal（例如調整分錄）可同時觸及多個商戶帳戶。
	a, b := uuid.New(), uuid.New()
	j := &Journal{
		EventID: uuid.New(), Livemode: true, ReferenceType: RefAdjustment, ReferenceID: "ticket-1",
		Entries: []Entry{
			{Account: MerchantPayable(a, "TWD", true), Direction: Debit, Amount: twd(10)},
			{Account: MerchantPayable(b, "TWD", true), Direction: Credit, Amount: twd(10)},
		},
	}
	require.NoError(t, j.Validate())
}

func TestJournal_AccountKeys(t *testing.T) {
	m := uuid.New()
	j := validJournal(m)
	j.Entries = append(j.Entries, Entry{Account: FeeRevenue("TWD", true), Direction: Debit, Amount: twd(1)},
		Entry{Account: MerchantPayable(m, "TWD", true), Direction: Credit, Amount: twd(1)})
	keys := j.AccountKeys()
	assert.Len(t, keys, 3)
	assert.Equal(t, PSPReceivable("stripe", "TWD", true), keys[0])
}

func TestReverse(t *testing.T) {
	m := uuid.New()
	orig := validJournal(m)
	orig.ID = uuid.New()
	orig.PublicID = "jrn_orig"
	orig.Template = TemplateJCAP
	orig.Metadata = map[string]string{MetaPaymentID: "pay_x", MetaTemplate: TemplateJCAP}
	for i := range orig.Entries {
		orig.Entries[i].AccountID = uuid.New()
	}
	now := time.Now()
	evID := uuid.New()

	rev, err := Reverse(orig, evID, "", now)
	require.NoError(t, err)
	assert.Equal(t, RefReversal, rev.ReferenceType)
	assert.Equal(t, SourceReversal, rev.SourceType)
	assert.Equal(t, TemplateJREV, rev.Template)
	assert.Equal(t, evID, rev.EventID)
	require.NotNil(t, rev.ReversalOf)
	assert.Equal(t, orig.ID, *rev.ReversalOf)
	assert.Equal(t, "pay_x", rev.Metadata[MetaPaymentID])
	assert.NotContains(t, rev.Metadata, MetaTemplate)
	assert.Contains(t, rev.Description, "jrn_orig")
	require.Len(t, rev.Entries, len(orig.Entries))
	for i, e := range rev.Entries {
		assert.Equal(t, orig.Entries[i].Direction.Opposite(), e.Direction)
		assert.Equal(t, orig.Entries[i].Amount, e.Amount)
		assert.Equal(t, orig.Entries[i].Account, e.Account)
		assert.Equal(t, orig.Entries[i].AccountID, e.AccountID)
	}
	require.NoError(t, ValidateReversal(orig, rev))

	// 沖銷後餘額回到原點（P5）
	bal := Balances{}
	require.NoError(t, bal.Apply(orig))
	require.NoError(t, bal.Apply(rev))
	for k, v := range bal {
		assert.Zero(t, v, k)
	}

	// 已沖銷不可再沖
	already := *orig
	r := uuid.New()
	already.ReversedBy = &r
	_, err = Reverse(&already, uuid.New(), "", now)
	assert.ErrorIs(t, err, ErrJournalAlreadyReversed)
	assert.ErrorIs(t, ValidateReversal(&already, rev), ErrJournalAlreadyReversed)

	// 不是鏡像
	bad := *rev
	bad.Entries = append([]Entry(nil), rev.Entries...)
	bad.Entries[0].Amount = twd(999)
	assert.ErrorIs(t, ValidateReversal(orig, &bad), ErrReversalMismatch)

	// reversal_of 指錯
	wrong := *rev
	x := uuid.New()
	wrong.ReversalOf = &x
	assert.ErrorIs(t, ValidateReversal(orig, &wrong), ErrReversalMismatch)

	// 未儲存的 journal 不可沖銷
	_, err = Reverse(validJournal(m), uuid.New(), "", now)
	assert.ErrorIs(t, err, ErrJournalNotFound)
	_, err = Reverse(nil, uuid.New(), "", now)
	assert.ErrorIs(t, err, ErrJournalNotFound)
}

func TestDirection_Opposite(t *testing.T) {
	assert.Equal(t, Credit, Debit.Opposite())
	assert.Equal(t, Debit, Credit.Opposite())
	assert.True(t, Debit.Valid())
	assert.False(t, Direction("x").Valid())
}
