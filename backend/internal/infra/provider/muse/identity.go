package muse

import (
	"fmt"
	"strconv"
	"strings"
)

// browserIdentity is the desktop Chrome an account presents. Each account
// keeps one (stored in its credential state), so accounts sharing the browser
// do not all look like the same machine. The Chrome version is the running
// browser's, as a real user's Chrome updates itself.
type browserIdentity struct {
	Key string
	// token is the platform part of the user agent string.
	token string
	// platform is navigator.platform; the rest are user agent client hints.
	platform, uaPlatform, platformVersion, architecture string
}

// stateBrowser is the credential state key holding the account's identity.
const stateBrowser = "browser"

// Chrome keeps "Windows NT 10.0" and "Intel Mac OS X 10_15_7" in the user
// agent on every Windows and macOS version; the client hints tell them apart.
var browserIdentities = []browserIdentity{
	{Key: "windows-10", token: "Windows NT 10.0; Win64; x64", platform: "Win32", uaPlatform: "Windows", platformVersion: "10.0.0", architecture: "x86"},
	{Key: "windows-11", token: "Windows NT 10.0; Win64; x64", platform: "Win32", uaPlatform: "Windows", platformVersion: "19.0.0", architecture: "x86"},
	{Key: "macos-intel", token: "Macintosh; Intel Mac OS X 10_15_7", platform: "MacIntel", uaPlatform: "macOS", platformVersion: "15.7.1", architecture: "x86"},
	{Key: "macos-arm", token: "Macintosh; Intel Mac OS X 10_15_7", platform: "MacIntel", uaPlatform: "macOS", platformVersion: "26.0.1", architecture: "arm"},
	{Key: "linux", token: "X11; Linux x86_64", platform: "Linux x86_64", uaPlatform: "Linux", platformVersion: "6.8.0", architecture: "x86"},
}

// defaultChromeVersion is the bundled browser image's version
// (docker-compose muse-browser); session renewals over HTTP, which never see
// the browser, present it.
const defaultChromeVersion = "155.0.8059.40"

func identityByKey(key string) (browserIdentity, bool) {
	for _, value := range browserIdentities {
		if value.Key == key {
			return value, true
		}
	}
	return browserIdentity{}, false
}

// userAgent is the reduced user agent string Chrome sends: only the major
// version, the rest zeros.
func (b browserIdentity) userAgent(version string) string {
	major, _, _ := strings.Cut(version, ".")
	return "Mozilla/5.0 (" + b.token + ") AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + major + ".0.0.0 Safari/537.36"
}

// override is Emulation.setUserAgentOverride for the identity; it changes the
// User-Agent and Sec-CH-UA headers and navigator.userAgent, platform and
// userAgentData together, so they agree.
func (b browserIdentity) override(version string) map[string]any {
	major, _, _ := strings.Cut(version, ".")
	return map[string]any{
		"userAgent": b.userAgent(version),
		"platform":  b.platform,
		"userAgentMetadata": map[string]any{
			"brands":          chromeBrands(major, major),
			"fullVersionList": chromeBrands(major, version),
			"fullVersion":     version,
			"platform":        b.uaPlatform,
			"platformVersion": b.platformVersion,
			"architecture":    b.architecture,
			"model":           "",
			"mobile":          false,
			"bitness":         "64",
			"wow64":           false,
		},
	}
}

// clientHints are the low-entropy Sec-CH-UA headers Chrome sends on every
// request, for HTTP calls made outside the browser.
func (b browserIdentity) clientHints(version string) map[string]string {
	major, _, _ := strings.Cut(version, ".")
	brands := make([]string, 0, 3)
	for _, brand := range chromeBrands(major, major) {
		brands = append(brands, strconv.Quote(brand["brand"])+";v="+strconv.Quote(brand["version"]))
	}
	return map[string]string{"Sec-Ch-Ua": strings.Join(brands, ", "), "Sec-Ch-Ua-Mobile": "?0", "Sec-Ch-Ua-Platform": strconv.Quote(b.uaPlatform)}
}

// chromeBrands is Google Chrome's brand list: Chromium's GREASE brand,
// character pair and order are chosen by the major version, so the list
// matches what that Chrome version really sends.
func chromeBrands(major, version string) []map[string]string {
	seed, err := strconv.Atoi(major)
	if err != nil || seed < 0 {
		seed = 0
	}
	chars := []string{" ", "(", ":", "-", ".", "/", ")", ";", "=", "?", "_"}
	greaseVersion := []string{"8", "99", "24"}[seed%3]
	if version != major {
		greaseVersion += ".0.0.0"
	}
	grease := map[string]string{"brand": fmt.Sprintf("Not%sA%sBrand", chars[seed%len(chars)], chars[(seed+1)%len(chars)]), "version": greaseVersion}
	chromium := map[string]string{"brand": "Chromium", "version": version}
	chrome := map[string]string{"brand": "Google Chrome", "version": version}
	orders := [6][3]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}}
	order := orders[seed%6]
	list := make([]map[string]string, 3)
	list[order[0]], list[order[1]], list[order[2]] = grease, chromium, chrome
	return list
}
