package egress

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	application "github.com/chenyme/grok2api/backend/internal/application/egress"
	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/pkg/flight"
)

// acquireUnavailableFallback keeps the primary selection error when fallback
// is disabled. A fixed fallback is never silently replaced with direct traffic
// when it is invalid or unavailable.
func (m *Manager) acquireUnavailableFallback(ctx context.Context, scope domain.Scope, affinity string, allowDirect bool, encryptedCredentialCookies string, managedClearance bool, primaryErr error) (*Lease, bool, error) {
	lease, configured, applied, err := m.acquireFallback(ctx, scope, affinity, allowDirect, encryptedCredentialCookies, managedClearance, primaryErr)
	if err != nil {
		return nil, configured, err
	}
	if applied {
		return lease, configured, nil
	}
	return nil, true, primaryErr
}

// acquireFallback is called before an upstream request is written. Transport
// failures are intentionally not retried through this path because replaying
// a non-idempotent upstream request on a different IP can duplicate work.
func (m *Manager) acquireFallback(ctx context.Context, scope domain.Scope, affinity string, allowDirect bool, encryptedCredentialCookies string, managedClearance bool, primaryErr error) (*Lease, bool, bool, error) {
	fallback, supported, err := m.fallbackFor(ctx, scope, time.Now().UTC())
	return m.applyFallback(ctx, scope, affinity, allowDirect, encryptedCredentialCookies, managedClearance, primaryErr, fallback, supported, err)
}

func (m *Manager) applyFallback(ctx context.Context, scope domain.Scope, affinity string, allowDirect bool, encryptedCredentialCookies string, managedClearance bool, primaryErr error, fallback domain.FallbackConfig, supported bool, configErr error) (*Lease, bool, bool, error) {
	if configErr != nil {
		return nil, false, false, fallbackError(primaryErr, fmt.Errorf("读取出口回退配置: %w", configErr))
	}
	if !supported {
		return nil, false, false, nil
	}
	switch fallback.Mode {
	case domain.FallbackModeNone:
		return nil, false, false, nil
	case domain.FallbackModeDirect:
		if !allowDirect {
			recordSelection(ctx, Selection{NodeName: "direct", Scope: scope})
			return nil, false, true, nil
		}
		lease, _, err := m.leaseForNode(ctx, scope, affinity, encryptedCredentialCookies, managedClearance, domain.Node{ID: 0, Name: "direct", Scope: scope, Enabled: true, Health: 1})
		if err != nil {
			return nil, false, false, fallbackError(primaryErr, fmt.Errorf("获取本地直连回退: %w", err))
		}
		return lease, false, true, nil
	case domain.FallbackModeFixed:
		selected, err := m.fixedFallbackNode(ctx, scope, fallback.NodeID)
		if err != nil {
			return nil, false, false, fallbackError(primaryErr, err)
		}
		lease, _, err := m.leaseForNode(ctx, scope, affinity, encryptedCredentialCookies, managedClearance, selected)
		if err != nil {
			return nil, false, false, fallbackError(primaryErr, fmt.Errorf("获取固定回退节点 %d: %w", selected.ID, err))
		}
		return lease, true, true, nil
	default:
		return nil, false, false, fallbackError(primaryErr, fmt.Errorf("出口回退模式 %q 无效", fallback.Mode))
	}
}

func (m *Manager) fallbackFor(ctx context.Context, scope domain.Scope, now time.Time) (domain.FallbackConfig, bool, error) {
	config, supported, err := m.loadOperationsConfig(ctx, now)
	if err != nil || !supported {
		return domain.FallbackConfig{Mode: domain.FallbackModeNone}, supported, err
	}
	return config.FallbackFor(scope), true, nil
}

func (m *Manager) loadOperationsConfig(ctx context.Context, now time.Time) (domain.OperationsConfig, bool, error) {
	configRepository, ok := m.repository.(operationsConfigRepository)
	if !ok {
		return domain.OperationsConfig{}, false, nil
	}
	m.operationsMu.RLock()
	cached := m.operationsConfig
	m.operationsMu.RUnlock()
	if !cached.expiresAt.IsZero() && now.Before(cached.expiresAt) {
		return cached.value, true, nil
	}
	loaded, err := flight.Do(ctx, &m.operationsConfigLoad, "operations", snapshotLoadTimeout, func(ctx context.Context) (domain.OperationsConfig, error) {
		checkTime := time.Now().UTC()
		m.operationsMu.RLock()
		cached := m.operationsConfig
		m.operationsMu.RUnlock()
		if !cached.expiresAt.IsZero() && checkTime.Before(cached.expiresAt) {
			return cached.value, nil
		}
		m.operationsMu.RLock()
		version := m.operationsConfigVer
		m.operationsMu.RUnlock()
		value, err := configRepository.GetEgressOperationsConfig(ctx)
		if err != nil {
			return domain.OperationsConfig{}, err
		}
		m.operationsMu.Lock()
		if version == m.operationsConfigVer {
			m.operationsConfig = cachedOperationsConfig{value: value, expiresAt: checkTime.Add(operationsConfigSnapshotTTL)}
		}
		m.operationsMu.Unlock()
		return value, nil
	})
	if err != nil {
		return domain.OperationsConfig{}, true, err
	}
	return loaded, true, nil
}

func (m *Manager) fixedFallbackNode(ctx context.Context, scope domain.Scope, nodeID uint64) (domain.Node, error) {
	if nodeID == 0 {
		return domain.Node{}, errors.New("固定回退节点未指定")
	}
	selected, err := m.repository.GetEgressNode(ctx, nodeID)
	if err != nil {
		return domain.Node{}, fmt.Errorf("读取固定回退节点 %d: %w", nodeID, err)
	}
	if !domain.SupportsScope(selected.Scope, scope) {
		return domain.Node{}, fmt.Errorf("固定回退节点 %d 与 %s 作用域不兼容", nodeID, scope)
	}
	if !selected.Enabled {
		return domain.Node{}, fmt.Errorf("固定回退节点 %d 已禁用", nodeID)
	}
	if selected.ProxyPool {
		return domain.Node{}, fmt.Errorf("固定回退节点 %d 使用代理池模式", nodeID)
	}
	if strings.TrimSpace(selected.EncryptedProxyURL) == "" {
		return domain.Node{}, fmt.Errorf("固定回退节点 %d 未配置代理地址", nodeID)
	}
	if selected.CooldownUntil != nil && time.Now().UTC().Before(*selected.CooldownUntil) {
		return domain.Node{}, fmt.Errorf("固定回退节点 %d 正在冷却", nodeID)
	}
	proxyURL, err := m.cipher.Decrypt(selected.EncryptedProxyURL)
	if err != nil {
		return domain.Node{}, fmt.Errorf("读取固定回退节点 %d 代理配置: %w", nodeID, err)
	}
	proxyURL, err = application.NormalizeProxyURL(proxyURL)
	if err != nil || proxyURL == "" {
		return domain.Node{}, fmt.Errorf("固定回退节点 %d 代理地址无效", nodeID)
	}
	if strings.Contains(proxyURL, application.ProxyAccountPlaceholder) {
		return domain.Node{}, fmt.Errorf("固定回退节点 %d 使用账号代理模板", nodeID)
	}
	return selected, nil
}

func fallbackError(primaryErr, fallbackErr error) error {
	if primaryErr == nil {
		return fallbackErr
	}
	return fmt.Errorf("%w；出口回退不可用: %v", primaryErr, fallbackErr)
}

func (m *Manager) InvalidateOperationsConfig() {
	m.operationsMu.Lock()
	m.operationsConfig = cachedOperationsConfig{}
	m.operationsConfigVer++
	m.operationsMu.Unlock()
}
