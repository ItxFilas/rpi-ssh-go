package auth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// protectedCookieFile is the on-disk form of a cookie file whose content is
// encrypted for the current user. Files without the "protected" field are
// plain JSON, as written on other systems and by earlier versions.
type protectedCookieFile struct {
	Protected string `json:"protected"`
	Data      string `json:"data"`
}

// sealCookieFile returns what is written to disk for the cookie JSON. Where
// the platform can encrypt for the current user it does; elsewhere the file
// is plain and relies on its owner-only permissions.
func sealCookieFile(plain []byte) ([]byte, error) {
	sealed, scheme, err := protectForUser(plain)
	if err != nil {
		return nil, err
	}
	if scheme == "" {
		return plain, nil
	}
	return json.Marshal(protectedCookieFile{Protected: scheme, Data: base64.StdEncoding.EncodeToString(sealed)})
}

// openCookieFile reverses sealCookieFile and passes plain files through.
func openCookieFile(stored []byte) ([]byte, error) {
	var envelope protectedCookieFile
	if err := json.Unmarshal(stored, &envelope); err != nil || envelope.Protected == "" {
		return stored, nil
	}
	sealed, err := base64.StdEncoding.DecodeString(envelope.Data)
	if err != nil {
		return nil, fmt.Errorf("cookie file is damaged: %w", err)
	}
	return unprotectForUser(sealed, envelope.Protected)
}

type SerializableCookie struct {
	Name     string    `json:"name"`
	Value    string    `json:"value"`
	Path     string    `json:"path"`
	Domain   string    `json:"domain"`
	Expires  time.Time `json:"expires"`
	Secure   bool      `json:"secure"`
	HttpOnly bool      `json:"http_only"`
}

type FileCookieJar struct {
	filePath string
	jar      *cookiejar.Jar
	entries  map[string][]*http.Cookie
	mu       sync.RWMutex
	saveMu   sync.Mutex
	dirty    bool
	modCount uint64
}

func normalizeHost(host string) string {
	host = strings.TrimSpace(host)
	if strings.Contains(host, "://") {
		if u, err := url.Parse(host); err == nil {
			host = u.Host
		}
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.ToLower(host)
}

func normalizeCookieDomain(domain, defaultHost string) string {
	d := strings.TrimSpace(domain)
	if d == "" {
		d = defaultHost
	}
	d = strings.ToLower(d)
	d = strings.TrimPrefix(d, ".")
	return d
}

func normalizeCookiePath(path string) string {
	p := strings.TrimSpace(path)
	if p == "" {
		return "/"
	}
	return p
}

func defaultPath(path string) string {
	if len(path) == 0 || path[0] != '/' {
		return "/"
	}
	i := strings.LastIndex(path, "/")
	if i == 0 {
		return "/"
	}
	return path[:i]
}

func cookieIdentityMatch(c1, c2 *http.Cookie, hostKey string) bool {
	if c1.Name != c2.Name {
		return false
	}
	d1 := normalizeCookieDomain(c1.Domain, hostKey)
	d2 := normalizeCookieDomain(c2.Domain, hostKey)
	if d1 != d2 {
		return false
	}
	p1 := normalizeCookiePath(c1.Path)
	p2 := normalizeCookiePath(c2.Path)
	return p1 == p2
}

func cookieEqual(c1, c2 *http.Cookie) bool {
	return c1.Name == c2.Name &&
		c1.Value == c2.Value &&
		c1.Path == c2.Path &&
		c1.Domain == c2.Domain &&
		c1.Expires.Equal(c2.Expires) &&
		c1.Secure == c2.Secure &&
		c1.HttpOnly == c2.HttpOnly
}

func cloneCookie(c *http.Cookie) *http.Cookie {
	clone := *c
	return &clone
}

func NewFileCookieJar(filePath string) (*FileCookieJar, error) {
	innerJar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}

	fcj := &FileCookieJar{
		filePath: filePath,
		jar:      innerJar,
		entries:  make(map[string][]*http.Cookie),
	}

	_ = fcj.Load()
	return fcj, nil
}

func (f *FileCookieJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	if len(cookies) == 0 {
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	hostKey := normalizeHost(u.Host)
	if hostKey == "" {
		hostKey = normalizeHost(u.String())
	}

	var reqHost, reqPath string
	if u != nil {
		reqHost = u.Hostname()
		reqPath = u.Path
	}
	if reqHost == "" {
		reqHost = hostKey
	}

	now := time.Now()
	var jarCookies []*http.Cookie
	existing := f.entries[hostKey]

	for _, incoming := range cookies {
		c := cloneCookie(incoming)

		// Normalize/default Domain and Path before checking expiry and matching identity
		if c.Domain == "" {
			c.Domain = reqHost
		}
		if c.Path == "" {
			c.Path = defaultPath(reqPath)
		}

		// Max-Age precedence over Expires (RFC 6265)
		if c.MaxAge > 0 {
			c.Expires = now.Add(time.Duration(c.MaxAge) * time.Second)
		}

		isExpired := c.MaxAge < 0 || (!c.Expires.IsZero() && c.Expires.Before(now))

		// Find existing matching cookie by identity (Name, Domain, Path)
		matchIdx := -1
		for i, oldC := range existing {
			if cookieIdentityMatch(oldC, c, hostKey) {
				matchIdx = i
				break
			}
		}

		if isExpired {
			// Deleted cookie
			if matchIdx >= 0 {
				existing = append(existing[:matchIdx], existing[matchIdx+1:]...)
				f.dirty = true
				f.modCount++
			}
			jarCookies = append(jarCookies, c)
		} else {
			if matchIdx >= 0 {
				if !cookieEqual(existing[matchIdx], c) {
					existing[matchIdx] = c
					f.dirty = true
					f.modCount++
				}
			} else {
				existing = append(existing, c)
				f.dirty = true
				f.modCount++
			}
			jarCookies = append(jarCookies, c)
		}
	}

	if len(existing) == 0 {
		delete(f.entries, hostKey)
	} else {
		f.entries[hostKey] = existing
	}

	f.jar.SetCookies(u, jarCookies)
}

func (f *FileCookieJar) Cookies(u *url.URL) []*http.Cookie {
	f.mu.RLock()
	defer f.mu.RUnlock()

	jarCookies := f.jar.Cookies(u)
	if len(jarCookies) == 0 {
		return jarCookies
	}

	hostKey := normalizeHost(u.Host)
	stored := f.entries[hostKey]
	if len(stored) == 0 {
		for k, v := range f.entries {
			if normalizeHost(k) == hostKey {
				stored = v
				break
			}
		}
	}

	used := make([]bool, len(stored))
	out := make([]*http.Cookie, len(jarCookies))
	for i, jc := range jarCookies {
		out[i] = cloneCookie(jc)

		bestIdx := -1
		// 1. Match on Name, Value, and Path (picking longest matching path)
		for j, sc := range stored {
			if used[j] || sc.Name != jc.Name || sc.Value != jc.Value {
				continue
			}
			if jc.Path != "" && sc.Path != jc.Path {
				continue
			}
			if bestIdx == -1 || len(sc.Path) > len(stored[bestIdx].Path) {
				bestIdx = j
			}
		}

		// 2. Fallback: match on Name (and Path if jc.Path is set) if Value didn't match
		if bestIdx == -1 {
			for j, sc := range stored {
				if used[j] || sc.Name != jc.Name {
					continue
				}
				if jc.Path != "" && sc.Path != jc.Path {
					continue
				}
				if bestIdx == -1 || len(sc.Path) > len(stored[bestIdx].Path) {
					bestIdx = j
				}
			}
		}

		if bestIdx >= 0 {
			used[bestIdx] = true
			sc := stored[bestIdx]
			out[i].Path = sc.Path
			out[i].Domain = sc.Domain
			out[i].Expires = sc.Expires
			out[i].Secure = sc.Secure
			out[i].HttpOnly = sc.HttpOnly
		}
	}
	return out
}

// IsSessionExpired checks whether the session cookies for a host are absent or expired/expiring
// within the specified buffer duration.
func (f *FileCookieJar) IsSessionExpired(host string, buffer time.Duration) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()

	normHost := normalizeHost(host)

	cookies := f.entries[normHost]
	if len(cookies) == 0 {
		for k, v := range f.entries {
			if normalizeHost(k) == normHost {
				cookies = v
				break
			}
		}
	}

	if len(cookies) == 0 {
		return true // No cookies found for host
	}

	now := time.Now()
	cutoff := now.Add(buffer)

	var sessionCookies []*http.Cookie
	for _, c := range cookies {
		name := strings.ToLower(c.Name)
		if strings.Contains(name, "session") || name == "auth_token" || name == "connect_token" {
			sessionCookies = append(sessionCookies, c)
		}
	}

	if len(sessionCookies) > 0 {
		for _, c := range sessionCookies {
			if c.Value == "" {
				continue
			}
			// Zero expiration means in-memory / non-expiring session cookie
			if c.Expires.IsZero() {
				return false
			}
			if c.Expires.After(cutoff) {
				return false
			}
		}
		return true
	}

	// Fallback when cookies exist without standard session names
	hasValid := false
	for _, c := range cookies {
		if c.Value == "" {
			continue
		}
		if c.Expires.IsZero() || c.Expires.After(cutoff) {
			hasValid = true
			break
		}
	}

	return !hasValid
}

// HasValidSession returns true if host has at least one valid, unexpired session cookie.
func (f *FileCookieJar) HasValidSession(host string, buffer time.Duration) bool {
	return !f.IsSessionExpired(host, buffer)
}

// ClearHost removes all cookies stored for the specified host and rebuilds the inner jar.
func (f *FileCookieJar) ClearHost(host string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	normHost := normalizeHost(host)
	removed := false
	for k := range f.entries {
		if normalizeHost(k) == normHost {
			delete(f.entries, k)
			removed = true
		}
	}
	if removed {
		f.dirty = true
		f.modCount++
	}

	innerJar, _ := cookiejar.New(nil)
	for h, cookies := range f.entries {
		for _, c := range cookies {
			targetURL := "https://" + h
			if c.Path != "" && c.Path != "/" {
				targetURL += c.Path
			}
			if u, err := url.Parse(targetURL); err == nil {
				innerJar.SetCookies(u, []*http.Cookie{c})
			}
		}
	}
	f.jar = innerJar
}

// Clear removes all stored cookies and resets the inner jar.
func (f *FileCookieJar) Clear() {
	f.mu.Lock()
	defer f.mu.Unlock()

	if len(f.entries) > 0 {
		f.dirty = true
		f.modCount++
	}
	f.entries = make(map[string][]*http.Cookie)
	innerJar, _ := cookiejar.New(nil)
	f.jar = innerJar
}

// PruneExpired removes all cookies whose expiration date has passed.
func (f *FileCookieJar) PruneExpired() {
	f.mu.Lock()
	defer f.mu.Unlock()

	now := time.Now()
	modified := false
	for host, cookies := range f.entries {
		var active []*http.Cookie
		for _, c := range cookies {
			if c.Expires.IsZero() || c.Expires.After(now) {
				active = append(active, c)
			} else {
				modified = true
			}
		}
		if len(active) == 0 {
			delete(f.entries, host)
		} else {
			f.entries[host] = active
		}
	}

	if modified {
		f.dirty = true
		f.modCount++
		innerJar, _ := cookiejar.New(nil)
		for h, cookies := range f.entries {
			for _, c := range cookies {
				targetURL := "https://" + h
				if c.Path != "" && c.Path != "/" {
					targetURL += c.Path
				}
				if u, err := url.Parse(targetURL); err == nil {
					innerJar.SetCookies(u, []*http.Cookie{c})
				}
			}
		}
		f.jar = innerJar
	}
}

func (f *FileCookieJar) Save() error {
	f.saveMu.Lock()
	defer f.saveMu.Unlock()

	f.mu.RLock()
	if !f.dirty {
		if _, err := os.Stat(f.filePath); err == nil {
			f.mu.RUnlock()
			return nil
		}
	}
	saveModCount := f.modCount

	data := make(map[string][]SerializableCookie, len(f.entries))
	for host, cookies := range f.entries {
		var list []SerializableCookie
		for _, c := range cookies {
			list = append(list, SerializableCookie{
				Name:     c.Name,
				Value:    c.Value,
				Path:     c.Path,
				Domain:   c.Domain,
				Expires:  c.Expires,
				Secure:   c.Secure,
				HttpOnly: c.HttpOnly,
			})
		}
		data[host] = list
	}
	f.mu.RUnlock()

	plain, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	bytes, err := sealCookieFile(plain)
	if err != nil {
		return err
	}

	dir := filepath.Dir(f.filePath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}

	tmpFile, err := os.CreateTemp(dir, "cookies-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmpFile.Name()

	if _, err := tmpFile.Write(bytes); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpName)
		return err
	}

	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpName)
		return err
	}

	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}

	_ = os.Chmod(tmpName, 0600)

	var renameErr error
	for attempt := 0; attempt < 5; attempt++ {
		renameErr = os.Rename(tmpName, f.filePath)
		if renameErr == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if renameErr != nil {
		_ = os.Remove(tmpName)
		return renameErr
	}

	f.mu.Lock()
	if f.modCount == saveModCount {
		f.dirty = false
	}
	f.mu.Unlock()

	return nil
}

func (f *FileCookieJar) Load() error {
	stored, err := os.ReadFile(f.filePath)
	if err != nil {
		return err
	}
	bytes, err := openCookieFile(stored)
	if err != nil {
		return err
	}

	var data map[string][]SerializableCookie
	if err := json.Unmarshal(bytes, &data); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	innerJar, err := cookiejar.New(nil)
	if err != nil {
		return err
	}

	now := time.Now()
	f.entries = make(map[string][]*http.Cookie)

	for host, list := range data {
		normHost := normalizeHost(host)
		var activeCookies []*http.Cookie
		for _, sc := range list {
			// Skip cookies whose expiry has passed
			if !sc.Expires.IsZero() && sc.Expires.Before(now) {
				continue
			}
			c := &http.Cookie{
				Name:     sc.Name,
				Value:    sc.Value,
				Path:     sc.Path,
				Domain:   sc.Domain,
				Expires:  sc.Expires,
				Secure:   sc.Secure,
				HttpOnly: sc.HttpOnly,
			}
			activeCookies = append(activeCookies, c)

			targetURL := "https://" + normHost
			if c.Path != "" && c.Path != "/" {
				targetURL += c.Path
			}
			if u, err := url.Parse(targetURL); err == nil {
				innerJar.SetCookies(u, []*http.Cookie{c})
			}
		}
		if len(activeCookies) > 0 {
			f.entries[normHost] = activeCookies
		}
	}
	f.jar = innerJar
	f.dirty = false
	return nil
}
