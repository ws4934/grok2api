package browserprofile

import (
	crand "crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/tls-client/profiles"
)

// Profile is the protocol-mode browser environment bound to one account
// from registration onward. It is a coherent parameter set shared by the HTTP
// headers, tls-client and the Sentinel VM runner. It is not a real browser
// fingerprint or a browser profile on disk.
type Profile struct {
	Name                   string   `json:"name"`
	TLSProfile             string   `json:"tls_profile"`
	BrowserFamily          string   `json:"browser_family"`
	BrowserOS              string   `json:"browser_os"`
	NavigatorPlatform      string   `json:"navigator_platform"`
	NavigatorVendor        string   `json:"navigator_vendor"`
	UserAgentDataPlatform  string   `json:"user_agent_data_platform"`
	ChromeMajor            string   `json:"chrome_major"`
	ChromeFullVersion      string   `json:"chrome_full_version"`
	UserAgent              string   `json:"user_agent"`
	SecCHUA                string   `json:"sec_ch_ua"`
	SecCHUAFullVersionList string   `json:"sec_ch_ua_full_version_list"`
	SecCHUAPlatform        string   `json:"sec_ch_ua_platform"`
	SecCHUAPlatformVersion string   `json:"sec_ch_ua_platform_version"`
	SecCHUAMobile          string   `json:"sec_ch_ua_mobile"`
	SecCHUAArch            string   `json:"sec_ch_ua_arch"`
	SecCHUABitness         string   `json:"sec_ch_ua_bitness"`
	SecCHUAModel           string   `json:"sec_ch_ua_model"`
	AcceptLanguage         string   `json:"accept_language"`
	NavigatorLanguage      string   `json:"navigator_language"`
	NavigatorLanguages     []string `json:"navigator_languages"`
	TimezoneIANA           string   `json:"timezone_iana"`
	TimezoneName           string   `json:"timezone_name"`
	TimezoneOffsetMinutes  int      `json:"timezone_offset_minutes"`
	ScreenWidth            int      `json:"screen_width"`
	ScreenHeight           int      `json:"screen_height"`
	AvailWidth             int      `json:"avail_width"`
	AvailHeight            int      `json:"avail_height"`
	OuterWidth             int      `json:"outer_width"`
	OuterHeight            int      `json:"outer_height"`
	InnerWidth             int      `json:"inner_width"`
	InnerHeight            int      `json:"inner_height"`
	HardwareConcurrency    int      `json:"hardware_concurrency"`
	DeviceMemory           int      `json:"device_memory"`
	JSHeapSizeLimit        int64    `json:"js_heap_size_limit"`
	DevicePixelRatio       int      `json:"device_pixel_ratio"`
}

// Base contains the fields that Turb's protocol profile keeps
// stable across its candidate desktop environments. tls-client v1.9.2 has a
// Chrome 131 profile, so the HTTP and JavaScript versions intentionally stay
// on Chrome 131 until a matching newer TLS profile is available locally.
func Base(width, height, cores int, heap int64) Profile {
	const chromeMajor = "131"
	const chromeFullVersion = "131.0.0.0"
	return Profile{
		Name:                   fmt.Sprintf("mac-chrome%s-%dx%d-%dc", chromeMajor, width, height, cores),
		TLSProfile:             "chrome_131",
		BrowserFamily:          "chrome",
		BrowserOS:              "macOS",
		NavigatorPlatform:      "MacIntel",
		NavigatorVendor:        "Google Inc.",
		UserAgentDataPlatform:  "macOS",
		ChromeMajor:            chromeMajor,
		ChromeFullVersion:      chromeFullVersion,
		UserAgent:              fmt.Sprintf("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%s Safari/537.36", chromeFullVersion),
		SecCHUA:                fmt.Sprintf(`"Google Chrome";v="%s", "Chromium";v="%s", "Not_A Brand";v="24"`, chromeMajor, chromeMajor),
		SecCHUAFullVersionList: fmt.Sprintf(`"Google Chrome";v="%s", "Chromium";v="%s", "Not_A Brand";v="24.0.0.0"`, chromeFullVersion, chromeFullVersion),
		SecCHUAPlatform:        `"macOS"`,
		SecCHUAPlatformVersion: `"15.7.0"`,
		SecCHUAMobile:          "?0",
		SecCHUAArch:            `"arm"`,
		SecCHUABitness:         `"64"`,
		SecCHUAModel:           `""`,
		AcceptLanguage:         "en-US,en;q=0.9",
		NavigatorLanguage:      "en-US",
		NavigatorLanguages:     []string{"en-US", "en"},
		TimezoneIANA:           "UTC",
		TimezoneName:           "Coordinated Universal Time",
		TimezoneOffsetMinutes:  0,
		ScreenWidth:            width,
		ScreenHeight:           height,
		AvailWidth:             width,
		AvailHeight:            maxInt(0, height-25),
		OuterWidth:             width,
		OuterHeight:            height,
		InnerWidth:             width,
		InnerHeight:            maxInt(0, height-87),
		HardwareConcurrency:    cores,
		DeviceMemory:           8,
		JSHeapSizeLimit:        heap,
		DevicePixelRatio:       2,
	}
}

// browserProfilePool follows Turb's protocol-mode approach: randomize a
// small set of plausible desktop environments, while keeping each selected
// profile internally consistent for the lifetime of one registration task.
var browserProfilePool = []Profile{
	Base(1680, 1050, 6, 4395630592),
	Base(1440, 900, 8, 4294967296),
	Base(1512, 982, 8, 4294967296),
	Base(1680, 1050, 8, 4294967296),
	Base(1728, 1117, 10, 4294967296),
	Base(1800, 1169, 10, 4294967296),
	Base(2056, 1329, 12, 4294967296),
}

// New selects a profile for a new protocol session. The
// selected profile is a value, so callers can safely persist and reuse it.
func New() Profile {
	if len(browserProfilePool) == 0 {
		return Base(1680, 1050, 8, 4294967296)
	}
	index, err := crand.Int(crand.Reader, big.NewInt(int64(len(browserProfilePool))))
	if err != nil {
		index = big.NewInt(0)
	}
	profile := browserProfilePool[int(index.Int64())]
	profile.NavigatorLanguages = append([]string(nil), profile.NavigatorLanguages...)
	return profile
}

func (p Profile) Valid() bool {
	return p.TLSProfile == "chrome_131" &&
		p.BrowserFamily == "chrome" &&
		p.BrowserOS == "macOS" &&
		p.UserAgent != "" && p.SecCHUA != "" &&
		p.NavigatorPlatform != "" && p.UserAgentDataPlatform != "" &&
		p.ScreenWidth > 0 && p.ScreenHeight > 0 &&
		p.HardwareConcurrency > 0 && p.DeviceMemory > 0 &&
		p.JSHeapSizeLimit > 0 && p.DevicePixelRatio > 0
}

func (p Profile) TLSClientProfile() profiles.ClientProfile {
	// Keep the mapping explicit. If another tls-client profile is added later,
	// it must be paired with matching UA/client-hint/Sentinel versions.
	return profiles.Chrome_131
}

func (p Profile) NavigatorLanguagesValue() string {
	if len(p.NavigatorLanguages) == 0 {
		return p.NavigatorLanguage
	}
	return strings.Join(p.NavigatorLanguages, ",")
}

func (p Profile) FingerprintMetadata() map[string]any {
	return map[string]any{
		"profile":                  p.Name,
		"user-agent":               p.UserAgent,
		"impersonate":              p.TLSProfile,
		"browser-family":           p.BrowserFamily,
		"browser-os":               p.BrowserOS,
		"navigator-platform":       p.NavigatorPlatform,
		"user-agent-data-platform": p.UserAgentDataPlatform,
		"sec-ch-ua":                p.SecCHUA,
		"sec-ch-ua-platform":       p.SecCHUAPlatform,
		"accept-language":          p.AcceptLanguage,
		"timezone":                 p.TimezoneIANA,
		"screen":                   fmt.Sprintf("%dx%d", p.ScreenWidth, p.ScreenHeight),
		"hardware-concurrency":     p.HardwareConcurrency,
		"device-memory":            p.DeviceMemory,
		"device-pixel-ratio":       p.DevicePixelRatio,
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Parse restores private account/task state without retaining
// references to its mutable slices. Unsupported profiles must not be reused.
func Parse(value any) (Profile, bool) {
	raw, err := json.Marshal(value)
	var profile Profile
	if err != nil || json.Unmarshal(raw, &profile) != nil || !profile.Valid() {
		return Profile{}, false
	}
	if profile.ChromeMajor != "131" || !strings.HasPrefix(profile.ChromeFullVersion, "131.") ||
		!strings.Contains(profile.UserAgent, "Chrome/"+profile.ChromeFullVersion) {
		return Profile{}, false
	}
	return profile, true
}

// FromFingerprint recovers the exact known template used by
// older accounts after their registration journal was cleared. Do not infer a
// different environment from an unknown name or a conflicting saved summary.
func FromFingerprint(value any) (Profile, bool) {
	fp, ok := value.(map[string]any)
	if !ok {
		return Profile{}, false
	}
	for _, candidate := range browserProfilePool {
		if stringValue(fp["profile"]) != candidate.Name {
			continue
		}
		// Timezone follows egress and is no longer part of the fixed template.
		// Validate it separately; all stable browser fields must still match.
		if value, exists := fp["timezone"]; exists {
			zone := stringValue(value)
			if zone == "" || zone == "Local" || len(zone) > 100 || strings.Contains(zone, "..") {
				return Profile{}, false
			}
			loc, err := time.LoadLocation(zone)
			if err != nil {
				return Profile{}, false
			}
			candidate.SetTimezone(loc, time.Now())
		}
		matches := true
		for key, want := range candidate.FingerprintMetadata() {
			if got, exists := fp[key]; exists && stringValue(got) != stringValue(want) {
				matches = false
				break
			}
		}
		if matches {
			return Parse(candidate)
		}
	}
	return Profile{}, false
}

// Default is a deterministic environment for imports without a known profile.
func Default() Profile { return Base(1680, 1050, 8, 4294967296) }
func Candidates() []Profile {
	out := make([]Profile, len(browserProfilePool))
	for i, p := range browserProfilePool {
		p.NavigatorLanguages = append([]string(nil), p.NavigatorLanguages...)
		out[i] = p
	}
	return out
}
func stringValue(v any) string {
	if v == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(v))
}
func (p *Profile) SetTimezone(loc *time.Location, at time.Time) {
	name, seconds := at.In(loc).Zone()
	p.TimezoneIANA = loc.String()
	p.TimezoneName = name
	if p.TimezoneIANA == "UTC" {
		p.TimezoneName = "Coordinated Universal Time"
	}
	p.TimezoneOffsetMinutes = -seconds / 60
}

// Headers is the shared registration and authenticated Web browser identity.
func (p Profile) Headers() map[string]string {
	return map[string]string{
		"User-Agent": p.UserAgent, "Accept-Language": p.AcceptLanguage, "Sec-CH-UA": p.SecCHUA,
		"Sec-CH-UA-Full-Version": `"` + p.ChromeFullVersion + `"`, "Sec-CH-UA-Full-Version-List": p.SecCHUAFullVersionList,
		"Sec-CH-UA-Mobile": p.SecCHUAMobile, "Sec-CH-UA-Platform": p.SecCHUAPlatform, "Sec-CH-UA-Platform-Version": p.SecCHUAPlatformVersion,
		"Sec-CH-UA-Arch": p.SecCHUAArch, "Sec-CH-UA-Bitness": p.SecCHUABitness, "Sec-CH-UA-Model": p.SecCHUAModel,
	}
}

// SetHeaderOrder keeps both transports deterministic; map iteration must not
// randomly change header order between login and subsequent Web requests.
func SetHeaderOrder(headers fhttp.Header) {
	keys := map[string]bool{"cookie": true} // The jar adds cookies after this call.
	for key := range headers {
		if key != fhttp.HeaderOrderKey && key != fhttp.PHeaderOrderKey {
			keys[strings.ToLower(key)] = true
		}
	}
	order := make([]string, 0, len(keys))
	for key := range keys {
		order = append(order, key)
	}
	sort.Strings(order)
	headers[fhttp.HeaderOrderKey] = order
}
