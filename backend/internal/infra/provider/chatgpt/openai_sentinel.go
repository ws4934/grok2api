// Protocol portions adapted from SkyKoo/gptGrok2api at 15773099641575776025e21d1290b5209b058c5a. See LICENSE.reference.
package chatgpt

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	mathrand "math/rand"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/provider/chatgpt/browserprofile"
	"golang.org/x/crypto/sha3"
)

type openAIRequirements struct {
	Token      string
	ProofToken string
	Turnstile  string
	SOToken    string
}

var (
	bootstrapScriptPattern = regexp.MustCompile(`(?i)<script[^>]+src=["']([^"']+)["']`)
	bootstrapBuildPattern  = regexp.MustCompile(`data-build=["']([^"']+)["']`)
)

// sessionBrowser returns the cached browser that serves account's chatgpt.com
// calls on ctx's egress, or nil off chatgpt.com.
func (o *OpenAIImage) sessionBrowser(ctx context.Context, account wireAccount) *browserHTTP {
	if !strings.HasPrefix(o.BaseURL, "https://chatgpt.com") {
		return nil
	}
	o.browserMu.Lock()
	defer o.browserMu.Unlock()
	b, err := accountBrowser(o.browsers, o.BaseURL, environmentFrom(ctx).ProxyURL, account.Token, account.Fields, o.RequestTimeout)
	if err != nil {
		return nil
	}
	return b
}

// bootstrap loads the page once per browser session (bootstrapTTL) for its
// Sentinel scripts and build.
func (o *OpenAIImage) bootstrap(ctx context.Context, account wireAccount) ([]string, string, error) {
	session := o.sessionBrowser(ctx, account)
	if session != nil {
		if scripts, build, ok := session.bootstrapped(); ok {
			return scripts, build, nil
		}
	}
	response, err := o.do(ctx, http.MethodGet, "/", account, nil, nil, false)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, "", readOpenAIHTTPError(response, "bootstrap")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return nil, "", err
	}
	sources := []string{}
	for _, match := range bootstrapScriptPattern.FindAllStringSubmatch(string(raw), -1) {
		if len(match) < 2 {
			continue
		}
		value, unescapeErr := url.QueryUnescape(html.UnescapeString(match[1]))
		if unescapeErr == nil && value != "" {
			sources = append(sources, value)
		}
	}
	if len(sources) == 0 {
		sources = []string{"https://chatgpt.com/backend-api/sentinel/sdk.js"}
	}
	build := ""
	if match := bootstrapBuildPattern.FindStringSubmatch(string(raw)); len(match) == 2 {
		build = match[1]
	}
	if session != nil {
		session.rememberBootstrap(sources, build)
	}
	return sources, build, nil
}

func (o *OpenAIImage) chatRequirements(ctx context.Context, account wireAccount, scripts []string, build string) (openAIRequirements, error) {
	// Keep the location stable, but obtain a fresh clock sample after uploads.
	// A slow upload must not make the Sentinel timestamp stale.
	zone := currentOpenAITimezone(ctx)
	profile, err := accountBrowserProfile(account.Fields)
	if err != nil {
		return openAIRequirements{}, err
	}
	legacy := buildLegacyRequirementsToken(profile, scripts, build, zone)
	prepareBody := map[string]any{"p": legacy}
	response, err := o.do(ctx, http.MethodPost, "/backend-api/sentinel/chat-requirements/prepare", account, prepareBody, map[string]string{"Content-Type": "application/json"}, false)
	if err != nil {
		return openAIRequirements{}, err
	}
	var prepare map[string]any
	if err := decodeOpenAIJSON(response, &prepare, "chat requirements prepare"); err != nil {
		return openAIRequirements{}, err
	}
	proof := ""
	if info, ok := prepare["proofofwork"].(map[string]any); ok && boolValue(info["required"]) {
		select {
		case o.pow <- struct{}{}:
		case <-ctx.Done():
			return openAIRequirements{}, ctx.Err()
		}
		budget, cancel := context.WithTimeout(ctx, 5*time.Second)
		proof = buildProofToken(budget, stringValue(info["seed"]), stringValue(info["difficulty"]), profile, scripts, build, zone)
		cancel()
		<-o.pow
		if proof == "" {
			return openAIRequirements{}, fmt.Errorf("Sentinel proof budget exhausted or canceled")
		}
	}
	turnstile := ""
	if info, ok := prepare["turnstile"].(map[string]any); ok && boolValue(info["required"]) {
		dx := stringValue(info["dx"])
		if dx == "" {
			return openAIRequirements{}, fmt.Errorf("unsupported Sentinel challenge")
		}
		if dx != "" {
			turnstile, err = solveSentinelTurnstileToken(ctx, dx, legacy, profile)
			if err != nil {
				return openAIRequirements{}, fmt.Errorf("solve OpenAI sentinel Turnstile: %w", err)
			}
		}
	}
	finalizeBody := map[string]any{
		"prepare_token":   stringValue(prepare["prepare_token"]),
		"proof_token":     proof,
		"turnstile_token": turnstile,
	}
	response, err = o.do(ctx, http.MethodPost, "/backend-api/sentinel/chat-requirements/finalize", account, finalizeBody, map[string]string{"Content-Type": "application/json"}, false)
	if err != nil {
		return openAIRequirements{}, err
	}
	var finalized map[string]any
	if err := decodeOpenAIJSON(response, &finalized, "chat requirements finalize"); err != nil {
		return openAIRequirements{}, err
	}
	token := stringValue(finalized["token"])
	if token == "" {
		return openAIRequirements{}, fmt.Errorf("OpenAI chat requirements returned no token")
	}
	return openAIRequirements{Token: token, ProofToken: proof, Turnstile: turnstile, SOToken: stringValue(finalized["so_token"])}, nil
}

func (o *OpenAIImage) requirementHeaders(requirements openAIRequirements) map[string]string {
	headers := map[string]string{
		"Content-Type": "application/json",
		"Accept":       "application/json",
		"OpenAI-Sentinel-Chat-Requirements-Token": requirements.Token,
	}
	if requirements.ProofToken != "" {
		headers["OpenAI-Sentinel-Proof-Token"] = requirements.ProofToken
	}
	if requirements.Turnstile != "" {
		headers["OpenAI-Sentinel-Turnstile-Token"] = requirements.Turnstile
	}
	if requirements.SOToken != "" {
		headers["OpenAI-Sentinel-SO-Token"] = requirements.SOToken
	}
	return headers
}

func buildLegacyRequirementsToken(profile browserprofile.Profile, scripts []string, build string, zone openAITimezoneSnapshot) string {
	config := []any{
		fmt.Sprintf("%dx%d", profile.ScreenWidth, profile.ScreenHeight), zone.date(),
		profile.JSHeapSizeLimit, 1, profile.UserAgent, firstStringValue(map[string]any{"script": firstNonEmptyString(scripts, "https://chatgpt.com/backend-api/sentinel/sdk.js")}, "script"),
		build, profile.NavigatorLanguage, profile.NavigatorLanguagesValue(), mathrand.Float64(),
		"webdriver-false", "location", "navigator", mathrand.Float64() * 1000, firstUUID(), "",
		profile.HardwareConcurrency, zone.at.UnixMilli(), 0, 0, 0, 0, 0, 0, 0, 0,
	}
	raw, _ := json.Marshal(config)
	return "gAAAAAC" + base64.StdEncoding.EncodeToString(raw)
}

func buildProofToken(ctx context.Context, seed, difficulty string, profile browserprofile.Profile, scripts []string, build string, zone openAITimezoneSnapshot) string {
	target, err := hexBytes(difficulty)
	if err != nil || len(target) == 0 {
		return ""
	}
	config := []any{
		fmt.Sprintf("%dx%d", profile.ScreenWidth, profile.ScreenHeight), zone.date(), profile.JSHeapSizeLimit, 0, profile.UserAgent,
		firstStringValue(map[string]any{"script": firstNonEmptyString(scripts, "https://chatgpt.com/backend-api/sentinel/sdk.js")}, "script"),
		build, profile.NavigatorLanguage, profile.NavigatorLanguagesValue(), 0.5, "webdriver-false", "location", "navigator",
		0, firstUUID(), "", profile.HardwareConcurrency, zone.at.UnixMilli(), 0, 0, 0, 0, 0, 0, 0, 0,
	}
	for i := 0; i < 500000; i++ {
		if i%256 == 0 && ctx.Err() != nil {
			return ""
		}
		config[3] = i
		config[9] = i >> 1
		raw, _ := json.Marshal(config)
		encoded := base64.StdEncoding.EncodeToString(raw)
		sum := sha3.Sum512(append([]byte(seed), []byte(encoded)...))
		if len(target) <= len(sum) && bytes.Compare(sum[:len(target)], target) <= 0 {
			return "gAAAAAB" + encoded
		}
	}
	return ""
}

func hexBytes(value string) ([]byte, error) {
	if len(value)%2 != 0 {
		return nil, fmt.Errorf("invalid hex difficulty")
	}
	result := make([]byte, len(value)/2)
	for i := range result {
		var parsed uint8
		if _, err := fmt.Sscanf(value[i*2:i*2+2], "%02x", &parsed); err != nil {
			return nil, err
		}
		result[i] = parsed
	}
	return result, nil
}
