package domain

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tenghongzou/paymentgateway/pkg/ids"
)

func TestDiscrepancy_StateMachine(t *testing.T) {
	newOpen := func() *Discrepancy {
		return &Discrepancy{ID: ids.NewUUID(), Kind: KindAmountMismatch, Status: DiscrepancyOpen}
	}
	t.Run("open → resolved", func(t *testing.T) {
		d := newOpen()
		require.NoError(t, d.Resolve("fixed at PSP", "ops:alice", now))
		assert.Equal(t, DiscrepancyResolved, d.Status)
		assert.Equal(t, "ops:alice", d.ResolvedBy)
		assert.Equal(t, "fixed at PSP", d.ResolutionNote)
		require.NotNil(t, d.ResolvedAt)
		assert.Equal(t, now, *d.ResolvedAt)
		assert.Equal(t, 1, d.Version)
	})
	t.Run("resolved without note allowed", func(t *testing.T) {
		d := newOpen()
		require.NoError(t, d.Resolve("", "ops:alice", now))
	})
	t.Run("open → ignored", func(t *testing.T) {
		d := newOpen()
		require.NoError(t, d.Ignore("PSP settles T+2", "ops:bob", now))
		assert.Equal(t, DiscrepancyIgnored, d.Status)
	})
	t.Run("ignore requires note", func(t *testing.T) {
		d := newOpen()
		require.ErrorIs(t, d.Ignore("  ", "ops:bob", now), ErrResolutionNoteRequired)
		assert.Equal(t, DiscrepancyOpen, d.Status)
	})
	t.Run("resolved_by required", func(t *testing.T) {
		d := newOpen()
		assert.ErrorIs(t, d.Resolve("x", "", now), ErrResolvedByRequired)
	})
	t.Run("resolved → ignored rejected", func(t *testing.T) {
		d := newOpen()
		require.NoError(t, d.Resolve("x", "ops:a", now))
		require.ErrorIs(t, d.Ignore("y", "ops:a", now), ErrInvalidTransition)
		assert.ErrorIs(t, d.Resolve("y", "ops:a", now), ErrInvalidTransition)
	})
	t.Run("ignored → resolved rejected", func(t *testing.T) {
		d := newOpen()
		require.NoError(t, d.Ignore("x", "ops:a", now))
		assert.ErrorIs(t, d.Resolve("y", "ops:a", now), ErrInvalidTransition)
	})
	t.Run("public id round trip", func(t *testing.T) {
		d := newOpen()
		pid := d.PublicID()
		assert.True(t, ids.HasPrefix(pid, PrefixDiscrepancy))
		u, err := ParseDiscrepancyID(pid)
		require.NoError(t, err)
		assert.Equal(t, d.ID, u)
		u, err = ParseDiscrepancyID(d.ID.String())
		require.NoError(t, err)
		assert.Equal(t, d.ID, u)
		_, err = ParseDiscrepancyID("dsc_nope")
		require.ErrorIs(t, err, ErrDiscrepancyNotFound)
		_, err = ParseDiscrepancyID("garbage")
		assert.ErrorIs(t, err, ErrDiscrepancyNotFound)
	})
}

func TestRun_Lifecycle(t *testing.T) {
	start, end, err := PeriodForDate("2026-08-19")
	require.NoError(t, err)
	assert.Equal(t, time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC), start)
	assert.Equal(t, 24*time.Hour, end.Sub(start))
	_, _, err = PeriodForDate("19/08/2026")
	require.ErrorIs(t, err, ErrInvalidPeriod)

	_, err = NewRun(MockProvider, end, start, "api", now)
	require.ErrorIs(t, err, ErrInvalidPeriod)

	r, err := NewRun(MockProvider, start, end, "", now)
	require.NoError(t, err)
	assert.Equal(t, RunPending, r.Status)
	assert.Equal(t, "api", r.TriggeredBy)
	assert.True(t, ids.HasPrefix(r.PublicID, PrefixRun))
	assert.Equal(t, "2026-08-19", r.SettlementDate())
	u, err := ParseRunID(r.PublicID)
	require.NoError(t, err)
	assert.Equal(t, r.ID, u)
	_, err = ParseRunID("rcn_x")
	require.ErrorIs(t, err, ErrRunNotFound)

	require.NoError(t, r.Start(now))
	assert.Equal(t, RunRunning, r.Status)
	require.ErrorIs(t, r.Start(now), ErrInvalidTransition)

	res := NewMatcher().Match(MatchInput{Provider: MockProvider, Now: now,
		Lines:   []SettlementLine{line(LinePayment, "ch_1", 1000, 59, "TWD"), line(LinePayment, "ch_2", 500, 0, "TWD"), line(LineFee, "f", 10, 0, "TWD")},
		Records: []PaymentRecord{record(RecordPayment, "ch_1", StatusCaptured, 1000), record(RecordPayment, "ch_3", StatusCaptured, 7)},
	})
	finish := now.Add(1500 * time.Millisecond)
	require.NoError(t, r.Complete(res, finish))
	assert.Equal(t, RunCompleted, r.Status)
	assert.Equal(t, 1, r.MatchedCount)
	assert.Equal(t, 2, r.UnmatchedCount)
	assert.Equal(t, 3, r.Summary.TotalLines)
	assert.Equal(t, 1, r.Summary.Skipped)
	assert.Equal(t, map[string]int{"missing_in_ledger": 1, "missing_in_psp": 1}, r.Summary.ByKind)
	assert.Equal(t, int64(1500), r.Summary.TotalSettled["TWD"])
	assert.Equal(t, int64(69), r.Summary.TotalFees["TWD"])
	assert.Equal(t, int64(1500), r.Summary.DurationMs)
	assert.Equal(t, 2, r.Version)
	require.ErrorIs(t, r.Fail("x", now), ErrInvalidTransition)
	require.ErrorIs(t, r.Complete(res, now), ErrInvalidTransition)

	r2, err := NewRun(MockProvider, start, end, "scheduler", now)
	require.NoError(t, err)
	require.NoError(t, r2.Fail("parse error", now))
	assert.Equal(t, RunFailed, r2.Status)
	assert.Equal(t, "parse error", r2.Error)

	r3, err := NewRun(MockProvider, start, end, "scheduler", now)
	require.NoError(t, err)
	require.NoError(t, r3.Complete(MatchResult{}, now), "pending 可直接完成")
	assert.NotNil(t, r3.StartedAt)
}

func TestSettlementFile_Lifecycle(t *testing.T) {
	f := NewSettlementFile(MockProvider, "mock.csv", FileHash([]byte("x")), nil, nil, now)
	assert.Equal(t, FileImporting, f.Status)
	assert.NotEqual(t, uuid.Nil, f.ID)
	f.MarkFailed("boom", now)
	assert.Equal(t, FileFailed, f.Status)
	assert.Equal(t, "boom", f.Error)
	f.MarkImporting(now)
	assert.Equal(t, FileImporting, f.Status)
	assert.Empty(t, f.Error)
	f.MarkImported(6, now)
	assert.Equal(t, FileImported, f.Status)
	assert.Equal(t, 6, f.RowCount)
	require.NotNil(t, f.ImportedAt)
}

func TestRecordKindMapping(t *testing.T) {
	for _, k := range []RecordKind{RecordPayment, RecordRefund, RecordDispute} {
		lt := k.LineType()
		back, ok := RecordKindForLine(lt)
		require.True(t, ok)
		assert.Equal(t, k, back)
	}
	_, ok := RecordKindForLine(LineFee)
	assert.False(t, ok)
	assert.Equal(t, LineType(""), RecordKind("x").LineType())
}
