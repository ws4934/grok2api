package egress

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"strings"
	"sync"
	"time"

	application "github.com/chenyme/grok2api/backend/internal/application/egress"
	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

// SetFailureProber enables an immediate, deduplicated connectivity probe after
// a fixed proxy reports a transport failure. The callback persists the probe
// result; it must not depend on the failed request context.
func (m *Manager) SetFailureProber(value FailureProber) {
	m.failureProbeMu.Lock()
	m.failureProber = value
	if value == nil {
		clear(m.failureProbes)
	}
	m.failureProbeMu.Unlock()
}

func (m *Manager) scheduleFailureProbe(node domain.Node) {
	m.failureProbeMu.Lock()
	prober := m.failureProber
	state := m.failureProbes[node.ID]
	if prober == nil || state.running {
		m.failureProbeMu.Unlock()
		return
	}
	state.running = true
	state.done = make(chan struct{})
	m.failureProbes[node.ID] = state
	done := state.done
	m.failureProbeMu.Unlock()

	m.log().Info("egress_failure_probe_scheduled", "node_id", node.ID, "node_name", node.Name, "scope", node.Scope)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), failureProbeTimeout)
		result, err := prober(ctx, node.ID)
		cancel()

		m.failureProbeMu.Lock()
		state := m.failureProbes[node.ID]
		if state.done == done {
			state.running = false
			state.lastCompleted = time.Now().UTC()
			m.failureProbes[node.ID] = state
		}
		close(done)
		m.failureProbeMu.Unlock()

		if err != nil {
			m.log().Warn("egress_failure_probe_failed", "node_id", node.ID, "node_name", node.Name, "scope", node.Scope, "error", err)
			return
		}
		if result.Status == domain.ProbeStatusHealthy {
			m.invalidateNodes(node.Scope)
		}
		m.log().Info("egress_failure_probe_completed", "node_id", node.ID, "node_name", node.Name, "scope", node.Scope, "probe_status", result.Status, "latency_ms", result.LatencyMS)
	}()
}

func (m *Manager) waitForFailureProbe(ctx context.Context, nodeID uint64) (bool, error) {
	now := time.Now().UTC()
	m.failureProbeMu.Lock()
	state, exists := m.failureProbes[nodeID]
	m.failureProbeMu.Unlock()
	if !exists {
		return false, nil
	}
	if !state.running {
		return !state.lastCompleted.IsZero() && now.Sub(state.lastCompleted) < failureProbeCompletionGrace, nil
	}
	if state.done == nil {
		return false, nil
	}
	timer := time.NewTimer(failureProbeWaitTimeout)
	defer timer.Stop()
	select {
	case <-state.done:
		return true, nil
	case <-ctx.Done():
		return false, ctx.Err()
	case <-timer.C:
		return false, nil
	}
}

type preparedEgressProbe struct {
	nodeID    uint64
	nodeName  string
	nodeScope domain.Scope
	proxyURL  string
}

// ProbeEgressNode verifies IPv4 and IPv6 independently through fixed provider
// endpoints. Both requests share one immutable node snapshot so a concurrent
// administrator edit cannot mix results from different proxy configurations.
func (m *Manager) ProbeEgressNode(ctx context.Context, node domain.Node) (domain.ProbeResult, error) {
	type outcome struct {
		family string
		result domain.ProbeFamilyResult
		err    error
	}
	startedAt := time.Now().UTC()
	var provider domain.ProbeProvider
	config, supported, configErr := m.loadOperationsConfig(ctx, time.Now().UTC())
	if configErr != nil {
		message := "读取代理探测服务配置失败"
		result := failedEgressProbeResult(provider, message, startedAt)
		m.logProbeSetupFailure(ctx, node, provider, "load_probe_config", message, configErr, result.LatencyMS)
		return result, configErr
	}
	if supported {
		provider = config.ProbeProvider.Normalized()
	} else {
		provider = domain.ProbeProviderCloudflare
	}
	target, stage, message, prepareErr := m.prepareEgressProbe(node)
	if prepareErr != nil {
		result := failedEgressProbeResult(provider, message, startedAt)
		m.logProbeSetupFailure(ctx, node, provider, stage, message, prepareErr, result.LatencyMS)
		return result, prepareErr
	}
	ipv4Endpoint, ipv6Endpoint := probeEndpoints(provider)
	outcomes := make(chan outcome, 2)
	for _, probe := range []struct{ family, endpoint string }{{"ipv4", ipv4Endpoint}, {"ipv6", ipv6Endpoint}} {
		go func() {
			result, err := m.probeEgressEndpoint(ctx, target, provider, probe.family, probe.endpoint)
			outcomes <- outcome{family: probe.family, result: result, err: err}
		}()
	}
	var ipv4Err, ipv6Err error
	result := domain.ProbeResult{Status: domain.ProbeStatusUnhealthy, Provider: provider}
	for range 2 {
		current := <-outcomes
		if current.family == "ipv4" {
			result.IPv4, ipv4Err = current.result, current.err
		} else {
			result.IPv6, ipv6Err = current.result, current.err
		}
	}
	result.TestedAt = time.Now().UTC()
	result.LatencyMS = max(result.IPv4.LatencyMS, result.IPv6.LatencyMS)
	if result.IPv4.Status == domain.ProbeStatusHealthy {
		result.Status, result.ExitIP = domain.ProbeStatusHealthy, result.IPv4.ExitIP
	} else if result.IPv6.Status == domain.ProbeStatusHealthy {
		result.Status, result.ExitIP = domain.ProbeStatusHealthy, result.IPv6.ExitIP
	}
	if result.Status == domain.ProbeStatusHealthy {
		return result, nil
	}
	errorsByFamily := make([]string, 0, 2)
	if result.IPv4.Error != "" {
		errorsByFamily = append(errorsByFamily, "IPv4: "+result.IPv4.Error)
	}
	if result.IPv6.Error != "" {
		errorsByFamily = append(errorsByFamily, "IPv6: "+result.IPv6.Error)
	}
	result.Error = strings.Join(errorsByFamily, "; ")
	if result.Error == "" {
		result.Error = "IPv4 和 IPv6 代理探测均失败"
	}
	return result, errors.Join(ipv4Err, ipv6Err, errors.New(result.Error))
}

func (m *Manager) prepareEgressProbe(node domain.Node) (preparedEgressProbe, string, string, error) {
	target := preparedEgressProbe{nodeID: node.ID, nodeName: node.Name, nodeScope: node.Scope}
	if node.ID == 0 {
		message := "代理节点 ID 无效"
		return target, "validate_node", message, errors.New(message)
	}
	proxyURL, err := m.cipher.Decrypt(node.EncryptedProxyURL)
	if err != nil {
		return target, "decrypt_proxy", "读取代理配置失败", err
	}
	proxyURL, err = application.NormalizeProxyURL(proxyURL)
	if err != nil {
		return target, "normalize_proxy", "代理地址无效", err
	}
	if proxyURL == "" {
		message := "未配置代理地址"
		return target, "normalize_proxy", message, errors.New(message)
	}
	if strings.Contains(proxyURL, application.ProxyAccountPlaceholder) {
		proxyURL, err = renderAccountProxyURL(proxyURL, "egress_probe")
		if err != nil {
			return target, "render_proxy_identity", "账号代理模板无效", err
		}
	}
	target.proxyURL = proxyURL
	return target, "", "", nil
}

func failedEgressProbeResult(provider domain.ProbeProvider, message string, startedAt time.Time) domain.ProbeResult {
	completedAt := time.Now().UTC()
	latencyMS := max(1, int(completedAt.Sub(startedAt).Milliseconds()))
	family := domain.ProbeFamilyResult{
		Status: domain.ProbeStatusUnhealthy, TestedAt: completedAt, LatencyMS: latencyMS, Error: message,
	}
	return domain.ProbeResult{
		Status: domain.ProbeStatusUnhealthy, TestedAt: completedAt, LatencyMS: latencyMS, Error: message,
		Provider: provider, IPv4: family, IPv6: family,
	}
}

func (m *Manager) logProbeSetupFailure(ctx context.Context, node domain.Node, provider domain.ProbeProvider, stage, message string, err error, durationMS int) {
	attributes := []any{
		"node_id", node.ID, "node_name", node.Name, "scope", node.Scope,
		"probe_provider", provider, "address_family", "all", "endpoint", "",
		"stage", stage, "status_code", 0, "duration_ms", durationMS,
		"connect_ms", 0, "tls_ms", 0, "first_byte_ms", 0,
		"probe_error", message,
	}
	if err != nil {
		attributes = append(attributes, "error", sanitizeFlareSolverrMessage(err.Error()))
	}
	m.log().WarnContext(ctx, "egress_probe_failed", attributes...)
}

func probeEndpoints(provider domain.ProbeProvider) (string, string) {
	if provider.Normalized() == domain.ProbeProviderCloudflare {
		return cloudflareIPv4ProbeEndpoint, cloudflareIPv6ProbeEndpoint
	}
	return egressIPv4ProbeEndpoint, egressIPv6ProbeEndpoint
}

func (m *Manager) probeEgressEndpoint(ctx context.Context, target preparedEgressProbe, provider domain.ProbeProvider, family, targetURL string) (result domain.ProbeFamilyResult, probeErr error) {
	startedAt := time.Now().UTC()
	result = domain.ProbeFamilyResult{Status: domain.ProbeStatusUnhealthy, TestedAt: startedAt}
	stage := "create_client"
	statusCode := 0
	var traceMu sync.Mutex
	var connectStartedAt, tlsStartedAt time.Time
	connectDone, connectFailed := false, false
	tlsDone, tlsFailed := false, false
	gotFirstByte := false
	connectMS, tlsMS, firstByteMS := 0, 0, 0
	defer func() {
		completedAt := time.Now().UTC()
		durationMS := max(1, int(completedAt.Sub(startedAt).Milliseconds()))
		result.TestedAt = completedAt
		if result.LatencyMS == 0 {
			result.LatencyMS = durationMS
		}
		traceMu.Lock()
		if !connectStartedAt.IsZero() && connectMS == 0 {
			connectMS = max(1, int(time.Since(connectStartedAt).Milliseconds()))
		}
		if !tlsStartedAt.IsZero() && tlsMS == 0 {
			tlsMS = max(1, int(time.Since(tlsStartedAt).Milliseconds()))
		}
		if stage == "first_byte" && firstByteMS == 0 {
			firstByteMS = durationMS
		}
		connectDurationMS, tlsDurationMS, firstByteDurationMS := connectMS, tlsMS, firstByteMS
		traceMu.Unlock()
		attributes := []any{
			"node_id", target.nodeID,
			"node_name", target.nodeName,
			"scope", target.nodeScope,
			"probe_provider", provider,
			"address_family", family,
			"endpoint", targetURL,
			"stage", stage,
			"status_code", statusCode,
			"duration_ms", durationMS,
			"connect_ms", connectDurationMS,
			"tls_ms", tlsDurationMS,
			"first_byte_ms", firstByteDurationMS,
		}
		if probeErr != nil || result.Status != domain.ProbeStatusHealthy {
			attributes = append(attributes, "probe_error", result.Error)
			if probeErr != nil {
				attributes = append(attributes, "error", sanitizeFlareSolverrMessage(probeErr.Error()))
			}
			m.log().WarnContext(ctx, "egress_probe_failed", attributes...)
			return
		}
		attributes = append(attributes, "latency_ms", result.LatencyMS, "exit_ip", result.ExitIP)
		m.log().InfoContext(ctx, "egress_probe_succeeded", attributes...)
	}()
	clientFactory := m.newBuildClient
	if clientFactory == nil {
		clientFactory = newBuildRequestClient
	}
	client, err := clientFactory(target.proxyURL, egressProbeTimeout)
	if err != nil {
		result.Error = "创建代理连接失败"
		return result, err
	}
	defer client.CloseIdleConnections()
	probeCtx, cancel := context.WithTimeout(ctx, egressProbeTimeout)
	defer cancel()
	probeCtx = httptrace.WithClientTrace(probeCtx, &httptrace.ClientTrace{
		ConnectStart: func(_, _ string) {
			traceMu.Lock()
			if connectStartedAt.IsZero() {
				connectStartedAt = time.Now()
			}
			traceMu.Unlock()
		},
		ConnectDone: func(_, _ string, traceErr error) {
			traceMu.Lock()
			connectDone = true
			connectFailed = traceErr != nil
			if !connectStartedAt.IsZero() && connectMS == 0 {
				connectMS = max(1, int(time.Since(connectStartedAt).Milliseconds()))
			}
			traceMu.Unlock()
		},
		TLSHandshakeStart: func() {
			traceMu.Lock()
			if tlsStartedAt.IsZero() {
				tlsStartedAt = time.Now()
			}
			traceMu.Unlock()
		},
		TLSHandshakeDone: func(_ tls.ConnectionState, traceErr error) {
			traceMu.Lock()
			tlsDone = true
			tlsFailed = traceErr != nil
			if !tlsStartedAt.IsZero() && tlsMS == 0 {
				tlsMS = max(1, int(time.Since(tlsStartedAt).Milliseconds()))
			}
			traceMu.Unlock()
		},
		GotFirstResponseByte: func() {
			traceMu.Lock()
			gotFirstByte = true
			if firstByteMS == 0 {
				firstByteMS = max(1, int(time.Since(startedAt).Milliseconds()))
			}
			traceMu.Unlock()
		},
	})
	stage = "build_request"
	request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, targetURL, nil)
	if err != nil {
		result.Error = "构造探测请求失败"
		return result, err
	}
	request.Header.Set("User-Agent", DefaultUserAgent)
	stage = "execute_request"
	response, err := client.Do(request)
	if err != nil {
		traceMu.Lock()
		switch {
		case !connectStartedAt.IsZero() && (!connectDone || connectFailed):
			stage = "connect"
		case !tlsStartedAt.IsZero() && (!tlsDone || tlsFailed):
			stage = "tls"
		case (connectDone || tlsDone) && !gotFirstByte:
			stage = "first_byte"
		default:
			stage = "execute_request"
		}
		traceMu.Unlock()
		result.Error = "代理连接失败"
		return result, err
	}
	statusCode = response.StatusCode
	defer response.Body.Close()
	stage = "read_response"
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if readErr != nil {
		result.Error = "读取探测响应失败"
		return result, readErr
	}
	stage = "validate_status"
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		result.Error = fmt.Sprintf("探测服务返回 HTTP %d", response.StatusCode)
		return result, errors.New(result.Error)
	}
	stage = "decode_response"
	exitIP, err := decodeProbeIP(body)
	if err != nil {
		result.Error = "探测服务响应格式无效"
		return result, err
	}
	stage = "validate_exit_ip"
	address, err := netip.ParseAddr(exitIP)
	if err != nil || (family == "ipv4" && !address.Is4()) || (family == "ipv6" && !address.Is6()) {
		result.Error = fmt.Sprintf("探测服务未返回有效 %s 出口 IP", strings.ToUpper(family))
		if err == nil {
			err = errors.New(result.Error)
		}
		return result, err
	}
	result.Status = domain.ProbeStatusHealthy
	result.LatencyMS = max(1, int(time.Since(startedAt).Milliseconds()))
	result.ExitIP = address.String()
	result.Error = ""
	stage = "complete"
	return result, nil
}

func decodeProbeIP(body []byte) (string, error) {
	var payload struct {
		IP string `json:"ip"`
	}
	if json.Unmarshal(body, &payload) == nil && strings.TrimSpace(payload.IP) != "" {
		return strings.TrimSpace(payload.IP), nil
	}
	for line := range strings.SplitSeq(string(body), "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if found && key == "ip" && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value), nil
		}
	}
	return "", errors.New("probe response does not contain an IP address")
}
