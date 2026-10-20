package browserprofile

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/fhttp/cookiejar"
	"golang.org/x/net/publicsuffix"
)

// Cookie is a cookie restored into the jar for the configured origin.
type Cookie struct {
	Name     string    `json:"name"`
	Value    string    `json:"value"`
	Domain   string    `json:"domain,omitempty"`
	Path     string    `json:"path"`
	Expires  time.Time `json:"expires,omitempty"`
	Secure   bool      `json:"secure,omitempty"`
	HTTPOnly bool      `json:"http_only,omitempty"`
	SameSite int       `json:"same_site,omitempty"`
}

// CookieJar delegates request matching to the RFC jar (host, path, secure,
// expiry and public suffix rules).
type CookieJar struct {
	jar    *cookiejar.Jar
	origin *url.URL
}

func NewCookieJar(origin string) *CookieJar {
	u, _ := url.Parse(origin)
	j, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	return &CookieJar{jar: j, origin: u}
}

func (j *CookieJar) Cookies(u *url.URL) []*fhttp.Cookie { return j.jar.Cookies(u) }

func (j *CookieJar) SetCookies(u *url.URL, cookies []*fhttp.Cookie) { j.jar.SetCookies(u, cookies) }

func (j *CookieJar) Restore(value any) {
	if j.origin == nil {
		return
	}
	for _, c := range ParseCookies(value) {
		if (&http.Cookie{Name: c.Name, Value: c.Value, Path: c.Path, Domain: c.Domain}).Valid() != nil {
			continue
		}
		if !c.Expires.IsZero() && !c.Expires.After(time.Now()) {
			continue
		}
		domain := strings.TrimPrefix(strings.ToLower(c.Domain), ".")
		if domain != "" && domain != strings.ToLower(j.origin.Hostname()) {
			continue
		}
		j.SetCookies(j.origin, []*fhttp.Cookie{{Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path, Expires: c.Expires, Secure: c.Secure, HttpOnly: c.HTTPOnly, SameSite: fhttp.SameSite(c.SameSite)}})
	}
}

func ParseCookies(value any) []Cookie {
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > 256<<10 {
		return nil
	}
	var cookies []Cookie
	if json.Unmarshal(raw, &cookies) != nil || len(cookies) > 128 {
		return nil
	}
	return cookies
}
