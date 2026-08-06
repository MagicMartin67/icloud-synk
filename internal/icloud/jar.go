package icloud

import (
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// Jar is a minimal, serialisable cookie jar. net/http/cookiejar cannot be
// dumped to disk, and the whole iCloud session lives in its cookies, so we
// keep our own with just enough of RFC 6265 to talk to Apple.
type Jar struct {
	mu      sync.Mutex
	entries map[string]*http.Cookie // key: domain|path|name
}

func NewJar() *Jar {
	return &Jar{entries: make(map[string]*http.Cookie)}
}

func jarKey(c *http.Cookie) string {
	return strings.ToLower(c.Domain) + "|" + c.Path + "|" + c.Name
}

func (j *Jar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, c := range cookies {
		cc := *c
		if cc.Domain == "" {
			cc.Domain = u.Hostname()
		}
		cc.Domain = strings.TrimPrefix(strings.ToLower(cc.Domain), ".")
		if cc.Path == "" {
			cc.Path = "/"
		}
		key := jarKey(&cc)
		// A cookie is deleted by an empty value, MaxAge<0 or a past Expires.
		expired := !cc.Expires.IsZero() && cc.Expires.Before(time.Now())
		if cc.MaxAge < 0 || cc.Value == "" || expired {
			delete(j.entries, key)
			continue
		}
		j.entries[key] = &cc
	}
}

func (j *Jar) Cookies(u *url.URL) []*http.Cookie {
	j.mu.Lock()
	defer j.mu.Unlock()

	host := strings.ToLower(u.Hostname())
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	secure := u.Scheme == "https"

	var out []*http.Cookie
	for key, c := range j.entries {
		if !domainMatch(host, c.Domain) || !pathMatch(path, c.Path) {
			continue
		}
		if c.Secure && !secure {
			continue
		}
		if !c.Expires.IsZero() && c.Expires.Before(time.Now()) {
			delete(j.entries, key)
			continue
		}
		out = append(out, &http.Cookie{Name: c.Name, Value: c.Value})
	}
	// Longest path first, as required by RFC 6265 §5.4.
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}

func domainMatch(host, domain string) bool {
	if host == domain {
		return true
	}
	return strings.HasSuffix(host, "."+domain)
}

func pathMatch(reqPath, cookiePath string) bool {
	if cookiePath == "/" || reqPath == cookiePath {
		return true
	}
	if !strings.HasPrefix(reqPath, cookiePath) {
		return false
	}
	if strings.HasSuffix(cookiePath, "/") {
		return true
	}
	return len(reqPath) > len(cookiePath) && reqPath[len(cookiePath)] == '/'
}

// storedCookie is the on-disk form of a cookie.
type storedCookie struct {
	Name    string    `json:"name"`
	Value   string    `json:"value"`
	Domain  string    `json:"domain"`
	Path    string    `json:"path"`
	Expires time.Time `json:"expires,omitzero"`
	Secure  bool      `json:"secure,omitempty"`
}

func (j *Jar) export() []storedCookie {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]storedCookie, 0, len(j.entries))
	for _, c := range j.entries {
		out = append(out, storedCookie{
			Name: c.Name, Value: c.Value, Domain: c.Domain,
			Path: c.Path, Expires: c.Expires, Secure: c.Secure,
		})
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Domain != out[b].Domain {
			return out[a].Domain < out[b].Domain
		}
		return out[a].Name < out[b].Name
	})
	return out
}

func (j *Jar) importCookies(list []storedCookie) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, s := range list {
		c := &http.Cookie{
			Name: s.Name, Value: s.Value, Domain: s.Domain,
			Path: s.Path, Expires: s.Expires, Secure: s.Secure,
		}
		if c.Path == "" {
			c.Path = "/"
		}
		j.entries[jarKey(c)] = c
	}
}

// replaceAll swaps the jar contents in place. Replacing the jar object instead
// would race with requests already in flight.
func (j *Jar) replaceAll(list []storedCookie) {
	j.mu.Lock()
	j.entries = make(map[string]*http.Cookie, len(list))
	j.mu.Unlock()
	j.importCookies(list)
}

// value returns the value of a cookie by name, ignoring domain and path. Used
// for the session sanity checks (e.g. X-APPLE-WEBAUTH-USER).
func (j *Jar) value(name string) string {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, c := range j.entries {
		if strings.EqualFold(c.Name, name) {
			return c.Value
		}
	}
	return ""
}
