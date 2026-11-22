package connect

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"rpi-ssh-go/pkg/auth"
)

// newCountingClient serves a devices page and a shell page, counting how often
// the devices page is fetched.
func newCountingClient(t *testing.T) (*Client, *int32) {
	t.Helper()
	var deviceFetches int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		switch r.URL.Path {
		case "/devices":
			atomic.AddInt32(&deviceFetches, 1)
			_, _ = fmt.Fprint(w, `<table><tr><td><a href="/devices/abc-123">pi-one</a> Online Remote shell</td></tr></table>`)
		case "/devices/abc-123/remote-shell-session":
			_, _ = fmt.Fprint(w, `<div data-shell-ice-configuration-value="{}"></div>`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	jar, err := auth.NewFileCookieJar(filepath.Join(t.TempDir(), "cookies.json"))
	if err != nil {
		t.Fatalf("failed to create cookie jar: %v", err)
	}
	serverURL, _ := url.Parse(server.URL)
	jar.SetCookies(serverURL, []*http.Cookie{{
		Name:    "connect_session",
		Value:   "valid_token",
		Path:    "/",
		Expires: time.Now().Add(time.Hour),
	}})

	return &Client{
		httpClient: &http.Client{Jar: jar},
		cookieJar:  jar,
		baseURL:    server.URL,
	}, &deviceFetches
}

func TestColdConnectProbesDevicesPageOnce(t *testing.T) {
	client, deviceFetches := newCountingClient(t)
	ctx := context.Background()

	devices, err := client.ListDevices(ctx, false)
	if err != nil {
		t.Fatalf("ListDevices failed: %v", err)
	}
	if len(devices) != 1 || devices[0].ID != "abc-123" {
		t.Fatalf("unexpected devices: %+v", devices)
	}
	if _, _, err := client.GetShellSessionInfo(ctx, "abc-123"); err != nil {
		t.Fatalf("GetShellSessionInfo failed: %v", err)
	}
	if got := atomic.LoadInt32(deviceFetches); got != 1 {
		t.Fatalf("expected the devices page to be fetched once during a cold connect, got %d", got)
	}
}

func TestForcedRefreshReusesAuthenticationProbe(t *testing.T) {
	client, deviceFetches := newCountingClient(t)
	ctx := context.Background()

	// The authentication probe of a forced refresh is itself a live fetch.
	if _, err := client.ListDevices(ctx, true); err != nil {
		t.Fatalf("forced ListDevices failed: %v", err)
	}
	if got := atomic.LoadInt32(deviceFetches); got != 1 {
		t.Fatalf("expected one devices fetch for a forced refresh, got %d", got)
	}

	// A second forced refresh must still go to the network.
	if _, err := client.ListDevices(ctx, true); err != nil {
		t.Fatalf("second forced ListDevices failed: %v", err)
	}
	if got := atomic.LoadInt32(deviceFetches); got != 2 {
		t.Fatalf("expected a second forced refresh to fetch live data once, got %d fetches in total", got)
	}
}

func statusClient(status int, calls *int32) *Client {
	return &Client{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				atomic.AddInt32(calls, 1)
				return &http.Response{
					StatusCode: status,
					Header:     http.Header{"Location": []string{"/sign-in"}},
					Body:       &trackingCloser{Reader: strings.NewReader(`{"id":"bogus"}`)},
					Request:    req,
				}, nil
			}),
		},
	}
}

func TestPostOfferRejectsErrorStatus(t *testing.T) {
	var calls int32
	client := statusClient(http.StatusForbidden, &calls)

	connID, err := client.PostOffer(context.Background(), "dev-1", "sdp", "csrf", "https://example.com")
	if err == nil {
		t.Fatalf("expected an error for a rejected offer, got connection ID %q", connID)
	}
	if !strings.Contains(err.Error(), "403") {
		t.Fatalf("expected the status code in the error, got: %v", err)
	}
}

// Raspberry Pi Connect answers 400 Bad Request to offers that are not sent
// as JSON (observed 2026-11-20); the browser client posts application/json.
func TestPostOfferSendsJSONContentType(t *testing.T) {
	client := &Client{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				status := http.StatusCreated
				if req.Header.Get("Content-Type") != "application/json" || req.Header.Get("Accept") != "application/json" {
					status = http.StatusBadRequest
				}
				return &http.Response{
					StatusCode: status,
					Header:     http.Header{"Location": []string{"/devices/dev-1/connections/conn-1"}},
					Body:       &trackingCloser{Reader: strings.NewReader("")},
					Request:    req,
				}, nil
			}),
		},
	}

	connID, err := client.PostOffer(context.Background(), "dev-1", "sdp", "csrf", "https://example.com")
	if err != nil {
		t.Fatalf("expected the JSON offer to be accepted, got: %v", err)
	}
	if connID != "conn-1" {
		t.Fatalf("expected connection ID from Location header, got %q", connID)
	}
}

func TestPollAnswerFailsFastWhenUnauthorized(t *testing.T) {
	var calls int32
	client := statusClient(http.StatusUnauthorized, &calls)

	start := time.Now()
	_, err := client.PollAnswer(context.Background(), "dev-1", "conn-1", "https://example.com", 5*time.Second)
	if err == nil {
		t.Fatal("expected an error for an unauthorized poll")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("unauthorized poll took %v instead of failing fast", elapsed)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected a single poll request, got %d", got)
	}
}

// A connection the service keeps answering with an error for will never
// produce an answer; waiting out the whole timeout only delays the report.
func TestPollAnswerGivesUpOnRepeatedFailures(t *testing.T) {
	var calls int32
	client := statusClient(http.StatusNotFound, &calls)
	client.httpClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Header:     make(http.Header),
			Body:       &trackingCloser{Reader: strings.NewReader("not found")},
			Request:    req,
		}, nil
	})

	start := time.Now()
	_, err := client.PollAnswer(context.Background(), "dev-1", "conn-1", "https://example.com", 10*time.Second)
	if err == nil {
		t.Fatal("expected an error")
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("polling continued for %v although every response was an error", elapsed)
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("expected the status code in the error, got: %v", err)
	}
}

func TestPollAnswerSurvivesTransientFailures(t *testing.T) {
	var calls int32
	client := &Client{httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		status, body := http.StatusOK, `{"id":"conn-1","answer":{"type":"answer","sdp":"v=0"}}`
		if atomic.AddInt32(&calls, 1) <= 2 {
			status, body = http.StatusBadGateway, "bad gateway"
		}
		return &http.Response{
			StatusCode: status,
			Header:     make(http.Header),
			Body:       &trackingCloser{Reader: strings.NewReader(body)},
			Request:    req,
		}, nil
	})}}

	sdp, err := client.PollAnswer(context.Background(), "dev-1", "conn-1", "https://example.com", 10*time.Second)
	if err != nil {
		t.Fatalf("two failed polls should be retried, got: %v", err)
	}
	if sdp != "v=0" {
		t.Fatalf("unexpected answer %q", sdp)
	}
}

// A poll that ends up on another page (the sign-in form) means the login is
// gone. No answer will ever come from there.
func TestPollAnswerFailsWhenRedirectedAway(t *testing.T) {
	var calls int32
	client := &Client{httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		landed := req.Clone(req.Context())
		u := *req.URL
		u.Path = "/sign-in"
		landed.URL = &u
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       &trackingCloser{Reader: strings.NewReader("<html>Sign in</html>")},
			Request:    landed,
		}, nil
	})}}

	start := time.Now()
	_, err := client.PollAnswer(context.Background(), "dev-1", "conn-1", "https://example.com", 10*time.Second)
	if err == nil {
		t.Fatal("expected an error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("polling continued for %v after being redirected to the sign-in page", elapsed)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected a single poll, got %d", got)
	}
}
