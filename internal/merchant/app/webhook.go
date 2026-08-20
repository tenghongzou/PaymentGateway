package app

import (
	"context"
	"fmt"
	"slices"

	"github.com/tenghongzou/paymentgateway/internal/merchant/domain"
)

// CreateWebhookEndpointInput 對應 CreateWebhookEndpointRequest。
type CreateWebhookEndpointInput struct {
	MerchantID    string
	URL           string
	Description   string
	EnabledEvents []string
	Mode          domain.Mode
	Metadata      map[string]string
}

// WebhookEndpointView 為端點 + （視情境）解密後的 secret。
type WebhookEndpointView struct {
	Endpoint *domain.WebhookEndpoint
	// Secret 只在建立、輪替、include_secrets 時有值。
	Secret         string
	PreviousSecret string
}

// CreateWebhookEndpoint 建立端點；回傳的 Secret 僅此一次。
func (s *Service) CreateWebhookEndpoint(ctx context.Context, in CreateWebhookEndpointInput) (*WebhookEndpointView, error) {
	if _, err := domain.ParseMode(string(in.Mode)); err != nil {
		return nil, err
	}
	if err := s.urlPolicy.Validate(ctx, in.URL); err != nil {
		return nil, err
	}
	var out *WebhookEndpointView
	err := s.tx.WithinTx(ctx, func(ctx context.Context) error {
		m, err := s.lockMerchant(ctx, in.MerchantID)
		if err != nil {
			return err
		}
		if err := m.AssertWritable(); err != nil {
			return err
		}
		n, err := s.hooks.CountLive(ctx, m.ID)
		if err != nil {
			return fmt.Errorf("app: count endpoints: %w", err)
		}
		if n >= s.cfg.MaxWebhookEndpoints {
			return domain.ErrWebhookEndpointLimit.WithMessage("merchant already has %d webhook endpoints", n)
		}
		now := s.clock.Now()
		// 先產生 id 以便把它放進 AAD：NewWebhookEndpoint 會再產生 id，所以這裡先建立後再加密並回填。
		e, err := domain.NewWebhookEndpoint(domain.NewWebhookEndpointParams{
			MerchantID: m.ID, URL: in.URL, Description: in.Description, EnabledEvents: in.EnabledEvents,
			Mode: in.Mode, Metadata: in.Metadata, SecretEnc: "pending",
		}, now)
		if err != nil {
			return err
		}
		secret := domain.GenerateWebhookSecret()
		enc, err := s.cipher.Encrypt(secret, webhookAAD(e.ID, "secret_current"))
		if err != nil {
			return fmt.Errorf("app: encrypt webhook secret: %w", err)
		}
		e.SecretCurrentEnc = enc
		if err := s.hooks.Create(ctx, e); err != nil {
			return fmt.Errorf("app: create endpoint: %w", err)
		}
		out = &WebhookEndpointView{Endpoint: e, Secret: secret}
		return s.emit(ctx, AggregateWebhookEndpoint, EventWebhookEndpointCreated, m.PublicID(), webhookEndpointEventData(e))
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// 可更新欄位（proto UpdateWebhookEndpoint 註解）。
const (
	FieldURL           = "url"
	FieldDescription   = "description"
	FieldEnabledEvents = "enabled_events"
)

var updatableEndpointFields = []string{FieldURL, FieldDescription, FieldEnabledEvents, FieldStatus, FieldMetadata}

// WebhookEndpointPatch 為 update_mask 指定欄位的新值。
type WebhookEndpointPatch struct {
	URL           string
	Description   string
	EnabledEvents []string
	// Status 只允許 enabled / disabled。
	Status   domain.EndpointStatus
	Metadata map[string]string
}

// UpdateWebhookEndpointInput 對應 UpdateWebhookEndpointRequest。
type UpdateWebhookEndpointInput struct {
	MerchantID   string
	EndpointID   string
	Fields       []string
	Patch        WebhookEndpointPatch
	RotateSecret bool
}

// UpdateWebhookEndpoint 部分更新；RotateSecret=true 時輪替 secret（舊 secret 保留 24h）並回傳新 secret（僅此一次）。
func (s *Service) UpdateWebhookEndpoint(ctx context.Context, in UpdateWebhookEndpointInput) (*WebhookEndpointView, error) {
	if len(in.Fields) == 0 && !in.RotateSecret {
		return nil, domain.ErrParameterMissing.WithParam("update_mask").WithMessage("update_mask must not be empty unless rotate_secret is set")
	}
	for _, f := range in.Fields {
		if !slices.Contains(updatableEndpointFields, f) {
			return nil, domain.ErrParameterInvalid.WithParam("update_mask").WithMessage("field %q cannot be updated", f)
		}
	}
	mid, err := domain.ParseMerchantID(in.MerchantID)
	if err != nil {
		return nil, err
	}
	eid, err := domain.ParseWebhookEndpointID(in.EndpointID)
	if err != nil {
		return nil, err
	}
	if slices.Contains(in.Fields, FieldURL) {
		if err := s.urlPolicy.Validate(ctx, in.Patch.URL); err != nil {
			return nil, err
		}
	}
	var out *WebhookEndpointView
	err = s.tx.WithinTx(ctx, func(ctx context.Context) error {
		m, err := s.merchants.GetForUpdate(ctx, mid)
		if err != nil {
			return err
		}
		if err := m.AssertWritable(); err != nil {
			return err
		}
		e, err := s.hooks.Get(ctx, mid, eid)
		if err != nil {
			return err
		}
		if e.IsDeleted() {
			return domain.ErrNotFound.WithMessage("webhook endpoint %s is deleted", in.EndpointID)
		}
		now := s.clock.Now()
		for _, f := range in.Fields {
			if err := applyEndpointField(e, f, in.Patch); err != nil {
				return err
			}
		}
		view := &WebhookEndpointView{Endpoint: e}
		if in.RotateSecret {
			secret := domain.GenerateWebhookSecret()
			enc, err := s.cipher.Encrypt(secret, webhookAAD(e.ID, "secret_current"))
			if err != nil {
				return fmt.Errorf("app: encrypt webhook secret: %w", err)
			}
			// previous 的 AAD 是 secret_previous 欄位，需重新加密一次舊 secret。
			oldPlain, err := s.cipher.Decrypt(e.SecretCurrentEnc, webhookAAD(e.ID, "secret_current"))
			if err != nil {
				return domain.ErrSecretUnavailable.Wrap(err)
			}
			oldEnc, err := s.cipher.Encrypt(oldPlain, webhookAAD(e.ID, "secret_previous"))
			if err != nil {
				return fmt.Errorf("app: encrypt previous webhook secret: %w", err)
			}
			e.RotateSecret(enc, now)
			e.SecretPreviousEnc = oldEnc
			view.Secret = secret
			view.PreviousSecret = oldPlain
		} else {
			e.ExpirePreviousSecret(now)
		}
		e.UpdatedAt = now
		if err := s.hooks.Update(ctx, e); err != nil {
			return fmt.Errorf("app: update endpoint: %w", err)
		}
		out = view
		if in.RotateSecret {
			if err := s.emit(ctx, AggregateWebhookEndpoint, EventWebhookSecretRotated, in.MerchantID, webhookEndpointEventData(e)); err != nil {
				return err
			}
		}
		if len(in.Fields) > 0 || !in.RotateSecret {
			return s.emit(ctx, AggregateWebhookEndpoint, EventWebhookEndpointUpdated, in.MerchantID, webhookEndpointEventData(e))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func applyEndpointField(e *domain.WebhookEndpoint, field string, p WebhookEndpointPatch) error {
	switch field {
	case FieldURL:
		e.SetURL(p.URL)
		return nil
	case FieldDescription:
		return e.SetDescription(p.Description)
	case FieldEnabledEvents:
		return e.SetEnabledEvents(p.EnabledEvents)
	case FieldStatus:
		switch p.Status {
		case domain.EndpointEnabled:
			return e.Enable()
		case domain.EndpointDisabled:
			return e.Disable()
		default:
			return domain.ErrParameterInvalid.WithParam("status").WithMessage("status must be ENABLED or DISABLED")
		}
	case FieldMetadata:
		return e.SetMetadata(p.Metadata)
	default:
		return domain.ErrParameterInvalid.WithParam("update_mask").WithMessage("field %q cannot be updated", field)
	}
}

// DeleteWebhookEndpoint 軟刪除（冪等）。
func (s *Service) DeleteWebhookEndpoint(ctx context.Context, merchantID, endpointID string) error {
	mid, err := domain.ParseMerchantID(merchantID)
	if err != nil {
		return err
	}
	eid, err := domain.ParseWebhookEndpointID(endpointID)
	if err != nil {
		return err
	}
	return s.tx.WithinTx(ctx, func(ctx context.Context) error {
		e, err := s.hooks.Get(ctx, mid, eid)
		if err != nil {
			return err
		}
		now := s.clock.Now()
		if !e.MarkDeleted(now) {
			return nil
		}
		e.UpdatedAt = now
		if err := s.hooks.Update(ctx, e); err != nil {
			return fmt.Errorf("app: delete endpoint: %w", err)
		}
		return s.emit(ctx, AggregateWebhookEndpoint, EventWebhookEndpointDeleted, merchantID, webhookEndpointEventData(e))
	})
}

// ListWebhookEndpointsInput 對應 ListWebhookEndpointsRequest。
type ListWebhookEndpointsInput struct {
	MerchantID     string
	Mode           domain.Mode
	IncludeSecrets bool
	IncludeDeleted bool
	Page           Page
}

// ListWebhookEndpoints 列出端點；IncludeSecrets 時解密 current / previous（previous 只在輪替視窗內）。
// TODO：IncludeSecrets 應限 webhook-service（mTLS 身分 + authz interceptor），Phase 0 尚無 mTLS，先以 log 記錄。
func (s *Service) ListWebhookEndpoints(ctx context.Context, in ListWebhookEndpointsInput) ([]WebhookEndpointView, string, error) {
	mid, err := domain.ParseMerchantID(in.MerchantID)
	if err != nil {
		return nil, "", err
	}
	if in.Mode != "" {
		if _, err := domain.ParseMode(string(in.Mode)); err != nil {
			return nil, "", err
		}
	}
	items, next, err := s.hooks.List(ctx, mid, WebhookEndpointFilter{Mode: in.Mode, IncludeDeleted: in.IncludeDeleted}, in.Page.Normalize())
	if err != nil {
		return nil, "", err
	}
	now := s.clock.Now()
	views := make([]WebhookEndpointView, 0, len(items))
	for _, e := range items {
		v := WebhookEndpointView{Endpoint: e}
		if in.IncludeSecrets && !e.IsDeleted() {
			v.Secret, err = s.cipher.Decrypt(e.SecretCurrentEnc, webhookAAD(e.ID, "secret_current"))
			if err != nil {
				return nil, "", domain.ErrSecretUnavailable.Wrap(fmt.Errorf("endpoint %s: %w", e.PublicID(), err))
			}
			if e.PreviousSecretValid(now) {
				v.PreviousSecret, err = s.cipher.Decrypt(e.SecretPreviousEnc, webhookAAD(e.ID, "secret_previous"))
				if err != nil {
					s.log.WarnContext(ctx, "previous webhook secret undecryptable", "endpoint_id", e.PublicID(), "err", err)
					v.PreviousSecret = ""
				}
			}
		}
		views = append(views, v)
	}
	if in.IncludeSecrets {
		s.log.InfoContext(ctx, "webhook secrets disclosed via ListWebhookEndpoints", "merchant_id", in.MerchantID, "count", len(views))
	}
	return views, next, nil
}
