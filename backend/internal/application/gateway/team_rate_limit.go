package gateway

import (
	"strings"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

type teamModelRateLimit struct {
	TeamFingerprint string
	Until           time.Time
}

type teamRateLimitObservation struct {
	Fingerprint string
	ExpiresAt   time.Time
}

func teamModelRateLimitKey(providerValue accountdomain.Provider, teamFingerprint, upstreamModel string) string {
	return string(providerValue) + "\x00" + teamFingerprint + "\x00" + strings.TrimSpace(upstreamModel)
}

func rateLimitTeamFingerprint(teamID string) string {
	teamID = strings.ToLower(strings.TrimSpace(teamID))
	if teamID == "" {
		return ""
	}
	return security.HashToken(teamID)
}

func shortTeamFingerprint(value string) string {
	if len(value) <= 12 {
		return value
	}
	return value[:12]
}

func (s *Service) activeTeamModelRateLimit(credential accountdomain.Credential, upstreamModel string, now time.Time) (teamModelRateLimit, bool) {
	if !s.rateLimitActive.Load() {
		return teamModelRateLimit{}, false
	}
	credentialFingerprint := rateLimitTeamFingerprint(credential.TeamID)
	s.rateLimitMu.Lock()
	defer s.rateLimitMu.Unlock()
	if !s.rateLimitActive.Load() {
		return teamModelRateLimit{}, false
	}
	nextExpiry := s.rateLimitNextExpiry.Load()
	if nextExpiry <= 0 || now.UnixNano() >= nextExpiry {
		s.pruneTeamModelRateLimitsLocked(now)
		if len(s.rateLimits) == 0 {
			return teamModelRateLimit{}, false
		}
	}
	// Check the TeamID observed in an upstream response first, then current
	// credential metadata. The fallback prevents a historical observation from
	// permanently masking a later server-side team reassignment.
	observation := s.rateLimitTeams[credential.ID]
	observedFingerprint := observation.Fingerprint
	if observedFingerprint != "" && !now.Before(observation.ExpiresAt) {
		delete(s.rateLimitTeams, credential.ID)
		observedFingerprint = ""
	}
	teamFingerprints := [2]string{observedFingerprint, credentialFingerprint}
	fingerprintCount := 1
	if credentialFingerprint != observedFingerprint {
		fingerprintCount = 2
	}
	for index := 0; index < fingerprintCount; index++ {
		teamFingerprint := teamFingerprints[index]
		if teamFingerprint == "" {
			continue
		}
		key := teamModelRateLimitKey(credential.Provider, teamFingerprint, upstreamModel)
		value, ok := s.rateLimits[key]
		if !ok {
			continue
		}
		if !now.Before(value.Until) {
			delete(s.rateLimits, key)
			s.refreshTeamModelRateLimitStateLocked()
			continue
		}
		return value, true
	}
	return teamModelRateLimit{}, false
}

func (s *Service) pruneTeamModelRateLimitsLocked(now time.Time) {
	for key, value := range s.rateLimits {
		if !now.Before(value.Until) {
			delete(s.rateLimits, key)
		}
	}
	for accountID, observation := range s.rateLimitTeams {
		if !now.Before(observation.ExpiresAt) {
			delete(s.rateLimitTeams, accountID)
		}
	}
	s.refreshTeamModelRateLimitStateLocked()
}

func (s *Service) refreshTeamModelRateLimitStateLocked() {
	if len(s.rateLimits) == 0 {
		clear(s.rateLimitTeams)
		s.rateLimitNextExpiry.Store(0)
		s.rateLimitActive.Store(false)
		return
	}
	var nextExpiry time.Time
	for _, value := range s.rateLimits {
		if nextExpiry.IsZero() || value.Until.Before(nextExpiry) {
			nextExpiry = value.Until
		}
	}
	for _, observation := range s.rateLimitTeams {
		if nextExpiry.IsZero() || observation.ExpiresAt.Before(nextExpiry) {
			nextExpiry = observation.ExpiresAt
		}
	}
	s.rateLimitNextExpiry.Store(nextExpiry.UnixNano())
	s.rateLimitActive.Store(true)
}

func (s *Service) markTeamModelRateLimit(credential accountdomain.Credential, upstreamModel string, metadata provider.RateLimitMetadata, now time.Time) teamModelRateLimit {
	retryAfter := metadata.RetryAfter
	if retryAfter <= 0 {
		// RPS limits recover within about one second; do not apply the generic 1m cooldown.
		if strings.EqualFold(metadata.Scope, provider.RateLimitScopeRPS) {
			retryAfter = 2 * time.Second
		} else {
			retryAfter = time.Minute
		}
	}
	teamFingerprint := rateLimitTeamFingerprint(metadata.TeamID)
	value := teamModelRateLimit{TeamFingerprint: shortTeamFingerprint(teamFingerprint), Until: now.Add(retryAfter)}
	key := teamModelRateLimitKey(credential.Provider, teamFingerprint, upstreamModel)
	until := now.Add(retryAfter)
	s.rateLimitMu.Lock()
	s.rateLimitActive.Store(true)
	if s.rateLimits == nil {
		s.rateLimits = make(map[string]teamModelRateLimit)
	}
	if s.rateLimitTeams == nil {
		s.rateLimitTeams = make(map[uint64]teamRateLimitObservation)
	}
	if teamFingerprint != rateLimitTeamFingerprint(credential.TeamID) {
		s.rateLimitTeams[credential.ID] = teamRateLimitObservation{Fingerprint: teamFingerprint, ExpiresAt: until}
	} else {
		delete(s.rateLimitTeams, credential.ID)
	}
	for existingKey, value := range s.rateLimits {
		if !now.Before(value.Until) {
			delete(s.rateLimits, existingKey)
		}
	}
	for accountID, observation := range s.rateLimitTeams {
		if !now.Before(observation.ExpiresAt) {
			delete(s.rateLimitTeams, accountID)
		}
	}
	if current, ok := s.rateLimits[key]; ok && !current.Until.Before(until) {
		value = current
	} else {
		s.rateLimits[key] = value
	}
	s.refreshTeamModelRateLimitStateLocked()
	s.rateLimitMu.Unlock()
	return value
}
