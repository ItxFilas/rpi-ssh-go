package auth

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf16"
)

func utf16File(text string, order binary.AppendByteOrder, bom []byte) []byte {
	out := append([]byte(nil), bom...)
	for _, unit := range utf16.Encode([]rune(text)) {
		out = order.AppendUint16(out, unit)
	}
	return out
}

// Editors and shells on Windows write .env files in several encodings. A
// byte order mark must not hide the first key.
func TestLoadCredentialsFromReaderEncodings(t *testing.T) {
	const text = "RPI_EMAIL=user@example.com\r\nRPI_PASSWORD=p@ss word\r\n"

	tests := []struct {
		name string
		data []byte
	}{
		{"utf-8 with byte order mark", append([]byte{0xEF, 0xBB, 0xBF}, text...)},
		{"utf-16 little endian", utf16File(text, binary.LittleEndian, []byte{0xFF, 0xFE})},
		{"utf-16 big endian", utf16File(text, binary.BigEndian, []byte{0xFE, 0xFF})},
		{"export prefix", []byte("export RPI_EMAIL=user@example.com\nexport RPI_PASSWORD='p@ss word'\n")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			email, password := LoadCredentialsFromReader(bytes.NewReader(tc.data))
			if email != "user@example.com" || password != "p@ss word" {
				t.Fatalf("got email %q, password %q", email, password)
			}
		})
	}
}

func writeEnvFile(t *testing.T, email, password string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
	content := fmt.Sprintf("RPI_EMAIL=%s\nRPI_PASSWORD=%s\n", email, password)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Credentials set in the MCP server's environment are the explicit choice
// for that server; a .env file left on disk must not override them.
func TestEnvironmentCredentialsOverrideEnvFile(t *testing.T) {
	file := writeEnvFile(t, "stale@example.com", "stale-password")
	env := map[string]string{"RPI_EMAIL": "current@example.com", "RPI_PASSWORD": "current-password"}

	email, password := loadCredentials([]string{file}, func(k string) string { return env[k] })
	if email != "current@example.com" || password != "current-password" {
		t.Fatalf("environment did not take precedence: got %q / %q", email, password)
	}
}

func TestEnvFileUsedWhenEnvironmentIsIncomplete(t *testing.T) {
	file := writeEnvFile(t, "file@example.com", "file-password")
	env := map[string]string{"RPI_EMAIL": "only-email@example.com"}

	email, password := loadCredentials([]string{filepath.Join(t.TempDir(), "missing.env"), file}, func(k string) string { return env[k] })
	if email != "file@example.com" || password != "file-password" {
		t.Fatalf("expected the complete pair from the file, got %q / %q", email, password)
	}
}

// Credentials are searched next to the executable, in the home directory and
// in the working directory. Nothing else, and nothing tied to one machine.
func TestCredentialPathsAreDerivedFromTheEnvironment(t *testing.T) {
	allowed := map[string]bool{".env": true}
	if exe, err := os.Executable(); err == nil {
		allowed[filepath.Join(filepath.Dir(exe), ".env")] = true
	}
	if home, _ := os.UserHomeDir(); home != "" {
		allowed[filepath.Join(home, ".rpi-connect.env")] = true
		allowed[filepath.Join(home, ".env")] = true
	}
	for _, p := range credentialPaths() {
		if !allowed[p] {
			t.Fatalf("unexpected credential search path: %s", p)
		}
	}
}

// loginServer imitates the sign-in flow up to the point where credentials
// are posted, then answers that post with finalPath and finalBody.
func loginServer(t *testing.T, finalPath, finalBody string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/session"):
			http.Redirect(w, r, finalPath, http.StatusFound)
		case strings.HasSuffix(r.URL.Path, "/devices"):
			http.Redirect(w, r, "/sign-in", http.StatusFound)
		case strings.HasSuffix(r.URL.Path, "/sign-in"), strings.HasSuffix(r.URL.Path, "/auth/raspberry_pi"):
			_, _ = fmt.Fprint(w, `<html><head><meta name="csrf-token" content="csrf"></head><body>Sign in</body></html>`)
		case r.URL.Path == finalPath:
			_, _ = fmt.Fprint(w, finalBody)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestLoginErrorExplainsTwoFactorAuthentication(t *testing.T) {
	server := loginServer(t, "/two-factor", `<html><body><form>
		<label>Enter the code from your authenticator app</label>
		<input name="otp_attempt" autocomplete="one-time-code"></form></body></html>`)

	err := EnsureAuthenticatedWithBase(context.Background(), &http.Client{}, server.URL, "user@example.com", "password")
	if err == nil {
		t.Fatal("expected the login to fail")
	}
	if !strings.Contains(err.Error(), "two-factor") {
		t.Fatalf("error should say the account needs two-factor authentication, got: %v", err)
	}
}

func TestLoginErrorExplainsRejectedCredentials(t *testing.T) {
	server := loginServer(t, "/sign-in", `<html><head><meta name="csrf-token" content="csrf"></head><body>Invalid email or password.</body></html>`)

	err := EnsureAuthenticatedWithBase(context.Background(), &http.Client{}, server.URL, "user@example.com", "password")
	if err == nil {
		t.Fatal("expected the login to fail")
	}
	if !strings.Contains(err.Error(), "RPI_EMAIL") || !strings.Contains(err.Error(), "RPI_PASSWORD") {
		t.Fatalf("error should point at the configured credentials, got: %v", err)
	}
	if strings.Contains(err.Error(), "two-factor") {
		t.Fatalf("rejected credentials were reported as two-factor authentication: %v", err)
	}
}

// The cookie file holds a live login. It must not be readable by simply
// opening the file: encrypted for the user on Windows, owner-only elsewhere.
func TestSavedCookiesAreProtected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.json")
	jar, err := NewFileCookieJar(path)
	if err != nil {
		t.Fatal(err)
	}
	const secret = "s3cr3t-session-value-0123456789"
	jar.SetCookies(mustURL(t, "https://connect.raspberrypi.com/"), []*http.Cookie{
		{Name: "connect_session", Value: secret, Path: "/"},
	})
	if err := jar.Save(); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatal("cookie value is stored in plain text")
		}
	} else {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0077 != 0 {
			t.Fatalf("cookie file mode is %o, want owner-only", info.Mode().Perm())
		}
	}

	// A new process reads the same cookies back.
	reloaded, err := NewFileCookieJar(path)
	if err != nil {
		t.Fatal(err)
	}
	cookies := reloaded.Cookies(mustURL(t, "https://connect.raspberrypi.com/devices"))
	if len(cookies) != 1 || cookies[0].Value != secret {
		t.Fatalf("cookies were not restored: %+v", cookies)
	}
}

// Cookie files written by earlier versions are plain JSON and still load.
func TestPlainCookieFileFromEarlierVersionStillLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.json")
	legacy := `{"connect.raspberrypi.com":[{"name":"connect_session","value":"legacy-value","path":"/","domain":"connect.raspberrypi.com","expires":"0001-01-01T00:00:00Z","secure":true,"http_only":true}]}`
	if err := os.WriteFile(path, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	jar, err := NewFileCookieJar(path)
	if err != nil {
		t.Fatal(err)
	}
	cookies := jar.Cookies(mustURL(t, "https://connect.raspberrypi.com/devices"))
	if len(cookies) != 1 || cookies[0].Value != "legacy-value" {
		t.Fatalf("legacy cookie file was not loaded: %+v", cookies)
	}
}

func TestUnreadableCookieFileStartsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.json")
	if err := os.WriteFile(path, []byte(`{"protected":"dpapi","data":"bm90IGEgcmVhbCBibG9i"}`), 0600); err != nil {
		t.Fatal(err)
	}
	jar, err := NewFileCookieJar(path)
	if err != nil {
		t.Fatalf("a damaged cookie file must not stop the server: %v", err)
	}
	if got := jar.Cookies(mustURL(t, "https://connect.raspberrypi.com/")); len(got) != 0 {
		t.Fatalf("expected no cookies, got %+v", got)
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
