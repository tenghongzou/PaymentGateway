// Package domain 為 reconciliation-service 的領域層：結算檔、結算列、讀模型、比對器、對帳執行與差異。
//
// 只能 import 標準庫與 pkg/（docs/01 §7 import 規則）。
package domain

import (
	"errors"
	"fmt"

	"github.com/tenghongzou/paymentgateway/pkg/apperr"
)

// 領域錯誤（type / code 依 docs/03 §7）。
var (
	// ErrUnknownFormat 表示不支援的結算檔格式 / provider 組合。
	ErrUnknownFormat = apperr.New(apperr.TypeInvalidRequest, "parameter_invalid", "Unsupported settlement file format.").WithParam("file_format")
	// ErrEmptyFile 表示結算檔沒有任何資料列。
	ErrEmptyFile = apperr.New(apperr.TypeInvalidRequest, "parameter_invalid", "Settlement file contains no data rows.").WithParam("file")
	// ErrParse 表示結算檔解析失敗（訊息含列號與欄位）。
	ErrParse = apperr.New(apperr.TypeInvalidRequest, "parameter_invalid", "Settlement file could not be parsed.").WithParam("file")
	// ErrInvalidPeriod 表示 run 的期間不合法（period_end 必須晚於 period_start）。
	ErrInvalidPeriod = apperr.New(apperr.TypeInvalidRequest, "parameter_invalid", "settlement period_end must be after period_start.").WithParam("settlement_date")
	// ErrInvalidTransition 表示差異 / run 的狀態轉移不合法。
	ErrInvalidTransition = apperr.New(apperr.TypeInvalidRequest, "invalid_state_transition", "The resource is not in a state that allows this operation.")
	// ErrResolutionNoteRequired 表示忽略差異時必須填寫備註。
	ErrResolutionNoteRequired = apperr.New(apperr.TypeInvalidRequest, "parameter_missing", "resolution note is required.").WithParam("note")
	// ErrResolvedByRequired 表示處理差異時必須填寫處理者。
	ErrResolvedByRequired = apperr.New(apperr.TypeInvalidRequest, "parameter_missing", "resolved_by is required.").WithParam("resolved_by")
	// ErrRunNotFound 表示查無 run。
	ErrRunNotFound = apperr.New(apperr.TypeInvalidRequest, "resource_missing", "No such reconciliation run.").WithParam("run_id")
	// ErrDiscrepancyNotFound 表示查無差異。
	ErrDiscrepancyNotFound = apperr.New(apperr.TypeInvalidRequest, "resource_missing", "No such discrepancy.").WithParam("discrepancy_id")
	// ErrFileNotFound 表示查無結算檔。
	ErrFileNotFound = apperr.New(apperr.TypeInvalidRequest, "resource_missing", "No such settlement file.").WithParam("file_id")
	// ErrDuplicateFile 表示同一 file_hash 已存在（呼叫端應改為讀取既有檔案）。
	ErrDuplicateFile = errors.New("reconciliation: settlement file already exists")
	// ErrConcurrentModification 為樂觀鎖衝突。
	ErrConcurrentModification = apperr.ErrConcurrentModify
)

// ParseError 為結算檔某一列的解析錯誤；Line 為資料列號（1 起算，不含表頭），Field 為欄位名。
type ParseError struct {
	Line  int
	Field string
	Err   error
}

// Error 實作 error。
func (e *ParseError) Error() string {
	if e.Field != "" {
		return fmt.Sprintf("line %d field %q: %v", e.Line, e.Field, e.Err)
	}
	return fmt.Sprintf("line %d: %v", e.Line, e.Err)
}

// Unwrap 讓 errors.Is 能比對底層原因。
func (e *ParseError) Unwrap() error { return e.Err }

// newParseError 建立 ParseError。
func newParseError(line int, field string, format string, args ...any) *ParseError {
	return &ParseError{Line: line, Field: field, Err: fmt.Errorf(format, args...)}
}

// WrapParse 把解析錯誤包成對外的 ErrParse（保留原因供 errors.As 取出 *ParseError）。
func WrapParse(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrEmptyFile) {
		return err
	}
	return ErrParse.WithMessage("Settlement file could not be parsed: %v", err).Wrap(err)
}
