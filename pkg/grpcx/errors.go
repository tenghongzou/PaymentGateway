package grpcx

import (
	"context"
	"errors"
	"net/http"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	commonv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/common/v1"
	"github.com/tenghongzou/paymentgateway/pkg/apperr"
	"github.com/tenghongzou/paymentgateway/pkg/pgdb"
)

// ErrorFromDomain 把領域錯誤（*apperr.Error）轉成 gRPC status，並夾帶 pg.common.v1.ErrorDetail。
// 非 apperr 的錯誤：context 取消/逾時 → Canceled/DeadlineExceeded；pgdb.ErrNotFound → NotFound；
// pgdb.ErrConcurrentModification → Aborted(concurrent_modification)；其餘 → Internal（不洩漏內部訊息）。
func ErrorFromDomain(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	switch {
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "request canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "deadline exceeded")
	case errors.Is(err, pgdb.ErrNotFound):
		return withDetail(codes.NotFound, apperr.ErrResourceMissing)
	case errors.Is(err, pgdb.ErrConcurrentModification):
		return withDetail(codes.Aborted, apperr.ErrConcurrentModify)
	}
	if e := apperr.From(err); e != nil {
		return withDetail(codeFor(e), e)
	}
	return withDetail(codes.Internal, apperr.ErrInternal)
}

func withDetail(code codes.Code, e *apperr.Error) error {
	st := status.New(code, e.Message)
	st2, err := st.WithDetails(&commonv1.ErrorDetail{Type: e.Type, Code: e.Code, Message: e.Message, Param: e.Param})
	if err != nil {
		return st.Err()
	}
	return st2.Err()
}

// codeFor 依 (type, HTTP status) 決定 gRPC code（docs/03 §6.3）。
func codeFor(e *apperr.Error) codes.Code {
	switch e.HTTPStatus() {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return codes.InvalidArgument
	case http.StatusUnauthorized:
		return codes.Unauthenticated
	case http.StatusForbidden:
		return codes.PermissionDenied
	case http.StatusNotFound:
		return codes.NotFound
	case http.StatusConflict:
		if e.Type == apperr.TypeIdempotency || e.Code == "concurrent_modification" {
			return codes.Aborted
		}
		return codes.FailedPrecondition
	case http.StatusPaymentRequired, http.StatusBadGateway:
		return codes.FailedPrecondition
	case http.StatusTooManyRequests:
		return codes.ResourceExhausted
	case http.StatusServiceUnavailable:
		return codes.Unavailable
	case http.StatusGatewayTimeout:
		return codes.DeadlineExceeded
	case http.StatusGone:
		return codes.FailedPrecondition
	default:
		return codes.Internal
	}
}

// ErrorDetailFromStatus 從 gRPC status 取出 ErrorDetail（沒有時回 nil）。
func ErrorDetailFromStatus(st *status.Status) *commonv1.ErrorDetail {
	if st == nil {
		return nil
	}
	for _, d := range st.Details() {
		if ed, ok := d.(*commonv1.ErrorDetail); ok {
			return ed
		}
	}
	return nil
}

// ToAppError 把 gRPC error 還原成 *apperr.Error（有 ErrorDetail 時精確還原；否則依 gRPC code 對應）。
func ToAppError(err error) *apperr.Error {
	if err == nil {
		return nil
	}
	if e := apperr.From(err); e != nil {
		return e
	}
	st, ok := status.FromError(err)
	if !ok {
		if errors.Is(err, context.DeadlineExceeded) {
			return apperr.ErrTimeout.Wrap(err)
		}
		return apperr.ErrInternal.Wrap(err)
	}
	if ed := ErrorDetailFromStatus(st); ed != nil {
		return &apperr.Error{Type: ed.GetType(), Code: ed.GetCode(), Message: ed.GetMessage(), Param: ed.GetParam(), Err: err}
	}
	msg := st.Message()
	switch st.Code() {
	case codes.InvalidArgument:
		return apperr.ErrParameterInvalid.WithMessage("%s", msg).Wrap(err)
	case codes.NotFound:
		return apperr.ErrResourceMissing.WithMessage("%s", msg).Wrap(err)
	case codes.AlreadyExists:
		return apperr.New(apperr.TypeInvalidRequest, "invalid_state_transition", msg).Wrap(err)
	case codes.Aborted:
		return apperr.ErrIdempotencyMismatch.WithMessage("%s", msg).Wrap(err)
	case codes.FailedPrecondition:
		return apperr.New(apperr.TypeInvalidRequest, "invalid_state_transition", msg).Wrap(err)
	case codes.PermissionDenied:
		return apperr.New(apperr.TypeAuthentication, "insufficient_permissions", msg).Wrap(err)
	case codes.Unauthenticated:
		return apperr.New(apperr.TypeAuthentication, "invalid_api_key", msg).Wrap(err)
	case codes.ResourceExhausted:
		return apperr.ErrRateLimited.WithMessage("%s", msg).Wrap(err)
	case codes.Unavailable:
		return apperr.ErrServiceUnavailable.Wrap(err)
	case codes.DeadlineExceeded:
		return apperr.ErrTimeout.Wrap(err)
	case codes.Unimplemented:
		return apperr.New(apperr.TypeAPI, "not_implemented", "This operation is not implemented yet.").Wrap(err)
	case codes.OK, codes.Canceled, codes.Unknown, codes.Internal, codes.DataLoss, codes.OutOfRange:
		return apperr.ErrInternal.Wrap(err)
	default:
		return apperr.ErrInternal.Wrap(err)
	}
}
