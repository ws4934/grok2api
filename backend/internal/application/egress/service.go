package egress

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

var (
	ErrInvalidInput            = errors.New("代理节点参数无效")
	ErrInvalidFilter           = errors.New("出口代理筛选条件无效")
	ErrInvalidSort             = errors.New("代理节点排序条件无效")
	ErrNotFound                = errors.New("代理节点不存在")
	ErrProbeStale              = errors.New("代理配置在探测期间已更新，请重新测试")
	ErrQualityProbeUnavailable = errors.New("出口质量探测不可用")
	ErrQualityProbeNoAccount   = errors.New("质量检测暂无可调度账号")
	ErrQualityLeaseUnavailable = errors.New("租约级质量隔离不可用")
	ErrQualityLeaseConflict    = errors.New("租约状态已变化")
	ErrClearanceUnavailable    = errors.New("Clearance 刷新不可用")
	ErrProxyProfileUnavailable = errors.New("共享代理配置功能不可用")
	ErrProxyProfileInUse       = errors.New("共享代理配置仍被节点使用")
	ErrProxyProfileNotFound    = errors.New("共享代理配置不存在")
)

type Input struct {
	Name              string
	Scope             domain.Scope
	Enabled           bool
	ProxyPool         *bool
	AccountCapacity   *int
	ProxyURL          *string
	ProxyProfileID    *uint64
	ClearProxyURL     bool
	UserAgent         string
	CloudflareCookies *string
	ClearCookies      bool
}

type ListFilter struct {
	Scope       domain.Scope
	Enabled     string
	ProbeStatus string
	Assignment  string
	Sort        repository.SortQuery
}

type ServiceRepository interface {
	repository.EgressRepository
	repository.EgressNodePageRepository
}

type Service struct {
	repository                  ServiceRepository
	proxyProfiles               repository.EgressProxyProfileRepository
	accounts                    AccountBindingRepository
	qualityLeases               QualityLeaseRepository
	operations                  OperationsRepository
	cipher                      *security.Cipher
	mu                          sync.RWMutex
	browserUA                   string
	clearance                   ClearanceManager
	prober                      NodeProber
	operationsCache             OperationsConfigInvalidator
	qualityProber               QualityProber
	assignmentMu                sync.Mutex
	lastAssignmentRun           time.Time
	assignmentRunning           bool
	autoAssignMaxNodeShare      float64
	autoAssignMaxMigrationShare float64
}

type ClearanceManager interface {
	RefreshClearance(context.Context, uint64) error
	ForgetClearance(uint64)
}

type BatchClearanceManager interface {
	ForgetClearances([]uint64)
}

func NewService(storage ServiceRepository, cipher *security.Cipher, browserUA string, accounts ...AccountBindingRepository) *Service {
	service := &Service{repository: storage, cipher: cipher, browserUA: strings.TrimSpace(browserUA)}
	if profiles, ok := storage.(repository.EgressProxyProfileRepository); ok {
		service.proxyProfiles = profiles
	}
	if operations, ok := storage.(OperationsRepository); ok {
		service.operations = operations
	}
	if len(accounts) > 0 {
		service.accounts = accounts[0]
		if leases, ok := accounts[0].(QualityLeaseRepository); ok {
			service.qualityLeases = leases
		}
	}
	return service
}

func (s *Service) UpdateDefaults(browserUA string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.browserUA = strings.TrimSpace(browserUA)
}

func (s *Service) SetClearanceManager(value ClearanceManager) {
	s.mu.Lock()
	s.clearance = value
	s.mu.Unlock()
}

func (s *Service) DefaultUserAgents() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return map[string]string{
		string(domain.ScopeBuild): "", string(domain.ScopeWeb): s.browserUA, string(domain.ScopeConsole): s.browserUA,
		string(domain.ScopeWebAsset): s.browserUA, string(domain.ScopeConsoleAsset): s.browserUA,
	}
}

func (s *Service) List(ctx context.Context, page, pageSize int, search string, filter ListFilter) ([]domain.PublicNode, int64, error) {
	page, pageSize = repository.NormalizePage(page, pageSize, repository.DefaultPageSize)
	if !validListScope(filter.Scope) || !validListValue(filter.Enabled, "enabled", "disabled") ||
		!validListValue(filter.ProbeStatus, string(domain.ProbeStatusHealthy), string(domain.ProbeStatusUnhealthy), string(domain.ProbeStatusUnknown)) ||
		!validListValue(filter.Assignment, "bound", "unbound") {
		return nil, 0, ErrInvalidFilter
	}
	if !repository.IsValidSort(filter.Sort, "name", "scope", "proxy", "clearance", "health") {
		return nil, 0, ErrInvalidSort
	}
	var enabled *bool
	if filter.Enabled != "" {
		value := filter.Enabled == "enabled"
		enabled = &value
	}
	values, total, err := s.repository.ListEgressNodePage(ctx, repository.EgressNodeListQuery{
		Page: repository.PageQuery{Offset: (page - 1) * pageSize, Limit: pageSize, Search: strings.TrimSpace(search), Sort: filter.Sort},
		Filter: repository.EgressNodeListFilter{
			Scope: filter.Scope, Enabled: enabled, ProbeStatus: domain.ProbeStatus(filter.ProbeStatus), Assignment: filter.Assignment,
		},
	})
	if err != nil {
		return nil, 0, err
	}
	return s.publicNodes(values), total, nil
}

func (s *Service) ListAll(ctx context.Context, scope domain.Scope, sort repository.SortQuery) ([]domain.PublicNode, error) {
	if !validListScope(scope) {
		return nil, ErrInvalidFilter
	}
	if !repository.IsValidSort(sort, "name", "scope", "proxy", "clearance", "health") {
		return nil, ErrInvalidSort
	}
	values, err := s.repository.ListEgressNodes(ctx, scope, sort)
	if err != nil {
		return nil, err
	}
	return s.publicNodes(values), nil
}

func (s *Service) publicNodes(values []domain.Node) []domain.PublicNode {
	result := make([]domain.PublicNode, 0, len(values))
	for _, value := range values {
		result = append(result, s.publicNode(value))
	}
	return result
}

func validListScope(scope domain.Scope) bool {
	if domain.IsExtensionScope(scope) {
		return true
	}
	return scope == "" || scope == domain.ScopeBuild || scope == domain.ScopeWeb || scope == domain.ScopeConsole || scope == domain.ScopeWebAsset || scope == domain.ScopeConsoleAsset
}

func allServiceScopes() []domain.Scope {
	return []domain.Scope{domain.ScopeBuild, domain.ScopeWeb, domain.ScopeConsole, domain.ScopeWebAsset, domain.ScopeConsoleAsset}
}

func validListValue(value string, allowed ...string) bool {
	if value == "" {
		return true
	}
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func (s *Service) Create(ctx context.Context, input Input) (domain.PublicNode, error) {
	var err error
	input, err = s.resolveProxyProfile(ctx, 0, input)
	if err != nil {
		return domain.PublicNode{}, err
	}
	value, err := s.applyInput(domain.Node{}, input, true)
	if err != nil {
		return domain.PublicNode{}, err
	}
	created, err := s.repository.CreateEgressNode(ctx, value)
	if errors.Is(err, repository.ErrEgressProxyProfileNotFound) {
		return domain.PublicNode{}, ErrProxyProfileNotFound
	}
	if err == nil {
		s.forgetClearance(created.ID)
	}
	return s.publicNode(created), err
}

func (s *Service) Update(ctx context.Context, id uint64, input Input) (domain.PublicNode, error) {
	value, err := s.repository.GetEgressNode(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return domain.PublicNode{}, ErrNotFound
	}
	if err != nil {
		return domain.PublicNode{}, err
	}
	input, err = s.resolveProxyProfile(ctx, value.ProxyProfileID, input)
	if err != nil {
		return domain.PublicNode{}, err
	}
	previousScope := value.Scope
	previousProxyURL := value.EncryptedProxyURL
	value, err = s.applyInput(value, input, false)
	if err != nil {
		return domain.PublicNode{}, err
	}
	if err := s.validateFallbackNodeUpdate(ctx, value); err != nil {
		return domain.PublicNode{}, err
	}
	if previousScope != value.Scope {
		if err := s.validateNodeBindingScope(ctx, value.ID, value.Scope); err != nil {
			return domain.PublicNode{}, err
		}
	}
	updated, err := s.repository.UpdateEgressNode(ctx, value)
	if errors.Is(err, repository.ErrEgressProxyProfileNotFound) {
		return domain.PublicNode{}, ErrProxyProfileNotFound
	}
	if err == nil {
		s.forgetClearance(updated.ID)
		if !updated.Enabled || previousScope != updated.Scope || previousProxyURL != updated.EncryptedProxyURL {
			if clearErr := s.clearQualityLeasesForNodes(ctx, []uint64{updated.ID}); clearErr != nil {
				return s.publicNode(updated), clearErr
			}
		}
	}
	return s.publicNode(updated), err
}

// ProxyURL returns one administrator-selected secret without placing it in
// ordinary list/detail payloads. HTTP handlers must mark the response no-store.
func (s *Service) ProxyURL(ctx context.Context, id uint64) (string, error) {
	value, err := s.repository.GetEgressNode(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(value.EncryptedProxyURL) == "" {
		return "", fmt.Errorf("%w: 节点未配置代理地址", ErrInvalidInput)
	}
	proxyURL, err := s.cipher.Decrypt(value.EncryptedProxyURL)
	if err != nil {
		return "", err
	}
	return NormalizeProxyURL(proxyURL)
}

func (s *Service) validateFallbackNodeUpdate(ctx context.Context, node domain.Node) error {
	if s.operations == nil {
		return nil
	}
	config, err := s.operations.GetEgressOperationsConfig(ctx)
	if err != nil {
		return err
	}
	return s.validateFallbackNodeUpdateWithConfig(node, config)
}

func (s *Service) validateFallbackNodeUpdateWithConfig(node domain.Node, config domain.OperationsConfig) error {
	for _, scope := range allServiceScopes() {
		fallback := config.FallbackFor(scope)
		if fallback.Mode != domain.FallbackModeFixed || fallback.NodeID != node.ID {
			continue
		}
		if err := s.validateFixedFallbackNode(scope, node, false); err != nil {
			return fmt.Errorf("节点已配置为 %s 固定回退，无法应用当前修改: %w", scope, err)
		}
	}
	return nil
}

func (s *Service) Delete(ctx context.Context, id uint64) error {
	err := s.repository.DeleteEgressNode(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return ErrNotFound
	}
	if err == nil {
		s.forgetClearance(id)
		s.invalidateOperationsConfig()
	}
	return err
}

func (s *Service) RefreshClearance(ctx context.Context, id uint64) error {
	if _, err := s.repository.GetEgressNode(ctx, id); errors.Is(err, repository.ErrNotFound) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	s.mu.RLock()
	manager := s.clearance
	s.mu.RUnlock()
	if manager == nil {
		return ErrClearanceUnavailable
	}
	return manager.RefreshClearance(ctx, id)
}

func uniqueIDs(values []uint64) []uint64 {
	seen := make(map[uint64]struct{}, len(values))
	result := make([]uint64, 0, len(values))
	for _, value := range values {
		if value == 0 {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func (s *Service) forgetClearance(id uint64) {
	s.mu.RLock()
	manager := s.clearance
	s.mu.RUnlock()
	if manager != nil {
		manager.ForgetClearance(id)
	}
}

func (s *Service) forgetClearances(ids []uint64) {
	s.mu.RLock()
	manager := s.clearance
	s.mu.RUnlock()
	if manager == nil {
		return
	}
	if batch, ok := manager.(BatchClearanceManager); ok {
		batch.ForgetClearances(ids)
		return
	}
	for _, id := range ids {
		manager.ForgetClearance(id)
	}
}

func (s *Service) applyInput(value domain.Node, input Input, create bool) (domain.Node, error) {
	proxyPool := value.ProxyPool
	if input.ProxyPool != nil {
		proxyPool = *input.ProxyPool
	}
	profileChanged := input.ProxyProfileID != nil && value.ProxyProfileID != *input.ProxyProfileID
	configurationChanged := create || value.Scope != input.Scope || value.ProxyPool != proxyPool || (!value.Enabled && input.Enabled) || input.ClearProxyURL || input.ProxyURL != nil || profileChanged
	name := strings.TrimSpace(input.Name)
	if name == "" || len(name) > 160 {
		return domain.Node{}, fmt.Errorf("%w: 名称必须在 1 到 160 个字符之间", ErrInvalidInput)
	}
	if !validListScope(input.Scope) || input.Scope == "" {
		return domain.Node{}, fmt.Errorf("%w: scope 必须是 grok_build、grok_web、grok_console、grok_web_asset 或 grok_console_asset", ErrInvalidInput)
	}
	value.Name, value.Scope, value.Enabled, value.ProxyPool = name, input.Scope, input.Enabled, proxyPool
	if input.AccountCapacity != nil {
		if *input.AccountCapacity < 0 || *input.AccountCapacity > 100000 {
			return domain.Node{}, fmt.Errorf("%w: 每个代理的账号容量必须在 0 到 100000 之间", ErrInvalidInput)
		}
		value.AccountCapacity = *input.AccountCapacity
	}
	if input.Scope == domain.ScopeBuild {
		// Build 请求始终沿用 Provider 生成的 CLI User-Agent，出口节点不得覆盖协议身份。
		value.UserAgent = ""
	} else {
		value.UserAgent = strings.TrimSpace(input.UserAgent)
	}
	if input.Scope != domain.ScopeBuild && value.UserAgent == "" {
		s.mu.RLock()
		value.UserAgent = s.browserUA
		s.mu.RUnlock()
	}
	if len(value.UserAgent) > 512 {
		return domain.Node{}, fmt.Errorf("%w: User-Agent 过长", ErrInvalidInput)
	}
	if input.ProxyProfileID != nil {
		if value.SourceID != 0 && *input.ProxyProfileID != 0 {
			return domain.Node{}, fmt.Errorf("%w: 订阅管理的节点不能绑定共享代理配置", ErrInvalidInput)
		}
		value.ProxyProfileID = *input.ProxyProfileID
	} else if input.ClearProxyURL || input.ProxyURL != nil {
		value.ProxyProfileID = 0
	}
	if input.ClearProxyURL {
		value.EncryptedProxyURL = ""
		value.ProxyPool = false
	} else if input.ProxyURL != nil {
		normalized, err := NormalizeProxyURL(*input.ProxyURL)
		if err != nil {
			return domain.Node{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
		}
		if normalized != "" {
			value.EncryptedProxyURL, err = s.cipher.Encrypt(normalized)
			if err != nil {
				return domain.Node{}, err
			}
		}
	}
	if value.ProxyPool && strings.TrimSpace(value.EncryptedProxyURL) == "" {
		return domain.Node{}, fmt.Errorf("%w: 代理池模式需要配置代理地址", ErrInvalidInput)
	}
	if input.Scope == domain.ScopeBuild || input.Scope == domain.ScopeConsoleAsset {
		value.EncryptedCloudflareCookie = ""
	} else if input.ClearCookies {
		value.EncryptedCloudflareCookie = ""
	} else if input.CloudflareCookies != nil {
		if len(*input.CloudflareCookies) > maxCloudflareCookieBytes {
			return domain.Node{}, fmt.Errorf("%w: Cloudflare Cookie 不能超过 16 KiB", ErrInvalidInput)
		}
		cookies := SanitizeCloudflareCookies(*input.CloudflareCookies)
		if cookies != "" || create {
			var err error
			value.EncryptedCloudflareCookie, err = s.cipher.Encrypt(cookies)
			if err != nil {
				return domain.Node{}, err
			}
		}
	}
	if configurationChanged {
		value.Health = 1
		value.FailureCount = 0
		value.CooldownUntil = nil
		value.LastError = ""
		value.ProbeStatus = domain.ProbeStatusUnknown
		value.LastProbedAt = nil
		value.ProbeLatencyMS = 0
		value.ExitIP = ""
		value.ProbeError = ""
		value.ProbeProvider = ""
		value.IPv4Probe = domain.ProbeFamilyResult{Status: domain.ProbeStatusUnknown}
		value.IPv6Probe = domain.ProbeFamilyResult{Status: domain.ProbeStatusUnknown}
	}
	// Any administrator edit invalidates freshness. Keep the binding fingerprint:
	// managed mode may use the existing cookie as last-known-good only when the
	// target and actual proxy still match the binding that produced it.
	value.ClearanceRefreshedAt = nil
	value.ClearanceFingerprint = ""
	return value, nil
}

func (s *Service) publicNode(value domain.Node) domain.PublicNode {
	userAgent := value.UserAgent
	if value.Scope == domain.ScopeBuild {
		userAgent = ""
	}
	proxyDisplay, proxyFingerprint, accountBoundProxy := s.proxyMetadata(value.EncryptedProxyURL)
	proxyPool := value.ProxyPool || accountBoundProxy
	health, failureCount, cooldownUntil, lastError := value.Health, value.FailureCount, value.CooldownUntil, value.LastError
	if proxyPool {
		health, failureCount, cooldownUntil, lastError = 1, 0, nil, ""
	}
	return domain.PublicNode{
		ID: value.ID, Name: value.Name, Scope: value.Scope, Enabled: value.Enabled,
		ProxyConfigured: value.EncryptedProxyURL != "", ProxyDisplay: proxyDisplay, ProxyFingerprint: proxyFingerprint,
		UserAgent: userAgent, CookieConfigured: value.EncryptedCloudflareCookie != "",
		ProxyPool:         proxyPool,
		SourceID:          value.SourceID,
		AccountCapacity:   value.AccountCapacity,
		ProxyProfileID:    value.ProxyProfileID,
		ProxyProfileName:  value.ProxyProfileName,
		AccountBoundProxy: accountBoundProxy,
		Health:            health, FailureCount: failureCount, CooldownUntil: cooldownUntil, LastError: lastError,
		ProbeStatus: value.ProbeStatus, LastProbedAt: value.LastProbedAt, ProbeLatencyMS: value.ProbeLatencyMS, ExitIP: value.ExitIP, ProbeError: value.ProbeError,
		ProbeProvider: value.ProbeProvider,
		IPv4Probe:     value.IPv4Probe, IPv6Probe: value.IPv6Probe,
		AssignedAccountCount: value.AssignedAccountCount,
		CreatedAt:            value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}
}
