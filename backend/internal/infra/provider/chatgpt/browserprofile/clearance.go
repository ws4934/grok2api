package browserprofile

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/bogdanfinn/tls-client/profiles"
)

var chromeVersionPattern = regexp.MustCompile(`Chrome/((\d+)\.[\d.]+)`)

// ForUserAgent derives a runtime-only profile that presents the browser which
// solved a Cloudflare challenge. A cf_clearance cookie is honored only with the
// solver's User-Agent, so headers, client hints, the TLS fingerprint and the
// Sentinel environment all switch together; screen and locale stay the account's.
func ForUserAgent(base Profile, userAgent string) (Profile, bool) {
	match := chromeVersionPattern.FindStringSubmatch(userAgent)
	if match == nil || len(userAgent) > 512 {
		return base, false
	}
	p := base
	p.UserAgent, p.ChromeFullVersion, p.ChromeMajor = userAgent, match[1], match[2]
	p.TLSProfile = nearestChromeTLSProfile(p.ChromeMajor)
	switch {
	case strings.Contains(userAgent, "Windows"):
		p.BrowserOS, p.NavigatorPlatform, p.UserAgentDataPlatform = "Windows", "Win32", "Windows"
		p.SecCHUAPlatform, p.SecCHUAPlatformVersion, p.SecCHUAArch = `"Windows"`, `"15.0.0"`, `"x86"`
	case strings.Contains(userAgent, "Linux"):
		p.BrowserOS, p.NavigatorPlatform, p.UserAgentDataPlatform = "Linux", "Linux x86_64", "Linux"
		p.SecCHUAPlatform, p.SecCHUAPlatformVersion, p.SecCHUAArch = `"Linux"`, `""`, `"x86"`
	}
	p.SecCHUA = fmt.Sprintf(`"Google Chrome";v="%s", "Chromium";v="%s", "Not_A Brand";v="24"`, p.ChromeMajor, p.ChromeMajor)
	p.SecCHUAFullVersionList = fmt.Sprintf(`"Google Chrome";v="%s", "Chromium";v="%s", "Not_A Brand";v="24.0.0.0"`, p.ChromeFullVersion, p.ChromeFullVersion)
	p.Name = "clearance-" + strings.ToLower(p.BrowserOS) + "-chrome" + p.ChromeMajor
	return p, true
}

// nearestChromeTLSProfile picks the tls-client Chrome profile closest to major.
func nearestChromeTLSProfile(major string) string {
	if _, ok := profiles.MappedTLSClients["chrome_"+major]; ok {
		return "chrome_" + major
	}
	want, _ := strconv.Atoi(major)
	best, distance := "chrome_131", int(^uint(0)>>1)
	for name := range profiles.MappedTLSClients {
		version, err := strconv.Atoi(strings.TrimPrefix(name, "chrome_"))
		if !strings.HasPrefix(name, "chrome_") || err != nil {
			continue
		}
		d := version - want
		if d < 0 {
			d = -d
		}
		if d < distance || d == distance && name > best {
			best, distance = name, d
		}
	}
	return best
}
