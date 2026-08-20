package postgres

import (
	"errors"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/tenghongzou/paymentgateway/internal/ledger/domain"
	"github.com/tenghongzou/paymentgateway/pkg/apperr"
	"github.com/tenghongzou/paymentgateway/pkg/pgdb"
)

// ErrAppendOnly 表示嘗試 UPDATE / DELETE 帳本（被 reject_mutation trigger 拒絕）。
var ErrAppendOnly = apperr.New(apperr.TypeInvalidRequest, "invalid_state_transition", "The ledger is append-only; post a reversing journal instead.")

// PostgreSQL SQLSTATE（pgdb 未涵蓋者）。
const sqlstateIntegrityViolation = "23000"

// translateError 把 CHECK / trigger / 唯一鍵錯誤轉成領域錯誤；其他錯誤原樣回傳。
func translateError(err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	msg := pgErr.Message
	switch {
	case pgdb.IsUniqueViolation(err):
		switch pgErr.ConstraintName {
		case "journals_event_id_key", "journals_pkey", "journals_public_id_key":
			return domain.ErrDuplicateEvent.Wrap(err)
		}
		return apperr.New(apperr.TypeInvalidRequest, "invalid_state_transition", "Unique constraint violated: "+pgErr.ConstraintName).Wrap(err)
	case pgdb.IsCheckViolation(err):
		switch {
		case strings.Contains(msg, "must be active"):
			return domain.ErrAccountInactive.Wrap(err)
		case strings.Contains(msg, "does not match account"):
			return domain.ErrJournalCurrencyMismatch.Wrap(err)
		case pgErr.ConstraintName == "entries_amount_check":
			return domain.ErrEntryAmountInvalid.Wrap(err)
		case pgErr.ConstraintName == "accounts_type_normal_balance":
			return domain.ErrInvalidAccountCode.Wrap(err)
		case pgErr.ConstraintName == "journals_reference_type_check":
			return domain.ErrReferenceTypeInvalid.Wrap(err)
		case strings.Contains(pgErr.ConstraintName, "currency"):
			return domain.ErrInvalidCurrency.Wrap(err)
		}
		return apperr.ErrParameterInvalid.WithMessage("%s", msg).Wrap(err)
	case pgdb.IsForeignKeyViolation(err):
		if strings.Contains(msg, "account") {
			return domain.ErrAccountNotFound.Wrap(err)
		}
		if strings.Contains(msg, "journal") {
			return domain.ErrJournalNotFound.Wrap(err)
		}
		return apperr.ErrParameterInvalid.WithMessage("%s", msg).Wrap(err)
	case pgErr.Code == sqlstateIntegrityViolation:
		// assert_journal_balanced / reject_mutation 以 integrity_constraint_violation 拋出。
		switch {
		case strings.Contains(msg, "unbalanced"):
			return domain.ErrJournalUnbalanced.Wrap(err)
		case strings.Contains(msg, "at least two entries"):
			return domain.ErrJournalTooFewEntries.Wrap(err)
		case strings.Contains(msg, "mixes"):
			return domain.ErrJournalCurrencyMismatch.Wrap(err)
		case strings.Contains(msg, "append-only"):
			return ErrAppendOnly.Wrap(err)
		}
		return apperr.ErrParameterInvalid.WithMessage("%s", msg).Wrap(err)
	}
	return err
}
