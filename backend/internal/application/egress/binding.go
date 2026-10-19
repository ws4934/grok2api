package egress

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// AccountBindingRepository is intentionally narrow so existing account
// repository consumers do not gain egress concerns.
type AccountBindingRepository interface {
	CountProviderAccountsByIDs(context.Context, accountdomain.Provider, []uint64) (int64, error)
	UpdateEgressBindings(context.Context, accountdomain.Provider, []uint64, *uint64, accountdomain.EgressAssignmentMode, time.Time) (int64, error)
	ListEgressAssignments(context.Context, accountdomain.Provider) ([]accountdomain.Credential, error)
	ListEgressBindingProviders(context.Context, uint64) ([]accountdomain.Provider, error)
	ListEgressSourceBindingProviders(context.Context, uint64) ([]accountdomain.Provider, error)
}

type AssignmentResult struct {
	Assigned int
}

// AssignAccounts creates explicit many-to-one account bindings. A binding is
// not a proxy-pool preference: runtime requests must use the selected node.
func (s *Service) AssignAccounts(ctx context.Context, nodeID uint64, provider accountdomain.Provider, accountIDs []uint64, mode accountdomain.EgressAssignmentMode) (AssignmentResult, error) {
	if s.accounts == nil {
		return AssignmentResult{}, errors.New("账号出口绑定不可用")
	}
	if nodeID == 0 || !provider.IsValid() || !mode.IsValid() || len(accountIDs) == 0 {
		return AssignmentResult{}, fmt.Errorf("%w: 账号出口绑定参数无效", ErrInvalidInput)
	}
	node, err := s.repository.GetEgressNode(ctx, nodeID)
	if errors.Is(err, repository.ErrNotFound) {
		return AssignmentResult{}, ErrNotFound
	}
	if err != nil {
		return AssignmentResult{}, err
	}
	if !node.Enabled || strings.TrimSpace(node.EncryptedProxyURL) == "" {
		return AssignmentResult{}, fmt.Errorf("%w: 只能绑定启用且已配置代理地址的节点", ErrInvalidInput)
	}
	if !scopeSupportsProvider(node.Scope, provider) {
		return AssignmentResult{}, fmt.Errorf("%w: 代理节点作用域与账号来源不兼容", ErrInvalidInput)
	}
	unique := uniqueIDs(accountIDs)
	count, err := s.accounts.CountProviderAccountsByIDs(ctx, provider, unique)
	if err != nil {
		return AssignmentResult{}, err
	}
	if count != int64(len(unique)) {
		return AssignmentResult{}, fmt.Errorf("%w: 包含不属于当前账号池的账号", ErrInvalidInput)
	}
	assigned, err := s.accounts.UpdateEgressBindings(ctx, provider, unique, &nodeID, mode, time.Now().UTC())
	if err != nil {
		return AssignmentResult{}, err
	}
	return AssignmentResult{Assigned: int(assigned)}, nil
}

// UnassignAccounts removes an explicit binding and restores scope pool routing.
func (s *Service) UnassignAccounts(ctx context.Context, provider accountdomain.Provider, accountIDs []uint64) (AssignmentResult, error) {
	if s.accounts == nil {
		return AssignmentResult{}, errors.New("账号出口绑定不可用")
	}
	if !provider.IsValid() || len(accountIDs) == 0 {
		return AssignmentResult{}, fmt.Errorf("%w: 账号出口解绑参数无效", ErrInvalidInput)
	}
	unique := uniqueIDs(accountIDs)
	count, err := s.accounts.CountProviderAccountsByIDs(ctx, provider, unique)
	if err != nil {
		return AssignmentResult{}, err
	}
	if count != int64(len(unique)) {
		return AssignmentResult{}, fmt.Errorf("%w: 包含不属于当前账号池的账号", ErrInvalidInput)
	}
	updated, err := s.accounts.UpdateEgressBindings(ctx, provider, unique, nil, "", time.Time{})
	if err != nil {
		return AssignmentResult{}, err
	}
	return AssignmentResult{Assigned: int(updated)}, nil
}

func scopeSupportsProvider(scope domain.Scope, provider accountdomain.Provider) bool {
	switch provider {
	case accountdomain.ProviderBuild:
		return scope == domain.ScopeBuild
	case accountdomain.ProviderWeb:
		return scope == domain.ScopeWeb
	case accountdomain.ProviderConsole:
		return domain.SupportsScope(scope, domain.ScopeConsole)
	default:
		return domain.IsExtensionScope(scope) && string(scope) == string(provider)
	}
}

func (s *Service) validateNodeBindingScope(ctx context.Context, nodeID uint64, scope domain.Scope) error {
	if s.accounts == nil {
		return nil
	}
	providers, err := s.accounts.ListEgressBindingProviders(ctx, nodeID)
	if err != nil {
		return err
	}
	return validateBindingProviders(scope, providers)
}

func (s *Service) validateSourceBindingScope(ctx context.Context, sourceID uint64, scope domain.Scope) error {
	if s.accounts == nil {
		return nil
	}
	providers, err := s.accounts.ListEgressSourceBindingProviders(ctx, sourceID)
	if err != nil {
		return err
	}
	return validateBindingProviders(scope, providers)
}

func validateBindingProviders(scope domain.Scope, providers []accountdomain.Provider) error {
	for _, provider := range providers {
		if !scopeSupportsProvider(scope, provider) {
			return fmt.Errorf("%w: 当前节点仍绑定 %s 账号，不能改为 %s 作用域", ErrInvalidInput, provider, scope)
		}
	}
	return nil
}
