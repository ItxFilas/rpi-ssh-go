package auth

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
)

var (
	ErrCSRFNotFound         = errors.New("csrf token not found in response")
	ErrSessionExpired       = errors.New("session expired")
	ErrAuthenticationFailed = errors.New("authentication failed")
)

var (
	reMetaNameContent = regexp.MustCompile(`(?i)<meta[^>]+name\s*=\s*["']csrf-token["'][^>]*content\s*=\s*["']([^"']+)["']`)
	reMetaContentName = regexp.MustCompile(`(?i)<meta[^>]+content\s*=\s*["']([^"']+)["'][^>]*name\s*=\s*["']csrf-token["']`)
	reInputNameValue  = regexp.MustCompile(`(?i)<input[^>]+name\s*=\s*["'](?:authenticity_token|csrf_token|_csrf_token)["'][^>]*value\s*=\s*["']([^"']+)["']`)
	reInputValueName  = regexp.MustCompile(`(?i)<input[^>]+value\s*=\s*["']([^"']+)["'][^>]*name\s*=\s*["'](?:authenticity_token|csrf_token|_csrf_token)["']`)
)

// ZeroBytes overwrites the byte slice with zeros to minimize credential retention in memory.
func ZeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// CSRFManager provides thread-safe storage and extraction of CSRF tokens.
type CSRFManager struct {
	mu    sync.RWMutex
	token string
}

func NewCSRFManager() *CSRFManager {
	return &CSRFManager{}
}

func (m *CSRFManager) Token() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.token
}

func (m *CSRFManager) SetToken(token string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.token = token
}

func (m *CSRFManager) ExtractAndSet(htmlBody string) (string, error) {
	token := ExtractCSRF(htmlBody)
	if token == "" {
		return "", ErrCSRFNotFound
	}
	m.SetToken(token)
	return token, nil
}

func (m *CSRFManager) Clear() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.token = ""
}

// ExtractCSRF extracts a CSRF or authenticity token from an HTML body.
// It searches for meta tags and input tags with various attribute orderings and quoting styles.
func ExtractCSRF(body string) string {
	if m := reMetaNameContent.FindStringSubmatch(body); len(m) > 1 {
		return m[1]
	}
	if m := reMetaContentName.FindStringSubmatch(body); len(m) > 1 {
		return m[1]
	}
	if m := reInputNameValue.FindStringSubmatch(body); len(m) > 1 {
		return m[1]
	}
	if m := reInputValueName.FindStringSubmatch(body); len(m) > 1 {
		return m[1]
	}
	return ""
}

// ExtractCSRFWithError extracts a CSRF token and returns ErrCSRFNotFound if not found.
func ExtractCSRFWithError(body string) (string, error) {
	token := ExtractCSRF(body)
	if token == "" {
		return "", ErrCSRFNotFound
	}
	return token, nil
}

func cleanEnvValue(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
	}
	return strings.TrimSpace(v)
}

// maxEnvFileBytes bounds how much of a credentials file is read.
const maxEnvFileBytes = 1 << 20

// decodeEnvFile returns the text of a .env file whatever a Windows editor or
// shell redirect made of it: UTF-8 with or without a byte order mark, or
// UTF-16 in either byte order.
func decodeEnvFile(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte{0xEF, 0xBB, 0xBF}):
		return string(data[3:])
	case bytes.HasPrefix(data, []byte{0xFF, 0xFE}):
		return decodeUTF16(data[2:], binary.LittleEndian)
	case bytes.HasPrefix(data, []byte{0xFE, 0xFF}):
		return decodeUTF16(data[2:], binary.BigEndian)
	}
	return string(data)
}

func decodeUTF16(data []byte, order binary.ByteOrder) string {
	units := make([]uint16, 0, len(data)/2)
	for i := 0; i+1 < len(data); i += 2 {
		units = append(units, order.Uint16(data[i:]))
	}
	return string(utf16.Decode(units))
}

// LoadCredentialsFromReader parses environment-style key=value pairs from an io.Reader.
// Lines may carry a leading "export".
func LoadCredentialsFromReader(r io.Reader) (string, string) {
	var email, password string
	data, _ := io.ReadAll(io.LimitReader(r, maxEnvFileBytes))
	scanner := bufio.NewScanner(strings.NewReader(decodeEnvFile(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "export"); ok && rest != strings.TrimLeft(rest, " \t") {
			line = strings.TrimSpace(rest)
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := cleanEnvValue(parts[1])
		switch key {
		case "RPI_EMAIL":
			email = val
		case "RPI_PASSWORD":
			password = val
		}
	}
	return email, password
}

// LoadCredentials returns the Raspberry Pi ID email and password. RPI_EMAIL
// and RPI_PASSWORD in the environment win when both are set, since that is
// where an MCP client configures this server; otherwise the first .env file
// holding both is used.
func LoadCredentials() (string, string) {
	return loadCredentials(credentialPaths(), os.Getenv)
}

// credentialPaths lists the .env files searched, in order: next to the
// executable, in the home directory, and in the working directory.
func credentialPaths() []string {
	var paths []string
	if exePath, err := os.Executable(); err == nil {
		paths = append(paths, filepath.Join(filepath.Dir(exePath), ".env"))
	}
	if homeDir, _ := os.UserHomeDir(); homeDir != "" {
		paths = append(paths, filepath.Join(homeDir, ".rpi-connect.env"), filepath.Join(homeDir, ".env"))
	}
	return append(paths, ".env")
}

func loadCredentials(paths []string, getenv func(string) string) (string, string) {
	envEmail, envPassword := getenv("RPI_EMAIL"), getenv("RPI_PASSWORD")
	if envEmail != "" && envPassword != "" {
		return envEmail, envPassword
	}

	for _, p := range paths {
		if f, err := os.Open(p); err == nil {
			email, password := LoadCredentialsFromReader(f)
			_ = f.Close()
			if email != "" && password != "" {
				return email, password
			}
		}
	}
	return envEmail, envPassword
}

// EnsureAuthenticated ensures that the client possesses a valid authenticated session for connect.raspberrypi.com.
// If cookies are absent, expired, or near expiry (< 5 minutes), it auto-renews authentication.
func EnsureAuthenticated(ctx context.Context, client *http.Client, email, password string) error {
	_, err := EnsureAuthenticatedWithBody(ctx, client, "https://connect.raspberrypi.com", email, password)
	return err
}

// EnsureAuthenticatedWithBase ensures valid session for a custom base URL.
func EnsureAuthenticatedWithBase(ctx context.Context, client *http.Client, connectBase, email, password string) error {
	_, err := EnsureAuthenticatedWithBody(ctx, client, connectBase, email, password)
	return err
}

// EnsureAuthenticatedWithBody ensures authentication and returns the devices page body if retrieved during probe or login.
func EnsureAuthenticatedWithBody(ctx context.Context, client *http.Client, connectBase, email, password string) ([]byte, error) {
	u, err := url.Parse(connectBase)
	host := connectBase
	if err == nil && u.Host != "" {
		host = u.Host
	}

	// 1. In-memory cookie expiration pre-check (5-minute safety buffer)
	if jar, ok := client.Jar.(*FileCookieJar); ok {
		if !jar.HasValidSession(host, 5*time.Minute) {
			if email == "" || password == "" {
				return nil, fmt.Errorf("%w and RPI_EMAIL/RPI_PASSWORD not configured", ErrSessionExpired)
			}
			// Pre-check triggered auto-renewal: skip probe and immediately re-authenticate
			return performLogin(ctx, client, connectBase, email, password, nil)
		}
	}

	// 2. Probe devices endpoint to verify existing session is accepted by server
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	probeReq, err := http.NewRequestWithContext(probeCtx, "GET", connectBase+"/devices", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create probe request: %w", err)
	}
	probeReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/130.0.0.0 Safari/537.36")

	resp, err := client.Do(probeReq)
	if err != nil {
		return nil, fmt.Errorf("probe request failed: %w", err)
	}
	bodyBytes, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		return nil, fmt.Errorf("probe read response failed: %w", readErr)
	}

	if strings.Contains(resp.Request.URL.String(), "/devices") && resp.StatusCode == 200 {
		return bodyBytes, nil // Active session confirmed
	}

	if email == "" || password == "" {
		return nil, fmt.Errorf("%w and RPI_EMAIL/RPI_PASSWORD not configured", ErrSessionExpired)
	}

	// Probe indicated unauthenticated / redirected, continue to login
	return performLogin(ctx, client, connectBase, email, password, bodyBytes)
}

func performLogin(ctx context.Context, client *http.Client, connectBase, email, password string, initialBody []byte) ([]byte, error) {
	bodyBytes := initialBody
	if len(bodyBytes) == 0 {
		// Fetch sign-in or devices page to extract initial CSRF
		signCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		req, err := http.NewRequestWithContext(signCtx, "GET", connectBase+"/sign-in", nil)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("failed to create sign-in request: %w", err)
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/130.0.0.0 Safari/537.36")
		resp, err := client.Do(req)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("sign-in fetch failed: %w", err)
		}
		var readErr error
		bodyBytes, readErr = io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		if readErr != nil {
			return nil, fmt.Errorf("read sign-in response failed: %w", readErr)
		}

		if resp.StatusCode == http.StatusNotFound {
			// Fallback to /devices probe if /sign-in returned 404
			devCtx, devCancel := context.WithTimeout(ctx, 15*time.Second)
			reqDev, errDevReq := http.NewRequestWithContext(devCtx, "GET", connectBase+"/devices", nil)
			if errDevReq == nil {
				reqDev.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/130.0.0.0 Safari/537.36")
				respDev, errDev := client.Do(reqDev)
				if errDev == nil {
					var readFallbackErr error
					bodyBytes, readFallbackErr = io.ReadAll(respDev.Body)
					respDev.Body.Close()
					if readFallbackErr != nil {
						devCancel()
						return nil, fmt.Errorf("read fallback devices response failed: %w", readFallbackErr)
					}
				}
			}
			devCancel()
		}
	}

	// 1. Extract initial CSRF token
	csrf := ExtractCSRF(string(bodyBytes))
	if csrf == "" {
		return nil, fmt.Errorf("initial %w", ErrCSRFNotFound)
	}

	// 2. Post to /auth/raspberry_pi
	authForm := url.Values{}
	authForm.Set("authenticity_token", csrf)
	authCtx, authCancel := context.WithTimeout(ctx, 15*time.Second)
	reqAuth, err := http.NewRequestWithContext(authCtx, "POST", connectBase+"/auth/raspberry_pi", strings.NewReader(authForm.Encode()))
	if err != nil {
		authCancel()
		return nil, fmt.Errorf("failed to create auth request: %w", err)
	}
	reqAuth.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqAuth.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/130.0.0.0 Safari/537.36")

	resp, err := client.Do(reqAuth)
	if err != nil {
		authCancel()
		return nil, fmt.Errorf("auth post failed: %w", err)
	}
	bodyBytes, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	authCancel()
	if readErr != nil {
		return nil, fmt.Errorf("read auth response failed: %w", readErr)
	}

	// 3. Extract ID CSRF token
	idCSRF := ExtractCSRF(string(bodyBytes))
	if idCSRF == "" {
		return nil, fmt.Errorf("id provider %w", ErrCSRFNotFound)
	}

	idBase := "https://id.raspberrypi.com"
	if !strings.Contains(connectBase, "raspberrypi.com") {
		idBase = connectBase
	}

	// 4. Post credentials to id session endpoint with memory hygiene
	loginForm := url.Values{}
	loginForm.Set("authenticity_token", idCSRF)
	loginForm.Set("email", email)
	loginForm.Set("password", password)
	loginForm.Set("commit", "Sign in")

	encoded := loginForm.Encode()
	formBytes := []byte(encoded)
	defer ZeroBytes(formBytes)

	loginCtx, loginCancel := context.WithTimeout(ctx, 15*time.Second)
	reqLogin, err := http.NewRequestWithContext(loginCtx, "POST", idBase+"/session", bytes.NewReader(formBytes))
	if err != nil {
		loginCancel()
		return nil, fmt.Errorf("failed to create login request: %w", err)
	}
	reqLogin.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqLogin.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/130.0.0.0 Safari/537.36")
	reqLogin.Header.Set("Origin", idBase)
	reqLogin.Header.Set("Referer", idBase+"/sign-in")

	resp, err = client.Do(reqLogin)
	if err != nil {
		loginCancel()
		return nil, fmt.Errorf("login submission failed: %w", err)
	}
	finalBody, readLoginErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	loginCancel()
	if readLoginErr != nil {
		return nil, fmt.Errorf("read login response failed: %w", readLoginErr)
	}

	// 5. Verify successful authentication redirect
	if !strings.Contains(resp.Request.URL.Path, "devices") && !strings.Contains(resp.Request.URL.String(), "connect.raspberrypi.com/devices") {
		// Strip query parameters to avoid leaking tokens or sensitive strings
		sanitizedURL := resp.Request.URL.Scheme + "://" + resp.Request.URL.Host + resp.Request.URL.Path
		return nil, loginFailure(resp.Request.URL.Path, finalBody, sanitizedURL)
	}

	return finalBody, nil
}

// A page that asks for a second factor is recognised by its address or by
// the code field on it. The markers are deliberately specific: short ones
// such as "otp" or "2fa" also occur in asset file names.
var (
	reTwoFactorPath  = regexp.MustCompile(`(?i)(^|[/_-])(two[-_]?factor|2fa|mfa|otp|totp)([/_-]|$)`)
	twoFactorMarkers = []string{`autocomplete="one-time-code"`, "otp_attempt", "two-factor authentication", "authenticator app"}
)

// loginFailure explains why signing in did not end on the devices page. The
// sign-in service does not say why in a structured way, so the page the
// login ended on is read for the two common reasons.
func loginFailure(finalPath string, finalBody []byte, sanitizedURL string) error {
	twoFactor := reTwoFactorPath.MatchString(finalPath)
	body := strings.ToLower(string(finalBody))
	for _, marker := range twoFactorMarkers {
		twoFactor = twoFactor || strings.Contains(body, marker)
	}
	if twoFactor {
		return fmt.Errorf("%w: this Raspberry Pi ID uses two-factor authentication, which this server cannot complete (login stopped at %s)", ErrAuthenticationFailed, sanitizedURL)
	}
	return fmt.Errorf("%w: the sign-in was not accepted, check RPI_EMAIL and RPI_PASSWORD (login ended at %s)", ErrAuthenticationFailed, sanitizedURL)
}
