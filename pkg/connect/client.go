package connect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"rpi-ssh-go/pkg/auth"
)

var (
	reRow    = regexp.MustCompile(`(?s)<tr\b[^>]*>(.*?)</tr>`)
	reLink   = regexp.MustCompile(`(?s)<a\b[^>]*href=["']/devices/([A-Za-z0-9\-]+)["'][^>]*>(.*?)</a>`)
	reVer    = regexp.MustCompile(`\b\d+\.\d+\.\d+\b`)
	reOnline = regexp.MustCompile(`\bOnline\b`)
	reICE    = regexp.MustCompile(`data-shell-ice-configuration-value=["']([^"']+)["']`)

	// reSVG and reTag reduce markup to the text a person sees. Icons go
	// first: their path data is full of digits that look like versions.
	reSVG = regexp.MustCompile(`(?s)<svg\b.*?</svg>`)
	reTag = regexp.MustCompile(`(?s)<(?:[^>"']|"[^"]*"|'[^']*')*>`)

	// reDeviceLink matches a link to a device anywhere on the page.
	reDeviceLink = regexp.MustCompile(`href=["']/devices/[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}["']`)
)

// ErrDevicePageNotRecognised reports that the devices page links to devices
// but none could be read from it, which means its markup has changed.
var ErrDevicePageNotRecognised = errors.New("devices page markup not recognised; the Raspberry Pi Connect page layout may have changed")

// pollFailureLimit is how many consecutive error responses PollAnswer accepts
// before it stops waiting for an answer.
const pollFailureLimit = 5

// authFreshness is how long a successful authentication check is trusted
// before the session is probed again. It removes back-to-back probes of the
// devices page during a single connect.
const authFreshness = 10 * time.Second

type authCall struct {
	done chan struct{}
	err  error
}

type Client struct {
	httpClient  *http.Client
	cookieJar   *auth.FileCookieJar
	email       string
	password    string
	deviceCache []DeviceInfo
	cacheTime   time.Time
	cacheGen    uint64    // incremented each time deviceCache is replaced
	authOKAt    time.Time // last successful authentication check
	baseURL     string
	mu          sync.Mutex

	authMu     sync.Mutex
	activeAuth *authCall
}

func NewClient() (*Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	cookiePath := filepath.Join(home, ".rpi-connect-cookies.json")
	jar, err := auth.NewFileCookieJar(cookiePath)
	if err != nil {
		return nil, fmt.Errorf("cookie jar creation failed: %w", err)
	}

	email, password := auth.LoadCredentials()

	return &Client{
		httpClient: &http.Client{Jar: jar},
		cookieJar:  jar,
		email:      email,
		password:   password,
	}, nil
}

func (c *Client) SetBaseURL(u string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.baseURL = u
}

func (c *Client) getBaseURL() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.baseURL != "" {
		return c.baseURL
	}
	return "https://connect.raspberrypi.com"
}

func (c *Client) SaveCookies() {
	if c.cookieJar != nil {
		_ = c.cookieJar.Save()
	}
}

func truncateStr(s string, max int) string {
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

// htmlText returns the visible text of an HTML fragment: icons and tags
// removed, entities decoded, whitespace collapsed.
func htmlText(fragment string) string {
	text := reTag.ReplaceAllString(reSVG.ReplaceAllString(fragment, " "), " ")
	return strings.Join(strings.Fields(html.UnescapeString(text)), " ")
}

// parseDevicesPage reads the device table of the Connect devices page. A page
// without devices yields an empty list; a page that links to devices this
// parser cannot read yields ErrDevicePageNotRecognised.
func parseDevicesPage(htmlBody string) ([]DeviceInfo, error) {
	rows := reRow.FindAllStringSubmatch(htmlBody, -1)
	var devices []DeviceInfo
	for _, r := range rows {
		rowContent := r[1]
		linkMatch := reLink.FindStringSubmatch(rowContent)
		if len(linkMatch) < 3 {
			continue
		}

		// Status, capabilities and version are read from the text beside
		// the name, so a device called "Online" does not count as online.
		details := htmlText(strings.Replace(rowContent, linkMatch[0], " ", 1))

		devices = append(devices, DeviceInfo{
			ID:      linkMatch[1],
			Name:    htmlText(linkMatch[2]),
			Online:  reOnline.MatchString(details),
			Shell:   strings.Contains(details, "Remote shell"),
			Version: reVer.FindString(details),
		})
	}
	if len(devices) == 0 && reDeviceLink.MatchString(htmlBody) {
		return nil, ErrDevicePageNotRecognised
	}
	return devices, nil
}

// EnsureAuthenticated verifies the session, logging in again if needed. A
// check that succeeded within authFreshness is not repeated.
func (c *Client) EnsureAuthenticated(ctx context.Context) error {
	return c.ensureAuthenticated(ctx, false)
}

func (c *Client) ensureAuthenticated(ctx context.Context, force bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if !force {
		c.mu.Lock()
		fresh := !c.authOKAt.IsZero() && time.Since(c.authOKAt) < authFreshness
		c.mu.Unlock()
		if fresh {
			return nil
		}
	}

	c.authMu.Lock()
	if c.activeAuth != nil {
		call := c.activeAuth
		c.authMu.Unlock()

		select {
		case <-call.done:
			return call.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	call := &authCall{done: make(chan struct{})}
	c.activeAuth = call
	c.authMu.Unlock()

	errCh := make(chan error, 1)
	go func() {
		var err error
		defer func() {
			c.authMu.Lock()
			c.activeAuth = nil
			c.authMu.Unlock()
			call.err = err
			close(call.done)
		}()

		authCtx, authCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer authCancel()

		var devicesBody []byte
		devicesBody, err = auth.EnsureAuthenticatedWithBody(authCtx, c.httpClient, c.getBaseURL(), c.email, c.password)
		if err == nil {
			c.SaveCookies()
			// A body that cannot be parsed is not cached; ListDevices then
			// fetches the page itself and reports the problem.
			var devices []DeviceInfo
			parsed := false
			if len(devicesBody) > 0 {
				var parseErr error
				devices, parseErr = parseDevicesPage(string(devicesBody))
				parsed = parseErr == nil
			}
			c.mu.Lock()
			c.authOKAt = time.Now()
			if parsed {
				c.deviceCache = devices
				c.cacheTime = c.authOKAt
				c.cacheGen++
			}
			c.mu.Unlock()
		}
		errCh <- err
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) ListDevices(ctx context.Context, forceRefresh bool) ([]DeviceInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	c.mu.Lock()
	if !forceRefresh && !c.cacheTime.IsZero() && time.Since(c.cacheTime) < 30*time.Second {
		cached := make([]DeviceInfo, len(c.deviceCache))
		copy(cached, c.deviceCache)
		c.mu.Unlock()
		return cached, nil
	}
	genBefore := c.cacheGen
	c.mu.Unlock()

	if err := c.EnsureAuthenticated(ctx); err != nil {
		return nil, err
	}

	// The authentication check fetches the devices page itself. If it ran
	// during this call the list is already live, even for a forced refresh.
	// A counter tells, not timestamps: two clock readings this close
	// together are often equal.
	c.mu.Lock()
	if c.cacheGen != genBefore || (!forceRefresh && !c.cacheTime.IsZero() && time.Since(c.cacheTime) < 30*time.Second) {
		cached := make([]DeviceInfo, len(c.deviceCache))
		copy(cached, c.deviceCache)
		c.mu.Unlock()
		return cached, nil
	}
	c.mu.Unlock()

	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, "GET", c.getBaseURL()+"/devices", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create devices request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/130.0.0.0 Safari/537.36")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	b, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read devices response error: %w", readErr)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("devices request returned HTTP %d", resp.StatusCode)
	}
	if resp.Request == nil || resp.Request.URL == nil || resp.Request.URL.Path != req.URL.Path || resp.Request.URL.Host != req.URL.Host {
		return nil, fmt.Errorf("devices request redirected away from authenticated device page")
	}

	devices, err := parseDevicesPage(string(b))
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.deviceCache = make([]DeviceInfo, len(devices))
	copy(c.deviceCache, devices)
	c.cacheTime = time.Now()
	c.cacheGen++
	c.mu.Unlock()

	c.SaveCookies()

	result := make([]DeviceInfo, len(devices))
	copy(result, devices)
	return result, nil
}

func (c *Client) GetShellSessionInfo(ctx context.Context, deviceID string) (ICEConfig, string, error) {
	if err := c.EnsureAuthenticated(ctx); err != nil {
		return ICEConfig{}, "", err
	}

	baseURL := c.getBaseURL()
	shellURL := fmt.Sprintf("%s/devices/%s/remote-shell-session", baseURL, deviceID)

	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	req, err := http.NewRequestWithContext(reqCtx, "GET", shellURL, nil)
	if err != nil {
		cancel()
		return ICEConfig{}, "", fmt.Errorf("failed to create shell request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/130.0.0.0 Safari/537.36")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		cancel()
		return ICEConfig{}, "", fmt.Errorf("fetch shell page error: %w", err)
	}
	b, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	cancel()
	if readErr != nil {
		return ICEConfig{}, "", fmt.Errorf("read shell page error: %w", readErr)
	}

	htmlBody := string(b)
	shellCSRF := auth.ExtractCSRF(htmlBody)
	iceMatch := reICE.FindStringSubmatch(htmlBody)

	// If ICE config is missing, check if Raspberry Pi Connect served a re-authentication form
	if len(iceMatch) < 2 && (strings.Contains(htmlBody, `name="reauth"`) || strings.Contains(htmlBody, `action="/auth/raspberry_pi"`)) {
		reauthCSRF := auth.ExtractCSRF(htmlBody)
		form := url.Values{}
		form.Set("authenticity_token", reauthCSRF)
		form.Set("origin", fmt.Sprintf("/devices/%s/remote-shell-session", deviceID))

		postCtx, postCancel := context.WithTimeout(ctx, 15*time.Second)
		postReq, postReqErr := http.NewRequestWithContext(postCtx, "POST", baseURL+"/auth/raspberry_pi", strings.NewReader(form.Encode()))
		if postReqErr == nil {
			postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			postReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/130.0.0.0 Safari/537.36")
			postReq.Header.Set("Origin", baseURL)
			postReq.Header.Set("Referer", shellURL)

			postResp, postErr := c.httpClient.Do(postReq)
			if postErr == nil {
				bPost, _ := io.ReadAll(postResp.Body)
				postResp.Body.Close()
				htmlBody = string(bPost)
				shellCSRF = auth.ExtractCSRF(htmlBody)
				iceMatch = reICE.FindStringSubmatch(htmlBody)
			}
		}
		postCancel()
	}

	// If still missing, attempt full re-login and retry fetch once
	if len(iceMatch) < 2 && c.email != "" && c.password != "" {
		if err := c.ensureAuthenticated(ctx, true); err == nil {
			req2Ctx, req2Cancel := context.WithTimeout(ctx, 15*time.Second)
			req2, req2Err := http.NewRequestWithContext(req2Ctx, "GET", shellURL, nil)
			if req2Err == nil {
				req2.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/130.0.0.0 Safari/537.36")
				if resp2, err2 := c.httpClient.Do(req2); err2 == nil {
					b2, readErr := io.ReadAll(resp2.Body)
					resp2.Body.Close()
					if readErr == nil {
						htmlBody = string(b2)
						shellCSRF = auth.ExtractCSRF(htmlBody)
						iceMatch = reICE.FindStringSubmatch(htmlBody)
					}
				}
			}
			req2Cancel()
		}
	}

	if len(iceMatch) < 2 {
		return ICEConfig{}, "", fmt.Errorf("ICE configuration not found on shell page (status: %d, url: %s, body prefix: %q)", resp.StatusCode, resp.Request.URL.String(), truncateStr(htmlBody, 200))
	}

	var iceCfg ICEConfig
	if err := json.Unmarshal([]byte(html.UnescapeString(iceMatch[1])), &iceCfg); err != nil {
		return ICEConfig{}, "", fmt.Errorf("parse ICE config error: %w", err)
	}

	c.SaveCookies()
	return iceCfg, shellCSRF, nil
}

func (c *Client) PostOffer(ctx context.Context, deviceID, sdp, csrfToken, referer string) (string, error) {
	offerPayload, _ := json.Marshal(map[string]string{
		"type": "offer",
		"sdp":  sdp,
	})

	baseURL := c.getBaseURL()
	connEndpoint := fmt.Sprintf("%s/devices/%s/connections", baseURL, deviceID)

	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	reqOffer, err := http.NewRequestWithContext(reqCtx, "POST", connEndpoint, bytes.NewReader(offerPayload))
	if err != nil {
		return "", fmt.Errorf("failed to create offer request: %w", err)
	}
	// Must match the browser client: Connect rejects non-JSON offers with 400.
	reqOffer.Header.Set("Content-Type", "application/json")
	reqOffer.Header.Set("Accept", "application/json")
	reqOffer.Header.Set("X-CSRF-Token", csrfToken)
	reqOffer.Header.Set("Referer", referer)
	reqOffer.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/130.0.0.0 Safari/537.36")

	resp, err := c.httpClient.Do(reqOffer)
	if err != nil {
		return "", err
	}
	b, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		return "", fmt.Errorf("read offer response error: %w", readErr)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		// A rejected offer can never yield an answer; fail now instead of
		// polling a bogus connection until the answer timeout.
		return "", fmt.Errorf("offer rejected by Raspberry Pi Connect (status %d)", resp.StatusCode)
	}

	var connResp struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(b, &connResp)
	connID := connResp.ID
	if connID == "" {
		loc := resp.Header.Get("Location")
		parts := strings.Split(loc, "/")
		connID = parts[len(parts)-1]
	}

	if connID == "" {
		return "", fmt.Errorf("failed to retrieve connection ID from response (status %d)", resp.StatusCode)
	}

	c.SaveCookies()
	return connID, nil
}

func (c *Client) PollAnswer(ctx context.Context, deviceID, connID, referer string, maxDuration time.Duration) (string, error) {
	baseURL := c.getBaseURL()
	pollURL := fmt.Sprintf("%s/devices/%s/connections/%s", baseURL, deviceID, connID)

	pollCtx := ctx
	if maxDuration > 0 {
		var cancel context.CancelFunc
		pollCtx, cancel = context.WithTimeout(ctx, maxDuration)
		defer cancel()
	}

	first := true
	failures := 0 // consecutive polls answered with an error status
	for {
		if !first {
			select {
			case <-pollCtx.Done():
				return "", fmt.Errorf("timeout waiting for SDP answer from Raspberry Pi: %w", pollCtx.Err())
			case <-time.After(250 * time.Millisecond):
			}
		}
		first = false

		if err := pollCtx.Err(); err != nil {
			return "", fmt.Errorf("timeout waiting for SDP answer from Raspberry Pi: %w", err)
		}

		reqCtx, reqCancel := context.WithTimeout(pollCtx, 15*time.Second)
		reqPoll, err := http.NewRequestWithContext(reqCtx, "GET", pollURL, nil)
		if err != nil {
			reqCancel()
			return "", err
		}
		reqPoll.Header.Set("Accept", "application/json")
		reqPoll.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/130.0.0.0 Safari/537.36")
		reqPoll.Header.Set("Referer", referer)

		respPoll, err := c.httpClient.Do(reqPoll)
		if err != nil {
			reqCancel()
			if pollCtx.Err() != nil {
				return "", fmt.Errorf("timeout waiting for SDP answer from Raspberry Pi: %w", pollCtx.Err())
			}
			continue
		}

		pollBytes, readErr := io.ReadAll(respPoll.Body)
		respPoll.Body.Close()
		reqCancel()

		if respPoll.StatusCode == http.StatusUnauthorized || respPoll.StatusCode == http.StatusForbidden {
			// The session is not accepted; further polling cannot succeed.
			return "", fmt.Errorf("SDP answer poll rejected by Raspberry Pi Connect (status %d)", respPoll.StatusCode)
		}

		// Ending up on another page (the sign-in form) means the login is
		// gone; no answer will come from there.
		if respPoll.Request != nil && respPoll.Request.URL != nil && respPoll.Request.URL.Path != reqPoll.URL.Path {
			return "", fmt.Errorf("SDP answer poll was redirected to %s; the Raspberry Pi Connect login has probably expired", respPoll.Request.URL.Path)
		}

		// One failed poll can be a hiccup. A run of them means the
		// connection is not going to produce an answer.
		if respPoll.StatusCode != http.StatusOK {
			failures++
			if failures >= pollFailureLimit {
				return "", fmt.Errorf("SDP answer poll failed %d times in a row (status %d)", failures, respPoll.StatusCode)
			}
			continue
		}
		failures = 0

		if readErr == nil {
			var pollResult ConnectionResponse
			if err := json.Unmarshal(pollBytes, &pollResult); err == nil && pollResult.Answer != nil {
				c.SaveCookies()
				return pollResult.Answer.SDP, nil
			}
		}
	}
}
