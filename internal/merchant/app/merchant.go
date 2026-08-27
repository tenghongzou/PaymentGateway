package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/tenghongzou/paymentgateway/internal/merchant/domain"
)

// CreateMerchantInput 對應 CreateMerchantRequest。
type CreateMerchantInput struct {
	Name                string
	LegalName           string
	Country             string
	DefaultCurrency     string
	ContactEmail        string
	StatementDescriptor string
	ExternalRef         string
	Metadata            map[string]string
}

// CreateMerchant 建立商戶（狀態 active）並發佈 merchant.created。
func (s *Service) CreateMerchant(ctx context.Context, in CreateMerchantInput) (*domain.Merchant, error) {
	m, err := domain.NewMerchant(domain.NewMerchantParams{
		Name: in.Name, LegalName: in.LegalName, Country: in.Country, DefaultCurrency: in.DefaultCurrency,
		ContactEmail: in.ContactEmail, StatementDescriptor: in.StatementDescriptor, ExternalRef: in.ExternalRef, Metadata: in.Metadata,
	}, s.clock.Now())
	if err != nil {
		return nil, err
	}
	txErr := s.tx.WithinTx(ctx, func(ctx context.Context) error {
		if ref := strings.TrimSpace(in.ExternalRef); ref != "" {
			// TODO：DB 無 external_ref 專欄 / UNIQUE，目前為應用層檢查（併發下可能重複）。
			existing, err := s.merchants.FindByExternalRef(ctx, ref)
			switch {
			case err == nil && existing != nil:
				return domain.ErrAlreadyExists.WithParam("external_ref").WithMessage("external_ref %q is already used by %s", ref, existing.PublicID())
			case err != nil && !errors.Is(err, domain.ErrNotFound):
				return fmt.Errorf("app: find by external_ref: %w", err)
			}
		}
		if err := s.merchants.Create(ctx, m); err != nil {
			return fmt.Errorf("app: create merchant: %w", err)
		}
		return s.emit(ctx, AggregateMerchant, EventMerchantCreated, m.PublicID(), merchantEventData(m))
	})
	if txErr != nil {
		return nil, txErr
	}
	return m, nil
}

// GetMerchant 依 public id 取得商戶。
func (s *Service) GetMerchant(ctx context.Context, merchantID string) (*domain.Merchant, error) {
	return s.loadMerchant(ctx, merchantID)
}

// 可更新欄位（proto UpdateMerchant 註解）。
const (
	FieldName                = "name"
	FieldLegalName           = "legal_name"
	FieldContactEmail        = "contact_email"
	FieldStatus              = "status"
	FieldDefaultCurrency     = "default_currency"
	FieldStatementDescriptor = "default_statement_descriptor"
	FieldMetadata            = "metadata"
)

var updatableMerchantFields = []string{FieldName, FieldLegalName, FieldContactEmail, FieldStatus, FieldDefaultCurrency, FieldStatementDescriptor, FieldMetadata}

// MerchantPatch 為 update_mask 指定欄位的新值。
type MerchantPatch struct {
	Name                string
	LegalName           string
	ContactEmail        string
	Status              domain.Status
	DefaultCurrency     string
	StatementDescriptor string
	Metadata            map[string]string
}

// UpdateMerchantInput 對應 UpdateMerchantRequest（Fields = update_mask.paths）。
type UpdateMerchantInput struct {
	MerchantID string
	Fields     []string
	Patch      MerchantPatch
}

// UpdateMerchant 部分更新商戶；mask 含不可更新欄位回 parameter_invalid；狀態轉移依狀態機。
func (s *Service) UpdateMerchant(ctx context.Context, in UpdateMerchantInput) (*domain.Merchant, error) {
	if len(in.Fields) == 0 {
		return nil, domain.ErrParameterMissing.WithParam("update_mask").WithMessage("update_mask must not be empty")
	}
	for _, f := range in.Fields {
		if !slices.Contains(updatableMerchantFields, f) {
			return nil, domain.ErrParameterInvalid.WithParam("update_mask").WithMessage("field %q cannot be updated", f)
		}
	}
	var out *domain.Merchant
	err := s.tx.WithinTx(ctx, func(ctx context.Context) error {
		m, err := s.lockMerchant(ctx, in.MerchantID)
		if err != nil {
			return err
		}
		for _, f := range in.Fields {
			if err := applyMerchantField(m, f, in.Patch); err != nil {
				return err
			}
		}
		m.UpdatedAt = s.clock.Now()
		if err := s.merchants.Update(ctx, m); err != nil {
			return fmt.Errorf("app: update merchant: %w", err)
		}
		out = m
		return s.emit(ctx, AggregateMerchant, EventMerchantUpdated, m.PublicID(), merchantEventData(m))
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func applyMerchantField(m *domain.Merchant, field string, p MerchantPatch) error {
	switch field {
	case FieldName:
		return m.Rename(p.Name)
	case FieldLegalName:
		return m.SetLegalName(p.LegalName)
	case FieldContactEmail:
		return m.SetContactEmail(p.ContactEmail)
	case FieldStatus:
		if p.Status == "" {
			return domain.ErrParameterMissing.WithParam("status")
		}
		// 只允許 active / suspended / closed（pending 不可由 API 設定）。
		if p.Status == domain.StatusPending {
			return domain.ErrParameterInvalid.WithParam("status").WithMessage("status pending cannot be set via API")
		}
		return m.Transition(p.Status)
	case FieldDefaultCurrency:
		return m.SetDefaultCurrency(p.DefaultCurrency)
	case FieldStatementDescriptor:
		return m.SetStatementDescriptor(p.StatementDescriptor)
	case FieldMetadata:
		return m.SetMetadata(p.Metadata)
	default:
		return domain.ErrParameterInvalid.WithParam("update_mask").WithMessage("field %q cannot be updated", field)
	}
}

// ListMerchantsInput 對應 ListMerchantsRequest。
type ListMerchantsInput struct {
	Status  domain.Status
	Country string
	Page    Page
}

// ListMerchants 列出商戶（後台用）。
func (s *Service) ListMerchants(ctx context.Context, in ListMerchantsInput) ([]*domain.Merchant, string, error) {
	if in.Country != "" {
		if err := domain.ValidateCountry(in.Country); err != nil {
			return nil, "", err
		}
		in.Country = strings.ToUpper(in.Country)
	}
	return s.merchants.List(ctx, MerchantFilter{Status: in.Status, Country: in.Country}, in.Page.Normalize())
}
