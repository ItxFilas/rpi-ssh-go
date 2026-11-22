package auth

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestCookieStorePersistence(t *testing.T) {
	tmpDir := t.TempDir()
	cookiePath := filepath.Join(tmpDir, "cookies.json")

	store1, err := NewFileCookieJar(cookiePath)
	if err != nil {
		t.Fatalf("Failed to create store1: %v", err)
	}

	testURL, _ := url.Parse("https://connect.raspberrypi.com")
	testCookie := &http.Cookie{
		Name:     "session_id",
		Value:    "test_abc_123",
		Path:     "/",
		Domain:   "connect.raspberrypi.com",
		Expires:  time.Now().Add(24 * time.Hour),
		HttpOnly: true,
	}

	store1.SetCookies(testURL, []*http.Cookie{testCookie})
	if err := store1.Save(); err != nil {
		t.Fatalf("Failed to save store1: %v", err)
	}

	// Verify file was written
	if _, err := os.Stat(cookiePath); err != nil {
		t.Fatalf("Cookie file does not exist: %v", err)
	}

	// Load in fresh store2
	store2, err := NewFileCookieJar(cookiePath)
	if err != nil {
		t.Fatalf("Failed to load store2: %v", err)
	}

	loadedCookies := store2.Cookies(testURL)
	if len(loadedCookies) == 0 {
		t.Fatalf("Expected loaded cookies, got none")
	}

	found := false
	for _, c := range loadedCookies {
		if c.Name == "session_id" && c.Value == "test_abc_123" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Cookie session_id=test_abc_123 not found in loaded cookies")
	}
}

func TestIsSessionExpired(t *testing.T) {
	tmpDir := t.TempDir()
	cookiePath := filepath.Join(tmpDir, "cookies.json")
	store, err := NewFileCookieJar(cookiePath)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}

	host := "connect.raspberrypi.com"
	testURL, _ := url.Parse("https://" + host)

	// 1. Empty store should be reported as expired / invalid
	if !store.IsSessionExpired(host, 5*time.Minute) {
		t.Errorf("Expected empty store to report session expired")
	}
	if store.HasValidSession(host, 5*time.Minute) {
		t.Errorf("Expected empty store to not have valid session")
	}

	// 2. Already expired cookie
	expiredCookie := &http.Cookie{
		Name:    "connect_session",
		Value:   "old_val",
		Path:    "/",
		Domain:  host,
		Expires: time.Now().Add(-10 * time.Minute),
	}
	store.SetCookies(testURL, []*http.Cookie{expiredCookie})
	if !store.IsSessionExpired(host, 5*time.Minute) {
		t.Errorf("Expected expired cookie to report session expired")
	}
	if store.HasValidSession(host, 5*time.Minute) {
		t.Errorf("Expected expired cookie to report HasValidSession=false")
	}

	// 3. Cookie expiring within buffer (e.g. expires in 3 mins, buffer is 5 mins)
	imminentExpiryCookie := &http.Cookie{
		Name:    "connect_session",
		Value:   "soon_expire",
		Path:    "/",
		Domain:  host,
		Expires: time.Now().Add(3 * time.Minute),
	}
	store.SetCookies(testURL, []*http.Cookie{imminentExpiryCookie})
	if !store.IsSessionExpired(host, 5*time.Minute) {
		t.Errorf("Expected cookie expiring in 3m to report expired with 5m buffer")
	}

	// 4. Valid cookie with ample time remaining (e.g. 1 hour)
	validCookie := &http.Cookie{
		Name:    "connect_session",
		Value:   "valid_val",
		Path:    "/",
		Domain:  host,
		Expires: time.Now().Add(1 * time.Hour),
	}
	store.SetCookies(testURL, []*http.Cookie{validCookie})
	if store.IsSessionExpired(host, 5*time.Minute) {
		t.Errorf("Expected cookie with 1h expiry to report session valid (not expired)")
	}
	if !store.HasValidSession(host, 5*time.Minute) {
		t.Errorf("Expected HasValidSession to be true for 1h expiry")
	}

	// 5. Test URL / scheme normalization in host argument
	if store.IsSessionExpired("https://connect.raspberrypi.com/devices", 5*time.Minute) {
		t.Errorf("Expected URL with scheme/path to resolve host correctly")
	}
	if store.IsSessionExpired("connect.raspberrypi.com:443", 5*time.Minute) {
		t.Errorf("Expected host with port to resolve host correctly")
	}

	// 6. Non-expiring session cookie (Expires.IsZero())
	sessionCookie := &http.Cookie{
		Name:  "connect_session",
		Value: "session_token",
		Path:  "/",
	}
	store.SetCookies(testURL, []*http.Cookie{sessionCookie})
	if store.IsSessionExpired(host, 5*time.Minute) {
		t.Errorf("Expected zero-time session cookie to be considered valid")
	}
}

func TestClearAndPruneExpired(t *testing.T) {
	tmpDir := t.TempDir()
	cookiePath := filepath.Join(tmpDir, "cookies.json")
	store, err := NewFileCookieJar(cookiePath)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}

	host := "connect.raspberrypi.com"
	testURL, _ := url.Parse("https://" + host)

	store.SetCookies(testURL, []*http.Cookie{
		{
			Name:    "expired_1",
			Value:   "val1",
			Path:    "/",
			Expires: time.Now().Add(-1 * time.Hour),
		},
		{
			Name:    "valid_1",
			Value:   "val2",
			Path:    "/",
			Expires: time.Now().Add(1 * time.Hour),
		},
	})

	// Prune expired
	store.PruneExpired()
	cookies := store.Cookies(testURL)
	if len(cookies) != 1 || cookies[0].Name != "valid_1" {
		t.Errorf("Expected only valid_1 after pruning, got %v", cookies)
	}

	// Clear host
	store.ClearHost(host)
	if len(store.Cookies(testURL)) != 0 {
		t.Errorf("Expected 0 cookies after ClearHost")
	}
}

func TestCookieJar_Concurrent(t *testing.T) {
	tmpDir := t.TempDir()
	cookiePath := filepath.Join(tmpDir, "cookies.json")
	store, err := NewFileCookieJar(cookiePath)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}

	testURL, _ := url.Parse("https://connect.raspberrypi.com")
	done := make(chan bool)

	for i := 0; i < 20; i++ {
		go func(idx int) {
			for j := 0; j < 50; j++ {
				store.SetCookies(testURL, []*http.Cookie{
					{
						Name:    "session_id",
						Value:   "token",
						Path:    "/",
						Expires: time.Now().Add(1 * time.Hour),
					},
				})
				_ = store.Cookies(testURL)
				_ = store.IsSessionExpired("connect.raspberrypi.com", 5*time.Minute)
				_ = store.HasValidSession("connect.raspberrypi.com", 5*time.Minute)
			}
			done <- true
		}(i)
	}

	for i := 0; i < 20; i++ {
		<-done
	}

	if err := store.Save(); err != nil {
		t.Errorf("Failed to save concurrent store: %v", err)
	}
}

func TestDeletedCookieNotRestored(t *testing.T) {
	tmpDir := t.TempDir()
	cookiePath := filepath.Join(tmpDir, "cookies.json")
	store, err := NewFileCookieJar(cookiePath)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}

	testURL, _ := url.Parse("https://connect.raspberrypi.com")
	// Set initial cookie
	initialCookie := &http.Cookie{
		Name:    "session_token",
		Value:   "secret_abc",
		Path:    "/",
		Domain:  "connect.raspberrypi.com",
		Expires: time.Now().Add(24 * time.Hour),
	}
	store.SetCookies(testURL, []*http.Cookie{initialCookie})
	if err := store.Save(); err != nil {
		t.Fatalf("Failed to save initial cookie: %v", err)
	}

	// Verify cookie is present initially
	cookies := store.Cookies(testURL)
	if len(cookies) != 1 || cookies[0].Value != "secret_abc" {
		t.Fatalf("Expected initial cookie to be present, got: %v", cookies)
	}

	// Send deletion cookie with MaxAge <= 0 (e.g. MaxAge: -1)
	deletedCookie := &http.Cookie{
		Name:   "session_token",
		Value:  "",
		Path:   "/",
		Domain: "connect.raspberrypi.com",
		MaxAge: -1,
	}
	store.SetCookies(testURL, []*http.Cookie{deletedCookie})
	if err := store.Save(); err != nil {
		t.Fatalf("Failed to save after deletion: %v", err)
	}

	// Verify cookie is removed in-memory
	if len(store.Cookies(testURL)) != 0 {
		t.Fatalf("Expected cookie to be deleted in memory, but got: %v", store.Cookies(testURL))
	}

	// Reload from disk into a fresh store
	store2, err := NewFileCookieJar(cookiePath)
	if err != nil {
		t.Fatalf("Failed to load store2: %v", err)
	}

	// Assert deleted cookie did not resurrect from disk
	loadedCookies := store2.Cookies(testURL)
	if len(loadedCookies) != 0 {
		t.Fatalf("Deleted cookie resurrected from disk after reload: %v", loadedCookies)
	}

	// Also test deletion via past Expires
	cookie2 := &http.Cookie{
		Name:    "expired_token",
		Value:   "exp_val",
		Path:    "/",
		Domain:  "connect.raspberrypi.com",
		Expires: time.Now().Add(24 * time.Hour),
	}
	store.SetCookies(testURL, []*http.Cookie{cookie2})
	_ = store.Save()

	expiredCookie := &http.Cookie{
		Name:    "expired_token",
		Value:   "",
		Path:    "/",
		Domain:  "connect.raspberrypi.com",
		Expires: time.Now().Add(-1 * time.Hour),
	}
	store.SetCookies(testURL, []*http.Cookie{expiredCookie})
	_ = store.Save()

	store3, err := NewFileCookieJar(cookiePath)
	if err != nil {
		t.Fatalf("Failed to load store3: %v", err)
	}
	if len(store3.Cookies(testURL)) != 0 {
		t.Fatalf("Expired cookie resurrected from disk after reload: %v", store3.Cookies(testURL))
	}
}

func TestCookieMaxAgeSurvivesRestart(t *testing.T) {
	tmpDir := t.TempDir()
	cookiePath := filepath.Join(tmpDir, "cookies.json")
	store, err := NewFileCookieJar(cookiePath)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}

	testURL, _ := url.Parse("https://connect.raspberrypi.com")
	// Cookie with positive MaxAge (7200s = 2h), no initial Expires set
	cookie := &http.Cookie{
		Name:   "remember_me",
		Value:  "token_xyz",
		Path:   "/",
		Domain: "connect.raspberrypi.com",
		MaxAge: 7200,
	}
	store.SetCookies(testURL, []*http.Cookie{cookie})
	if err := store.Save(); err != nil {
		t.Fatalf("Failed to save: %v", err)
	}

	// Reload from fresh store
	store2, err := NewFileCookieJar(cookiePath)
	if err != nil {
		t.Fatalf("Failed to load store2: %v", err)
	}

	loadedCookies := store2.Cookies(testURL)
	if len(loadedCookies) != 1 {
		t.Fatalf("Expected 1 cookie after restart, got %d", len(loadedCookies))
	}

	c := loadedCookies[0]
	if c.Name != "remember_me" || c.Value != "token_xyz" {
		t.Errorf("Unexpected cookie content: %+v", c)
	}

	// Verify absolute expiry was computed and stored (should be ~2 hours from now, not zero)
	if c.Expires.IsZero() {
		t.Errorf("Expected reloaded cookie to have computed absolute expiry, but Expires was zero")
	}
	remaining := time.Until(c.Expires)
	if remaining < 7000*time.Second || remaining > 7300*time.Second {
		t.Errorf("Expected remaining expiry ~7200s, got %v", remaining)
	}
}

func TestSameNameDifferentPathCookies(t *testing.T) {
	tmpDir := t.TempDir()
	cookiePath := filepath.Join(tmpDir, "cookies.json")
	store, err := NewFileCookieJar(cookiePath)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}

	rootURL, _ := url.Parse("https://connect.raspberrypi.com/")
	devURL, _ := url.Parse("https://connect.raspberrypi.com/devices")

	// Set cookie at root path "/"
	store.SetCookies(rootURL, []*http.Cookie{
		{
			Name:    "pref",
			Value:   "root_pref",
			Path:    "/",
			Expires: time.Now().Add(1 * time.Hour),
		},
	})

	// Set cookie with SAME name at path "/devices"
	store.SetCookies(devURL, []*http.Cookie{
		{
			Name:    "pref",
			Value:   "dev_pref",
			Path:    "/devices",
			Expires: time.Now().Add(1 * time.Hour),
		},
	})

	if err := store.Save(); err != nil {
		t.Fatalf("Failed to save: %v", err)
	}

	// Load in fresh store
	store2, err := NewFileCookieJar(cookiePath)
	if err != nil {
		t.Fatalf("Failed to load store2: %v", err)
	}

	// Root URL should only see the root path cookie
	rootCookies := store2.Cookies(rootURL)
	if len(rootCookies) != 1 || rootCookies[0].Value != "root_pref" {
		t.Errorf("Expected root URL to have 'root_pref', got: %v", rootCookies)
	}

	// /devices URL should see both cookies (RFC 6265 path matching)
	devCookies := store2.Cookies(devURL)
	var foundRoot, foundDev bool
	for _, c := range devCookies {
		if c.Name == "pref" && c.Value == "root_pref" {
			foundRoot = true
		}
		if c.Name == "pref" && c.Value == "dev_pref" {
			foundDev = true
		}
	}
	if !foundRoot || !foundDev {
		t.Errorf("Expected both root and dev cookies on /devices, got: %v (foundRoot=%v, foundDev=%v)", devCookies, foundRoot, foundDev)
	}
}

func TestConcurrentSaveLeavesValidJSON(t *testing.T) {
	tmpDir := t.TempDir()
	cookiePath := filepath.Join(tmpDir, "cookies.json")
	store, err := NewFileCookieJar(cookiePath)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}

	testURL, _ := url.Parse("https://connect.raspberrypi.com")
	concurrency := 10
	iterations := 25

	var wg sync.WaitGroup
	errCh := make(chan error, concurrency*iterations)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				store.SetCookies(testURL, []*http.Cookie{
					{
						Name:    fmt.Sprintf("cookie_%d_%d", workerID, j),
						Value:   fmt.Sprintf("val_%d_%d", workerID, j),
						Path:    "/",
						Expires: time.Now().Add(1 * time.Hour),
					},
				})
				if err := store.Save(); err != nil {
					errCh <- err
				}
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatalf("Save error during concurrent writes: %v", err)
	}

	// Verify file is valid JSON and not corrupted
	content, err := os.ReadFile(cookiePath)
	if err != nil {
		t.Fatalf("Failed to read cookie file: %v", err)
	}

	// The file may be encrypted for the current user; decode it the way
	// Load does before checking the JSON inside.
	plain, err := openCookieFile(content)
	if err != nil {
		t.Fatalf("Cookie file corrupted! Could not open it: %v, raw content: %s", err, string(content))
	}

	var parsed map[string][]SerializableCookie
	if err := json.Unmarshal(plain, &parsed); err != nil {
		t.Fatalf("Cookie file corrupted! JSON unmarshal error: %v, raw content: %s", err, string(plain))
	}

	if len(parsed) == 0 {
		t.Fatalf("Expected parsed cookie entries, got 0")
	}
}

func TestDeletedCookieWithoutExplicitPath(t *testing.T) {
	tmpDir := t.TempDir()
	cookiePath := filepath.Join(tmpDir, "cookies.json")
	store, err := NewFileCookieJar(cookiePath)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}

	testURL, _ := url.Parse("https://connect.raspberrypi.com/devices")

	// Set cookie without explicit Path or Domain
	store.SetCookies(testURL, []*http.Cookie{
		{
			Name:    "no_path_token",
			Value:   "secret_value",
			Expires: time.Now().Add(24 * time.Hour),
		},
	})
	if err := store.Save(); err != nil {
		t.Fatalf("Failed to save store: %v", err)
	}

	// Verify it exists in store and on disk
	cookies := store.Cookies(testURL)
	if len(cookies) != 1 || cookies[0].Value != "secret_value" {
		t.Fatalf("Expected cookie to be present, got: %v", cookies)
	}

	// Now delete it using MaxAge: -1 WITHOUT setting Path or Domain
	deletionCookie := &http.Cookie{
		Name:   "no_path_token",
		Value:  "",
		MaxAge: -1,
	}
	store.SetCookies(testURL, []*http.Cookie{deletionCookie})
	if err := store.Save(); err != nil {
		t.Fatalf("Failed to save store after deletion: %v", err)
	}

	// In-memory check
	if len(store.Cookies(testURL)) != 0 {
		t.Fatalf("Expected cookie to be deleted in memory, but got: %v", store.Cookies(testURL))
	}

	// Reload from disk check
	store2, err := NewFileCookieJar(cookiePath)
	if err != nil {
		t.Fatalf("Failed to reload store2: %v", err)
	}
	loaded := store2.Cookies(testURL)
	if len(loaded) != 0 {
		t.Fatalf("Deleted cookie without explicit path resurrected from disk: %v", loaded)
	}

	// Also test: initial cookie has Path: "/", deletion cookie arrives on /devices without Path
	store.SetCookies(testURL, []*http.Cookie{
		{
			Name:    "root_cookie",
			Value:   "root_secret",
			Path:    "/",
			Expires: time.Now().Add(24 * time.Hour),
		},
	})
	_ = store.Save()
	if len(store.Cookies(testURL)) != 1 {
		t.Fatalf("Expected root_cookie to be present, got: %v", store.Cookies(testURL))
	}

	// Deletion cookie on /devices without Path (defaultPath("/devices") should be "/")
	store.SetCookies(testURL, []*http.Cookie{
		{
			Name:   "root_cookie",
			Value:  "",
			MaxAge: -1,
		},
	})
	_ = store.Save()

	store3, err := NewFileCookieJar(cookiePath)
	if err != nil {
		t.Fatalf("Failed to reload store3: %v", err)
	}
	if len(store3.Cookies(testURL)) != 0 {
		t.Fatalf("Expected root_cookie to be deleted by defaultPath on /devices, but got: %v", store3.Cookies(testURL))
	}
}

func TestMetadataEnrichmentSameNameDifferentPaths(t *testing.T) {
	tmpDir := t.TempDir()
	cookiePath := filepath.Join(tmpDir, "cookies.json")
	store, err := NewFileCookieJar(cookiePath)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}

	rootURL, _ := url.Parse("https://connect.raspberrypi.com/")
	devURL, _ := url.Parse("https://connect.raspberrypi.com/devices")

	t1 := time.Now().Add(1 * time.Hour).Truncate(time.Second)
	t2 := time.Now().Add(5 * time.Hour).Truncate(time.Second)

	// Set cookie at root "/"
	store.SetCookies(rootURL, []*http.Cookie{
		{
			Name:     "session",
			Value:    "root_val",
			Path:     "/",
			Secure:   false,
			HttpOnly: false,
			Expires:  t1,
		},
	})

	// Set cookie at "/devices" with same name but different path and metadata
	store.SetCookies(devURL, []*http.Cookie{
		{
			Name:     "session",
			Value:    "dev_val",
			Path:     "/devices",
			Secure:   true,
			HttpOnly: true,
			Expires:  t2,
		},
	})
	if err := store.Save(); err != nil {
		t.Fatalf("Failed to save store: %v", err)
	}

	// Reload from disk
	store2, err := NewFileCookieJar(cookiePath)
	if err != nil {
		t.Fatalf("Failed to reload store: %v", err)
	}

	devCookies := store2.Cookies(devURL)
	if len(devCookies) != 2 {
		t.Fatalf("Expected 2 cookies for /devices, got: %d (%v)", len(devCookies), devCookies)
	}

	for _, c := range devCookies {
		if c.Value == "root_val" {
			if c.Path != "/" {
				t.Errorf("Expected root_val to have Path '/', got '%s'", c.Path)
			}
			if c.Secure != false {
				t.Errorf("Expected root_val to have Secure=false, got true")
			}
			if c.HttpOnly != false {
				t.Errorf("Expected root_val to have HttpOnly=false, got true")
			}
			if !c.Expires.Equal(t1) {
				t.Errorf("Expected root_val to have Expires %v, got %v", t1, c.Expires)
			}
		} else if c.Value == "dev_val" {
			if c.Path != "/devices" {
				t.Errorf("Expected dev_val to have Path '/devices', got '%s'", c.Path)
			}
			if c.Secure != true {
				t.Errorf("Expected dev_val to have Secure=true, got false")
			}
			if c.HttpOnly != true {
				t.Errorf("Expected dev_val to have HttpOnly=true, got false")
			}
			if !c.Expires.Equal(t2) {
				t.Errorf("Expected dev_val to have Expires %v, got %v", t2, c.Expires)
			}
		} else {
			t.Errorf("Unexpected cookie value: %s", c.Value)
		}
	}

	// Also verify same-name, same-value cookies on different paths receive their distinct paths
	store.SetCookies(rootURL, []*http.Cookie{
		{
			Name:    "dup_val",
			Value:   "same",
			Path:    "/",
			Expires: t1,
		},
	})
	store.SetCookies(devURL, []*http.Cookie{
		{
			Name:    "dup_val",
			Value:   "same",
			Path:    "/devices",
			Expires: t2,
		},
	})
	_ = store.Save()

	store3, err := NewFileCookieJar(cookiePath)
	if err != nil {
		t.Fatalf("Failed to reload store: %v", err)
	}
	dupCookies := store3.Cookies(devURL)
	var hasRootPath, hasDevPath bool
	for _, c := range dupCookies {
		if c.Name == "dup_val" {
			if c.Path == "/" {
				hasRootPath = true
			} else if c.Path == "/devices" {
				hasDevPath = true
			}
		}
	}
	if !hasRootPath || !hasDevPath {
		t.Errorf("Expected dup_val to have both '/' and '/devices' paths, got hasRootPath=%v, hasDevPath=%v (%v)", hasRootPath, hasDevPath, dupCookies)
	}
}
