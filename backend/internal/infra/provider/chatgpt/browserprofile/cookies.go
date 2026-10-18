package browserprofile

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/fhttp/cookiejar"
	"golang.org/x/net/publicsuffix"
)

// Cookie is private account state. Only the ChatGPT origin's cookies survive
// login; authorization-domain cookies, OTPs and challenges are never exported.
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
// expiry and public suffix rules), recording only the configured origin.
type CookieJar struct {
	jar    *cookiejar.Jar
	origin *url.URL
	mu     sync.Mutex
	saved  map[string]Cookie
}

func NewCookieJar(origin string) *CookieJar {
	u, _ := url.Parse(origin)
	j, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	return &CookieJar{jar: j, origin: u, saved: map[string]Cookie{}}
}

func (j *CookieJar) Cookies(u *url.URL) []*fhttp.Cookie { return j.jar.Cookies(u) }

func (j *CookieJar) SetCookies(u *url.URL, cookies []*fhttp.Cookie) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.jar.SetCookies(u, cookies)
	if j.origin == nil || u.Scheme != j.origin.Scheme || u.Host != j.origin.Host {
		return
	}
	now := time.Now()
	for _, c := range cookies {
		if c == nil {
			continue
		}
		domain := strings.TrimPrefix(strings.ToLower(c.Domain), ".")
		if domain != "" && domain != strings.ToLower(u.Hostname()) {
			continue
		}
		if (&http.Cookie{Name: c.Name, Value: c.Value, Path: c.Path, Domain: c.Domain}).Valid() != nil {
			continue
		}
		path := c.Path
		if path == "" || path[0] != '/' {
			path = u.Path
			if i := strings.LastIndex(path, "/"); i > 0 {
				path = path[:i]
			} else {
				path = "/"
			}
		}
		// A host-only cookie and an explicit cookie for this host replace one
		// another in the RFC jar; do not retain both in the saved snapshot.
		key := strings.ToLower(u.Hostname()) + "\x00" + path + "\x00" + c.Name
		expires := c.Expires
		if c.MaxAge > 0 {
			expires = now.Add(time.Duration(c.MaxAge) * time.Second)
		}
		if c.MaxAge < 0 || (!expires.IsZero() && !expires.After(now)) {
			delete(j.saved, key)
			continue
		}
		if len(c.Name)+len(c.Value) > 16384 {
			continue
		}
		if _, ok := j.saved[key]; !ok && len(j.saved) >= 128 {
			continue
		}
		j.saved[key] = Cookie{Name: c.Name, Value: c.Value, Domain: c.Domain, Path: path, Expires: expires, Secure: c.Secure, HTTPOnly: c.HttpOnly, SameSite: int(c.SameSite)}
	}
}

func (j *CookieJar) Snapshot() []Cookie {
	j.mu.Lock()
	defer j.mu.Unlock()
	keys := make([]string, 0, len(j.saved))
	for key, c := range j.saved {
		if c.Expires.IsZero() || c.Expires.After(time.Now()) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	result := make([]Cookie, 0, len(keys))
	for _, key := range keys {
		result = append(result, j.saved[key])
	}
	return result
}

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

func CookieSecrets(value any) []string {
	var out []string
	for _, c := range ParseCookies(value) {
		if (&http.Cookie{Name: c.Name, Value: c.Value, Path: c.Path, Domain: c.Domain}).Valid() != nil {
			continue
		}
		if c.Value != "" {
			out = append(out, c.Value)
		}
	}
	return out
}

// TokenBinding prevents an imported/replaced access token from loading the
// previous login's cookies. The digest is private metadata, not a credential.
func TokenBinding(token string) string {
	if token == "" {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(token)))
}
