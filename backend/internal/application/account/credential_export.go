package account

import (
	"context"
	"fmt"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type ExportResult struct {
	Data  []byte
	Count int
}

type ExportPageResult struct {
	ExportResult
	NextID        uint64
	SnapshotMaxID uint64
	HasMore       bool
}

// ExportProviderCredentials 导出可由对应 Provider 导入接口重新读取的凭据文档。
func (s *Service) ExportProviderCredentials(ctx context.Context, providerValue accountdomain.Provider) (ExportResult, error) {
	return s.exportProviderCredentials(ctx, providerValue, repository.AccountListQuery{
		Page:   repository.PageQuery{Limit: maxCredentialExportAccounts + 1},
		Filter: repository.AccountListFilter{Provider: string(providerValue), Now: s.now()},
	}, true, 0)
}

// ExportProviderCredentialsCursor exports a stable provider batch bounded by
// the maximum account ID captured by the first request.
func (s *Service) ExportProviderCredentialsCursor(ctx context.Context, providerValue accountdomain.Provider, afterID, snapshotMaxID uint64, limit int) (ExportPageResult, error) {
	if limit < 1 || limit > maxCredentialExportAccounts {
		return ExportPageResult{}, invalidInput("单批导出数量必须在 1 到 10000 之间")
	}
	if afterID > 0 && snapshotMaxID == 0 {
		return ExportPageResult{}, invalidInput("继续导出时必须提供快照上界")
	}
	if snapshotMaxID > 0 && afterID > snapshotMaxID {
		return ExportPageResult{}, invalidInput("导出游标不能超过快照上界")
	}
	if !providerValue.IsValid() {
		return ExportPageResult{}, invalidInput("账号来源无效")
	}
	if snapshotMaxID == 0 {
		values, _, err := s.accounts.List(ctx, repository.AccountListQuery{
			Page:   repository.PageQuery{Limit: 1, Sort: repository.SortQuery{Field: "id", Direction: repository.SortDescending}},
			Filter: repository.AccountListFilter{Provider: string(providerValue), Now: s.now()},
		})
		if err != nil {
			return ExportPageResult{}, err
		}
		if len(values) == 0 {
			result, exportErr := s.marshalProviderCredentials(providerValue, nil)
			return ExportPageResult{ExportResult: result}, exportErr
		}
		snapshotMaxID = values[0].ID
	}
	values, total, err := s.accounts.List(ctx, repository.AccountListQuery{
		Page: repository.PageQuery{Limit: limit, Sort: repository.SortQuery{Field: "id", Direction: repository.SortAscending}},
		Filter: repository.AccountListFilter{
			Provider: string(providerValue), AfterID: afterID, ThroughID: snapshotMaxID, Now: s.now(),
		},
	})
	if err != nil {
		return ExportPageResult{}, err
	}
	result, err := s.marshalProviderCredentials(providerValue, values)
	if err != nil {
		return ExportPageResult{}, err
	}
	nextID := afterID
	if len(values) > 0 {
		nextID = values[len(values)-1].ID
	}
	return ExportPageResult{
		ExportResult: result, NextID: nextID, SnapshotMaxID: snapshotMaxID, HasMore: total > int64(len(values)),
	}, nil
}

// ExportProviderCredentialsByIDs 只导出管理端明确选择且属于指定 Provider 的账号。
func (s *Service) ExportProviderCredentialsByIDs(ctx context.Context, providerValue accountdomain.Provider, ids []uint64) (ExportResult, error) {
	values, err := normalizeIDs(ids, maxCredentialExportAccounts)
	if err != nil {
		return ExportResult{}, err
	}
	return s.exportProviderCredentials(ctx, providerValue, repository.AccountListQuery{
		Page: repository.PageQuery{Limit: len(values)},
		Filter: repository.AccountListFilter{
			Provider: string(providerValue), AccountIDs: values, RestrictIDs: true, Now: s.now(),
		},
	}, false, len(values))
}

func (s *Service) exportProviderCredentials(ctx context.Context, providerValue accountdomain.Provider, query repository.AccountListQuery, enforceTotalLimit bool, expectedCount int) (ExportResult, error) {
	if !providerValue.IsValid() {
		return ExportResult{}, invalidInput("账号来源无效")
	}
	values, total, err := s.accounts.List(ctx, query)
	if err != nil {
		return ExportResult{}, err
	}
	if enforceTotalLimit && total > maxCredentialExportAccounts {
		return ExportResult{}, fmt.Errorf("%w: 单次最多导出 10000 个账号", ErrExportLimit)
	}
	if err := validateCredentialExportCount(expectedCount, total, len(values)); err != nil {
		return ExportResult{}, err
	}
	return s.marshalProviderCredentials(providerValue, values)
}

func validateCredentialExportCount(expected int, total int64, actual int) error {
	if expected > 0 && (total != int64(expected) || actual != expected) {
		return invalidInput("所选账号包含不存在或不属于当前号池的账号")
	}
	return nil
}

func (s *Service) marshalProviderCredentials(providerValue accountdomain.Provider, values []accountdomain.Credential) (ExportResult, error) {
	if !providerValue.IsValid() {
		return ExportResult{}, invalidInput("账号来源无效")
	}
	if s.providers == nil {
		return ExportResult{}, fmt.Errorf("Provider 注册表未初始化")
	}
	adapter, ok := s.providers.CredentialCodec(providerValue)
	if !ok {
		return ExportResult{}, fmt.Errorf("Provider %s 不支持凭据导出", providerValue)
	}
	var err error
	seeds := make([]provider.CredentialSeed, 0, len(values))
	for _, value := range values {
		if value.Provider != providerValue {
			continue
		}
		accessToken := ""
		if value.EncryptedAccessToken != "" {
			accessToken, err = s.cipher.Decrypt(value.EncryptedAccessToken)
			if err != nil {
				return ExportResult{}, fmt.Errorf("解密账号 %d access token: %w", value.ID, err)
			}
		}
		refreshToken := ""
		if value.EncryptedRefreshToken != "" {
			refreshToken, err = s.cipher.Decrypt(value.EncryptedRefreshToken)
			if err != nil {
				return ExportResult{}, fmt.Errorf("解密账号 %d refresh token: %w", value.ID, err)
			}
		}
		cloudflareCookies := ""
		if value.EncryptedCloudflareCookie != "" {
			cloudflareCookies, err = s.cipher.Decrypt(value.EncryptedCloudflareCookie)
			if err != nil {
				return ExportResult{}, fmt.Errorf("解密账号 %d Cloudflare Cookie: %w", value.ID, err)
			}
		}
		if accessToken == "" && refreshToken == "" {
			return ExportResult{}, fmt.Errorf("账号 %d 没有可导出的凭据", value.ID)
		}
		state, err := s.decryptCredentialState(value.EncryptedState)
		if err != nil {
			return ExportResult{}, fmt.Errorf("解密账号 %d 凭据状态: %w", value.ID, err)
		}
		seeds = append(seeds, provider.CredentialSeed{
			Provider: value.Provider, AuthType: value.AuthType, WebTier: value.WebTier,
			Name: value.Name, Email: value.Email, UserID: value.UserID, TeamID: value.TeamID,
			OIDCClientID: value.OIDCClientID, AccessToken: accessToken, RefreshToken: refreshToken,
			CloudflareCookies: cloudflareCookies, ExpiresAt: value.ExpiresAt, Plan: value.Plan, State: state,
			WebNSFWEnabledAt: value.WebNSFWEnabledAt, WebTermsAcceptedAt: value.WebTermsAcceptedAt,
			WebTermsAcceptedVersion: value.WebTermsAcceptedVersion, WebBirthDateSetAt: value.WebBirthDateSetAt,
		})
	}
	data, err := adapter.MarshalCredentials(seeds)
	if err != nil {
		return ExportResult{}, err
	}
	return ExportResult{Data: data, Count: len(seeds)}, nil
}
