package settings

import (
	"encoding/json"
	"strings"
	"testing"

	settingsapp "github.com/chenyme/grok2api/backend/internal/application/settings"
)

func TestSettingsDTOExcludesBrowserIdentityFields(t *testing.T) {
	data, err := json.Marshal(settingsConfigDTO{})
	if err != nil {
		t.Fatal(err)
	}
	lower := strings.ToLower(string(data))
	for _, forbidden := range []string{"grok_device_id", "x-anonuserid", "x-userid", "x-challenge", "x-signature"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("settings response contains forbidden field %q", forbidden)
		}
	}
}

func TestSettingsResponseDoesNotExposeManualStatsigValue(t *testing.T) {
	response := newSettingsResponse(settingsapp.Snapshot{Config: settingsapp.EditableConfig{ProviderWeb: settingsapp.ProviderWebConfig{
		StatsigMode: "manual", StatsigManualValue: "must-not-leak", StatsigManualConfigured: true,
	}}})
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "must-not-leak") || strings.Contains(string(data), "statsigManualValue") {
		t.Fatalf("settings response leaked manual Statsig: %s", data)
	}
}

func TestSettingsResponseIncludesBuildTokenAuth(t *testing.T) {
	response := newSettingsResponse(settingsapp.Snapshot{Config: settingsapp.EditableConfig{ProviderBuild: settingsapp.ProviderBuildConfig{TokenAuth: "xai-grok-cli"}}})
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"tokenAuth":"xai-grok-cli"`) || !strings.Contains(string(data), `"tokenAuthConfigured":true`) {
		t.Fatalf("settings response lost Build token auth: %s", data)
	}
}

func TestSettingsResponseIncludesRecommendedBuildBaseline(t *testing.T) {
	response := newSettingsResponse(settingsapp.Snapshot{RecommendedProviderBuild: settingsapp.ProviderBuildRecommendation{
		ClientVersion: "1.0.60", UserAgent: "grok-shell/1.0.60 (linux; x86_64)",
	}})
	if response.RecommendedProviderBuild.ClientVersion != "1.0.60" || response.RecommendedProviderBuild.UserAgent == "" {
		t.Fatalf("recommended build = %#v", response.RecommendedProviderBuild)
	}
}

func TestSettingsResponseIncludesPreferFreeBuild(t *testing.T) {
	response := newSettingsResponse(settingsapp.Snapshot{Config: settingsapp.EditableConfig{
		Routing: settingsapp.RoutingConfig{PreferFreeBuild: true},
	}})
	if !response.Config.Routing.PreferFreeBuild {
		t.Fatal("preferFreeBuild was lost from settings response")
	}
}

func TestSettingsResponseIncludesMarkBuildChatDeniedAsReauth(t *testing.T) {
	response := newSettingsResponse(settingsapp.Snapshot{Config: settingsapp.EditableConfig{
		Routing: settingsapp.RoutingConfig{MarkBuildChatDeniedAsReauth: true},
	}})
	if !response.Config.Routing.MarkBuildChatDeniedAsReauth {
		t.Fatal("markBuildChatDeniedAsReauth was lost from settings response")
	}
}

func TestSettingsResponseIncludesBuildForbiddenCodes(t *testing.T) {
	response := newSettingsResponse(settingsapp.Snapshot{Config: settingsapp.EditableConfig{
		Accounts: settingsapp.AccountsConfig{
			MarkBuildForbiddenReauth:  true,
			BuildForbiddenReauthCodes: []string{"permission-denied", "team-access-denied"},
		},
	}})
	codes := response.Config.Accounts.BuildForbiddenReauthCodes
	if len(codes) != 2 || codes[0] != "permission-denied" || codes[1] != "team-access-denied" {
		t.Fatalf("Build forbidden codes = %#v", codes)
	}
}

func TestSettingsResponseIncludesSegmentedSelector(t *testing.T) {
	response := newSettingsResponse(settingsapp.Snapshot{Config: settingsapp.EditableConfig{
		Routing: settingsapp.RoutingConfig{SegmentedSelector: settingsapp.SegmentedSelectorConfig{
			Enabled: true, MinCandidates: 3000, WindowSize: 64,
		}},
	}})
	selector := response.Config.Routing.SegmentedSelector
	if !selector.Enabled || selector.MinCandidates != 3000 || selector.WindowSize != 64 {
		t.Fatalf("segmented selector = %#v", selector)
	}
}

func TestProviderAdobeSettingsWireShape(t *testing.T) {
	var dto settingsConfigDTO
	body := `{"providerAdobe":{"sherlockMintAPI":"http://mint:8777","timeout":"1m"}}`
	if err := json.Unmarshal([]byte(body), &dto); err != nil {
		t.Fatal(err)
	}
	want := settingsapp.ProviderAdobeConfig{SherlockMintAPI: "http://mint:8777", Timeout: "1m"}
	if input := dto.toApplication(); input.ProviderAdobe != want {
		t.Fatalf("providerAdobe input = %#v", input.ProviderAdobe)
	}
	data, err := json.Marshal(newSettingsResponse(settingsapp.Snapshot{Config: settingsapp.EditableConfig{ProviderAdobe: want}}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), body[1:len(body)-1]) {
		t.Fatalf("settings response providerAdobe = %s", data)
	}
}

func TestProviderChatGPTSettingsWireShape(t *testing.T) {
	var dto settingsConfigDTO
	body := `{"providerChatGPT":{"imageConversationModel":"gpt-5","outputTokenLimit":4096,"visionTokens":0,"defaultImageModel":"gpt-image-2","cloudflareClearance":true}}`
	if err := json.Unmarshal([]byte(body), &dto); err != nil {
		t.Fatal(err)
	}
	input := dto.toApplication()
	want := settingsapp.ProviderChatGPTConfig{ImageConversationModel: "gpt-5", OutputTokenLimit: 4096, DefaultImageModel: "gpt-image-2", CloudflareClearance: true}
	if input.ProviderChatGPT != want {
		t.Fatalf("providerChatGPT input = %#v", input.ProviderChatGPT)
	}
	data, err := json.Marshal(newSettingsResponse(settingsapp.Snapshot{Config: settingsapp.EditableConfig{ProviderChatGPT: want}}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), body[1:len(body)-1]) {
		t.Fatalf("settings response providerChatGPT = %s", data)
	}
}
