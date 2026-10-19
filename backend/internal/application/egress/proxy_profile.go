package egress

import (
	"context"
	"errors"
	"fmt"
	"strings"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type ProxyProfileInput struct {
	Name     string
	ProxyURL *string
}

func (s *Service) ListProxyProfiles(ctx context.Context, page, pageSize int, search string) ([]domain.PublicProxyProfile, int64, error) {
	if s.proxyProfiles == nil {
		return nil, 0, ErrProxyProfileUnavailable
	}
	page, pageSize = repository.NormalizePage(page, pageSize, repository.DefaultPageSize)
	values, total, err := s.proxyProfiles.ListEgressProxyProfiles(ctx, repository.PageQuery{
		Offset: (page - 1) * pageSize,
		Limit:  pageSize,
		Search: strings.TrimSpace(search),
	})
	if err != nil {
		return nil, 0, err
	}
	result := make([]domain.PublicProxyProfile, 0, len(values))
	for _, value := range values {
		result = append(result, s.publicProxyProfile(value))
	}
	return result, total, nil
}

func (s *Service) CreateProxyProfile(ctx context.Context, input ProxyProfileInput) (domain.PublicProxyProfile, error) {
	if s.proxyProfiles == nil {
		return domain.PublicProxyProfile{}, ErrProxyProfileUnavailable
	}
	name, proxyURL, err := validateProxyProfileInput(input, true)
	if err != nil {
		return domain.PublicProxyProfile{}, err
	}
	encrypted, err := s.cipher.Encrypt(proxyURL)
	if err != nil {
		return domain.PublicProxyProfile{}, err
	}
	created, err := s.proxyProfiles.CreateEgressProxyProfile(ctx, domain.ProxyProfile{Name: name, EncryptedProxyURL: encrypted})
	return s.publicProxyProfile(created), err
}

func (s *Service) GetProxyProfile(ctx context.Context, id uint64) (domain.PublicProxyProfile, error) {
	if s.proxyProfiles == nil {
		return domain.PublicProxyProfile{}, ErrProxyProfileUnavailable
	}
	value, err := s.proxyProfiles.GetEgressProxyProfile(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return domain.PublicProxyProfile{}, ErrProxyProfileNotFound
	}
	if err != nil {
		return domain.PublicProxyProfile{}, err
	}
	return s.publicProxyProfile(value), nil
}

func (s *Service) UpdateProxyProfile(ctx context.Context, id uint64, input ProxyProfileInput) (domain.PublicProxyProfile, error) {
	if s.proxyProfiles == nil {
		return domain.PublicProxyProfile{}, ErrProxyProfileUnavailable
	}
	current, err := s.proxyProfiles.GetEgressProxyProfile(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return domain.PublicProxyProfile{}, ErrProxyProfileNotFound
	}
	if err != nil {
		return domain.PublicProxyProfile{}, err
	}
	name, proxyURL, err := validateProxyProfileInput(input, false)
	if err != nil {
		return domain.PublicProxyProfile{}, err
	}
	current.Name = name
	proxyChanged := false
	if input.ProxyURL != nil {
		previous, decryptErr := s.cipher.Decrypt(current.EncryptedProxyURL)
		if decryptErr != nil {
			return domain.PublicProxyProfile{}, decryptErr
		}
		previous, decryptErr = NormalizeProxyURL(previous)
		if decryptErr != nil {
			return domain.PublicProxyProfile{}, decryptErr
		}
		proxyChanged = previous != proxyURL
		if proxyChanged {
			current.EncryptedProxyURL, err = s.cipher.Encrypt(proxyURL)
			if err != nil {
				return domain.PublicProxyProfile{}, err
			}
		}
	}
	updated, nodeIDs, err := s.proxyProfiles.UpdateEgressProxyProfile(ctx, current, proxyChanged)
	if errors.Is(err, repository.ErrNotFound) {
		return domain.PublicProxyProfile{}, ErrProxyProfileNotFound
	}
	if err != nil {
		return domain.PublicProxyProfile{}, err
	}
	if proxyChanged {
		s.forgetClearances(nodeIDs)
		if err := s.clearQualityLeasesForNodes(ctx, nodeIDs); err != nil {
			return s.publicProxyProfile(updated), err
		}
	}
	return s.publicProxyProfile(updated), nil
}

func (s *Service) DeleteProxyProfile(ctx context.Context, id uint64) error {
	if s.proxyProfiles == nil {
		return ErrProxyProfileUnavailable
	}
	err := s.proxyProfiles.DeleteEgressProxyProfile(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return ErrProxyProfileNotFound
	}
	if errors.Is(err, repository.ErrEgressProxyProfileInUse) || errors.Is(err, repository.ErrConflict) {
		return ErrProxyProfileInUse
	}
	return err
}

func (s *Service) ProxyProfileURL(ctx context.Context, id uint64) (string, error) {
	if s.proxyProfiles == nil {
		return "", ErrProxyProfileUnavailable
	}
	value, err := s.proxyProfiles.GetEgressProxyProfile(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return "", ErrProxyProfileNotFound
	}
	if err != nil {
		return "", err
	}
	proxyURL, err := s.cipher.Decrypt(value.EncryptedProxyURL)
	if err != nil {
		return "", err
	}
	return NormalizeProxyURL(proxyURL)
}

func validateProxyProfileInput(input ProxyProfileInput, create bool) (string, string, error) {
	name := strings.TrimSpace(input.Name)
	if name == "" || len(name) > 160 {
		return "", "", fmt.Errorf("%w: 共享代理配置名称必须在 1 到 160 个字符之间", ErrInvalidInput)
	}
	if input.ProxyURL == nil {
		if create {
			return "", "", fmt.Errorf("%w: 代理地址必填", ErrInvalidInput)
		}
		return name, "", nil
	}
	proxyURL, err := NormalizeProxyURL(*input.ProxyURL)
	if err != nil || proxyURL == "" {
		if err == nil {
			err = errors.New("代理地址不能为空")
		}
		return "", "", fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	return name, proxyURL, nil
}

func (s *Service) publicProxyProfile(value domain.ProxyProfile) domain.PublicProxyProfile {
	result := domain.PublicProxyProfile{
		ID: value.ID, Name: value.Name, BoundNodeCount: value.BoundNodeCount,
		CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}
	proxyURL, err := s.cipher.Decrypt(value.EncryptedProxyURL)
	if err != nil {
		return result
	}
	proxyURL, err = NormalizeProxyURL(proxyURL)
	if err != nil || proxyURL == "" {
		return result
	}
	result.ProxyDisplay = ProxyDisplay(proxyURL)
	result.ProxyFingerprint = security.HashToken(proxyURL)[:12]
	return result
}

func (s *Service) resolveProxyProfile(ctx context.Context, currentProfileID uint64, input Input) (Input, error) {
	if input.ProxyProfileID == nil {
		return input, nil
	}
	profileID := *input.ProxyProfileID
	if profileID == 0 {
		if input.ClearProxyURL {
			return Input{}, fmt.Errorf("%w: 取消共享代理配置与清除代理不能同时操作", ErrInvalidInput)
		}
		return input, nil
	}
	if input.ProxyURL != nil || input.ClearProxyURL {
		return Input{}, fmt.Errorf("%w: 使用共享代理配置时不能同时修改或清除代理地址", ErrInvalidInput)
	}
	if profileID == currentProfileID {
		return input, nil
	}
	if s.proxyProfiles == nil {
		return Input{}, ErrProxyProfileUnavailable
	}
	profile, err := s.proxyProfiles.GetEgressProxyProfile(ctx, profileID)
	if errors.Is(err, repository.ErrNotFound) {
		return Input{}, fmt.Errorf("%w: 共享代理配置不存在", ErrInvalidInput)
	}
	if err != nil {
		return Input{}, err
	}
	proxyURL, err := s.cipher.Decrypt(profile.EncryptedProxyURL)
	if err != nil {
		return Input{}, err
	}
	proxyURL, err = NormalizeProxyURL(proxyURL)
	if err != nil || proxyURL == "" {
		return Input{}, fmt.Errorf("%w: 共享代理配置的代理地址无效", ErrInvalidInput)
	}
	input.ProxyURL = &proxyURL
	return input, nil
}
