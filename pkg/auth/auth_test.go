package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestExtractCSRF(t *testing.T) {
	tests := []struct {
		name     string
		html     string
		expected string
	}{
		{
			name:     "standard meta tag double quotes",
			html:     `<meta name="csrf-token" content="token-meta-123">`,
			expected: "token-meta-123",
		},
		{
			name:     "inverted meta tag content before name",
			html:     `<meta content="token-meta-inverted" name="csrf-token">`,
			expected: "token-meta-inverted",
		},
		{
			name:     "single quoted meta tag",
			html:     `<meta name='csrf-token' content='token-meta-single'>`,
			expected: "token-meta-single",
		},
		{
			name:     "single quoted inverted meta tag",
			html:     `<meta content='token-meta-single-inv' name='csrf-token'>`,
			expected: "token-meta-single-inv",
		},
		{
			name:     "hidden input authenticity_token name before value",
			html:     `<input type="hidden" name="authenticity_token" value="auth-val-123" />`,
			expected: "auth-val-123",
		},
		{
			name:     "hidden input authenticity_token value before name",
			html:     `<input type="hidden" value="auth-val-inverted" name="authenticity_token" />`,
			expected: "auth-val-inverted",
		},
		{
			name:     "hidden input single quoted",
			html:     `<input name='authenticity_token' value='auth-val-single'>`,
			expected: "auth-val-single",
		},
		{
			name:     "hidden input csrf_token",
			html:     `<input type="hidden" name="csrf_token" value="csrf-simple-token" />`,
			expected: "csrf-simple-token",
		},
		{
			name:     "uppercase HTML tag and attributes with extra spaces",
			html:     `<META   NAME="csrf-token"   CONTENT="spaced-token" />`,
			expected: "spaced-token",
		},
		{
			name:     "empty html",
			html:     "",
			expected: "",
		},
		{
			name:     "html with no csrf tokens",
			html:     `<html><body><h1>Hello World</h1></body></html>`,
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractCSRF(tt.html)
			if got != tt.expected {
				t.Errorf("ExtractCSRF() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestCSRFManager(t *testing.T) {
	mgr := NewCSRFManager()
	if mgr.Token() != "" {
		t.Errorf("Expected initial empty token, got %q", mgr.Token())
	}

	html := `<meta name="csrf-token" content="cached-token-1">`
	token, err := mgr.ExtractAndSet(html)
	if err != nil {
		t.Fatalf("ExtractAndSet failed: %v", err)
	}
	if token != "cached-token-1" {
		t.Errorf("Expected token cached-token-1, got %q", token)
	}
	if mgr.Token() != "cached-token-1" {
		t.Errorf("Expected mgr.Token() = cached-token-1, got %q", mgr.Token())
	}

	// Empty HTML should return error and not overwrite existing token
	_, err = mgr.ExtractAndSet(`<div>No csrf here</div>`)
	if err == nil {
		t.Errorf("Expected error when token not found")
	}
	if mgr.Token() != "cached-token-1" {
		t.Errorf("Token was corrupted on failed extraction")
	}

	mgr.Clear()
	if mgr.Token() != "" {
		t.Errorf("Expected empty token after Clear(), got %q", mgr.Token())
	}
}

func TestLoadCredentialsFromReader(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		wantEmail    string
		wantPassword string
	}{
		{
			name: "plain unquoted",
			input: `RPI_EMAIL=user@example.com
RPI_PASSWORD=secret123`,
			wantEmail:    "user@example.com",
			wantPassword: "secret123",
		},
		{
			name: "double quoted with spaces",
			input: `RPI_EMAIL="user@example.com"
RPI_PASSWORD="my secret password"`,
			wantEmail:    "user@example.com",
			wantPassword: "my secret password",
		},
		{
			name: "single quoted",
			input: `RPI_EMAIL='user@example.com'
RPI_PASSWORD='pass with single quotes'`,
			wantEmail:    "user@example.com",
			wantPassword: "pass with single quotes",
		},
		{
			name: "comments and whitespace around equals",
			input: `# This is a comment
RPI_EMAIL = test@domain.org
# Another comment
RPI_PASSWORD =  'my_pass'  
`,
			wantEmail:    "test@domain.org",
			wantPassword: "my_pass",
		},
		{
			name: "missing values",
			input: `OTHER_KEY=hello
RPI_EMAIL=only_email@domain.com
`,
			wantEmail:    "only_email@domain.com",
			wantPassword: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			email, password := LoadCredentialsFromReader(strings.NewReader(tt.input))
			if email != tt.wantEmail {
				t.Errorf("email = %q, want %q", email, tt.wantEmail)
			}
			if password != tt.wantPassword {
				t.Errorf("password = %q, want %q", password, tt.wantPassword)
			}
		})
	}
}

func TestZeroBytes(t *testing.T) {
	buf := []byte("super_secret_credentials")
	ZeroBytes(buf)
	for i, b := range buf {
		if b != 0 {
			t.Fatalf("Byte at index %d was not zeroed: %v", i, b)
		}
	}
}

func TestEnsureAuthenticated_PreCheckAndCredentialHygiene(t *testing.T) {
	// Case 1: Session expired and credentials missing -> should fail immediately without network call
	cookiePath := t.TempDir() + "/cookies.json"
	jar, err := NewFileCookieJar(cookiePath)
	if err != nil {
		t.Fatalf("Failed to create jar: %v", err)
	}

	client := &http.Client{Jar: jar}

	// jar has no cookies, so session is expired.
	err = EnsureAuthenticated(context.Background(), client, "", "")
	if err == nil {
		t.Fatalf("Expected error for missing credentials when session is expired")
	}
	if !strings.Contains(err.Error(), "session expired") {
		t.Errorf("Unexpected error message: %v", err)
	}

	// Case 2: Ensure password is never leaked in errors
	password := "UltraSecretPassword12345!"
	var probeCount int32
	var loginCount int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "devices") {
			atomic.AddInt32(&probeCount, 1)
			// Return redirect to login page (simulating unauthenticated session)
			http.Redirect(w, r, "/sign-in", http.StatusFound)
			return
		}
		if strings.Contains(r.URL.Path, "sign-in") {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, `<html><head><meta name="csrf-token" content="mock-csrf"></head><body>Sign In</body></html>`)
			return
		}
		if strings.Contains(r.URL.Path, "raspberry_pi") {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, `<html><head><meta name="csrf-token" content="mock-id-csrf"></head><body>ID Sign In</body></html>`)
			return
		}
		if strings.Contains(r.URL.Path, "session") {
			atomic.AddInt32(&loginCount, 1)
			// Simulating invalid credentials login failure redirecting back to /sign-in
			http.Redirect(w, r, "/sign-in?error=invalid_credentials", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Seed cookie jar with a valid cookie for server URL
	serverURL, _ := url.Parse(server.URL)
	jar.SetCookies(serverURL, []*http.Cookie{
		{
			Name:    "connect_session",
			Value:   "valid_sess",
			Path:    "/",
			Expires: time.Now().Add(1 * time.Hour),
		},
	})

	// Check HasValidSession returns true for server host
	if !jar.HasValidSession(serverURL.Host, 5*time.Minute) {
		t.Fatalf("Expected session to be valid for %s", serverURL.Host)
	}

	// Call EnsureAuthenticatedCustom (or EnsureAuthenticated with custom endpoints for testing)
	err = EnsureAuthenticatedWithBase(context.Background(), client, server.URL, "user@example.com", password)
	if err == nil {
		t.Fatalf("Expected login failure error")
	}

	// CRITICAL CREDENTIAL HYGIENE CHECK:
	// Verify error does NOT contain password
	if strings.Contains(err.Error(), password) {
		t.Fatalf("SECURITY VIOLATION: Password leaked into error message: %v", err)
	}
}

func TestEnsureAuthenticated_PreCheckSkipsProbe(t *testing.T) {
	var probeCount int32
	var signInCount int32
	var authCount int32
	var sessionCount int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/devices"):
			atomic.AddInt32(&probeCount, 1)
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, `devices list`)
		case strings.HasSuffix(r.URL.Path, "/sign-in"):
			atomic.AddInt32(&signInCount, 1)
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, `<html><head><meta name="csrf-token" content="token-step1"></head><body>Sign In</body></html>`)
		case strings.HasSuffix(r.URL.Path, "/auth/raspberry_pi"):
			atomic.AddInt32(&authCount, 1)
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, `<html><head><input type="hidden" name="authenticity_token" value="token-step2" /></head><body>ID Sign In</body></html>`)
		case strings.HasSuffix(r.URL.Path, "/session"):
			atomic.AddInt32(&sessionCount, 1)
			// Successful login redirects to /devices
			http.Redirect(w, r, "/devices", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cookiePath := t.TempDir() + "/cookies.json"
	jar, err := NewFileCookieJar(cookiePath)
	if err != nil {
		t.Fatalf("Failed to create jar: %v", err)
	}

	serverURL, _ := url.Parse(server.URL)
	// Seed with expired cookie
	jar.SetCookies(serverURL, []*http.Cookie{
		{
			Name:    "connect_session",
			Value:   "old_expired",
			Path:    "/",
			Expires: time.Now().Add(-1 * time.Hour),
		},
	})

	client := &http.Client{Jar: jar}

	err = EnsureAuthenticatedWithBase(context.Background(), client, server.URL, "user@example.com", "mypassword")
	if err != nil {
		t.Fatalf("Expected successful login, got: %v", err)
	}

	// Verify that probe (/devices) was SKIPPED initially due to pre-check
	// Note: probeCount will only be 1 if redirected to /devices on successful login
	if atomic.LoadInt32(&signInCount) != 1 {
		t.Errorf("Expected 1 sign-in fetch, got %d", atomic.LoadInt32(&signInCount))
	}
	if atomic.LoadInt32(&authCount) != 1 {
		t.Errorf("Expected 1 auth post, got %d", atomic.LoadInt32(&authCount))
	}
	if atomic.LoadInt32(&sessionCount) != 1 {
		t.Errorf("Expected 1 session post, got %d", atomic.LoadInt32(&sessionCount))
	}
}

func TestCSRFManager_Concurrent(t *testing.T) {
	mgr := NewCSRFManager()
	done := make(chan bool)

	for i := 0; i < 20; i++ {
		go func(idx int) {
			for j := 0; j < 50; j++ {
				token := fmt.Sprintf("token-%d-%d", idx, j)
				mgr.SetToken(token)
				_ = mgr.Token()
				_, _ = mgr.ExtractAndSet(fmt.Sprintf(`<meta name="csrf-token" content="%s">`, token))
			}
			done <- true
		}(i)
	}

	for i := 0; i < 20; i++ {
		<-done
	}

	if mgr.Token() == "" {
		t.Errorf("Expected non-empty token after concurrent operations")
	}
}
