package connect

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"rpi-ssh-go/pkg/auth"
)

type roundTripFunc func(req *http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type stalledBody struct {
	reqCtx context.Context
}

func (s *stalledBody) Read(p []byte) (int, error) {
	<-s.reqCtx.Done()
	return 0, s.reqCtx.Err()
}

func (s *stalledBody) Close() error {
	return nil
}

type trackingCloser struct {
	io.Reader
	closed int32
}

func (c *trackingCloser) Close() error {
	atomic.AddInt32(&c.closed, 1)
	return nil
}

// TestHTTPDeadlineStopsStalledBody asserts that context deadline terminates reading stalled response body.
func TestHTTPDeadlineStopsStalledBody(t *testing.T) {
	client := &Client{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       &stalledBody{reqCtx: req.Context()},
					Request:    req,
				}, nil
			}),
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	// forceRefresh=true bypasses device cache
	_, err := client.ListDevices(ctx, true)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected error from reading stalled body with expired deadline, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Errorf("expected context.DeadlineExceeded in error, got: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("ListDevices took too long to abort on stalled body: %v", elapsed)
	}
}

// TestPollClosesNon200Body asserts non-200 responses have response bodies closed.
func TestPollClosesNon200Body(t *testing.T) {
	bodyTracker := &trackingCloser{
		Reader: strings.NewReader("not found"),
	}

	client := &Client{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusNotFound,
					Header:     make(http.Header),
					Body:       bodyTracker,
					Request:    req,
				}, nil
			}),
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, _ = client.PollAnswer(ctx, "dev-1", "conn-1", "https://example.com", 200*time.Millisecond)

	if atomic.LoadInt32(&bodyTracker.closed) == 0 {
		t.Fatalf("expected response body of non-200 poll response to be closed, but it was not")
	}
}

// TestPollParentCancellation asserts parent context cancellation aborts poll loop promptly.
func TestPollParentCancellation(t *testing.T) {
	client := &Client{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				// Return 200 with no answer yet
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"id":"conn-1","answer":null}`)),
					Request:    req,
				}, nil
			}),
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := client.PollAnswer(ctx, "dev-1", "conn-1", "https://example.com", 10*time.Second)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected error from cancelled context in PollAnswer, got nil")
	}
	if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("expected context.Canceled in error, got: %v", err)
	}
	if elapsed > 1*time.Second {
		t.Errorf("PollAnswer took too long to cancel: %v", elapsed)
	}
}

// TestConcurrentCachedReadNotBlockedByStalledRefresh verifies that an in-flight network call
// does not lock out concurrent requests for cached device info.
func TestConcurrentCachedReadNotBlockedByStalledRefresh(t *testing.T) {
	stalledStarted := make(chan struct{})
	releaseStalled := make(chan struct{})

	client := &Client{
		deviceCache: []DeviceInfo{
			{ID: "dev-cached", Name: "my-cached-pi", Online: true},
		},
		cacheTime: time.Now(),
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				close(stalledStarted)
				<-releaseStalled
				return nil, context.Canceled
			}),
		},
	}

	// Launch forced refresh that stalls on network
	go func() {
		_, _ = client.ListDevices(context.Background(), true)
	}()

	<-stalledStarted

	// Cached read should return immediately without waiting for stalled network I/O
	start := time.Now()
	devices, err := client.ListDevices(context.Background(), false)
	elapsed := time.Since(start)

	close(releaseStalled)

	if err != nil {
		t.Fatalf("expected cached devices read to succeed, got: %v", err)
	}
	if len(devices) != 1 || devices[0].Name != "my-cached-pi" {
		t.Fatalf("expected cached device 'my-cached-pi', got: %+v", devices)
	}
	if elapsed > 50*time.Millisecond {
		t.Errorf("cached read was blocked by stalled refresh: took %v", elapsed)
	}
}

func TestEmptyDeviceListCached(t *testing.T) {
	var networkRequests int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&networkRequests, 1)
		// Return 200 with HTML table having no device rows
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `<html><body><table><thead><tr><th>Name</th></tr></thead><tbody></tbody></table></body></html>`)
	}))
	defer server.Close()

	cookiePath := filepath.Join(t.TempDir(), "cookies.json")
	jar, err := auth.NewFileCookieJar(cookiePath)
	if err != nil {
		t.Fatalf("Failed to create cookie jar: %v", err)
	}

	serverURL, _ := url.Parse(server.URL)
	// Seed jar with valid session cookie so probe succeeds
	jar.SetCookies(serverURL, []*http.Cookie{
		{
			Name:    "connect_session",
			Value:   "valid_token",
			Path:    "/",
			Expires: time.Now().Add(1 * time.Hour),
		},
	})

	client := &Client{
		httpClient: &http.Client{Jar: jar},
		cookieJar:  jar,
		baseURL:    server.URL,
	}

	// First call to ListDevices fetches from network
	ctx := context.Background()
	devices1, err := client.ListDevices(ctx, false)
	if err != nil {
		t.Fatalf("ListDevices failed: %v", err)
	}
	if len(devices1) != 0 {
		t.Fatalf("Expected 0 devices, got %d", len(devices1))
	}
	initialReqs := atomic.LoadInt32(&networkRequests)
	if initialReqs == 0 {
		t.Fatalf("Expected at least 1 network request on initial fetch")
	}

	// Second call with forceRefresh=false within 30s should return cached empty list
	devices2, err := client.ListDevices(ctx, false)
	if err != nil {
		t.Fatalf("Second ListDevices failed: %v", err)
	}
	if len(devices2) != 0 {
		t.Fatalf("Expected 0 devices on cached read, got %d", len(devices2))
	}

	// Network request count must NOT increase
	afterReqs := atomic.LoadInt32(&networkRequests)
	if afterReqs != initialReqs {
		t.Fatalf("Expected empty device list to be cached for 30s without re-fetching; requests grew from %d to %d", initialReqs, afterReqs)
	}
}

func TestConcurrentAuthenticationSingleLogin(t *testing.T) {
	var loginAttempts int32
	var probeAttempts int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/devices"):
			atomic.AddInt32(&probeAttempts, 1)
			cookie, err := r.Cookie("connect_session")
			if err == nil && cookie.Value == "session_token_123" {
				w.Header().Set("Content-Type", "text/html")
				w.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprint(w, `<html><body>Devices List</body></html>`)
				return
			}
			// Return redirect to sign-in simulating unauthenticated state
			http.Redirect(w, r, "/sign-in", http.StatusFound)
		case strings.HasSuffix(r.URL.Path, "/sign-in"):
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, `<html><head><meta name="csrf-token" content="csrf-1"></head><body>Sign In</body></html>`)
		case strings.HasSuffix(r.URL.Path, "/auth/raspberry_pi"):
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, `<html><head><meta name="csrf-token" content="csrf-2"></head><body>ID Sign In</body></html>`)
		case strings.HasSuffix(r.URL.Path, "/session"):
			atomic.AddInt32(&loginAttempts, 1)
			// Add artificial delay to simulate network latency during authentication
			time.Sleep(50 * time.Millisecond)
			http.SetCookie(w, &http.Cookie{
				Name:    "connect_session",
				Value:   "session_token_123",
				Path:    "/",
				Expires: time.Now().Add(1 * time.Hour),
			})
			http.Redirect(w, r, "/devices", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cookiePath := filepath.Join(t.TempDir(), "cookies.json")
	jar, err := auth.NewFileCookieJar(cookiePath)
	if err != nil {
		t.Fatalf("Failed to create cookie jar: %v", err)
	}

	client := &Client{
		httpClient: &http.Client{Jar: jar},
		cookieJar:  jar,
		email:      "test@example.com",
		password:   "secret",
		baseURL:    server.URL,
	}

	concurrency := 8
	var wg sync.WaitGroup
	errCh := make(chan error, concurrency)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := client.EnsureAuthenticated(context.Background())
			if err != nil {
				errCh <- err
			}
		}()
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatalf("EnsureAuthenticated failed during concurrent call: %v", err)
	}

	actualLogins := atomic.LoadInt32(&loginAttempts)
	if actualLogins != 1 {
		t.Fatalf("Expected exactly 1 login attempt due to deduplication, got %d", actualLogins)
	}
}

func TestCachedDevicesReturnedAsDefensiveCopy(t *testing.T) {
	client := &Client{
		deviceCache: []DeviceInfo{
			{ID: "dev-1", Name: "original-name"},
		},
		cacheTime: time.Now(),
	}

	devices, err := client.ListDevices(context.Background(), false)
	if err != nil {
		t.Fatalf("ListDevices failed: %v", err)
	}
	if len(devices) != 1 {
		t.Fatalf("Expected 1 device, got %d", len(devices))
	}

	// Mutate the returned slice
	devices[0].Name = "mutated-name"

	// Fetch again from cache
	devices2, err := client.ListDevices(context.Background(), false)
	if err != nil {
		t.Fatalf("ListDevices failed: %v", err)
	}
	if devices2[0].Name != "original-name" {
		t.Fatalf("Expected original name 'original-name', but cache was mutated to %q!", devices2[0].Name)
	}
}

func TestDeviceRefreshRejectsFailedResponse(t *testing.T) {
	for _, scenario := range []string{"status", "redirect"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			c := &Client{httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				status := 200
				body := `<tr><a href="/devices/abc">pi</a>Online Remote shell</tr>`
				responseReq := req
				if calls > 1 {
					body = "unavailable"
					if scenario == "status" {
						status = 500
					} else {
						copyReq := req.Clone(req.Context())
						copyURL := *req.URL
						copyURL.Path = "/login"
						copyReq.URL = &copyURL
						responseReq = copyReq
					}
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: responseReq}, nil
			})}}
			// The first call authenticates; its probe of the devices page
			// succeeds and fills the cache.
			if _, err := c.ListDevices(context.Background(), false); err != nil {
				t.Fatalf("initial device list failed: %v", err)
			}
			// The login is still fresh, so the forced refresh fetches the
			// page itself, and that fetch fails.
			if _, err := c.ListDevices(context.Background(), true); err == nil {
				t.Fatal("failed response was cached as successful empty device list")
			}
			if calls != 2 {
				t.Fatalf("expected the forced refresh to fetch the page once more, got %d requests in total", calls)
			}
			if len(c.deviceCache) != 1 {
				t.Fatal("destroyed authenticated device cache")
			}
		})
	}
}

// The login check fetches the devices page itself, so a forced refresh that
// had to log in already holds a live list and must not fetch it again. This
// has to hold on every run, including when the clock does not advance
// between two readings (Windows timers are coarse).
func TestForcedRefreshReusesListFetchedByLoginCheck(t *testing.T) {
	for i := 0; i < 300; i++ {
		calls := 0
		c := &Client{httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			body := `<tr><a href="/devices/abc">pi</a>Online Remote shell</tr>`
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
		})}}
		devices, err := c.ListDevices(context.Background(), true)
		if err != nil || len(devices) != 1 {
			t.Fatalf("run %d: unexpected result %+v, %v", i, devices, err)
		}
		if calls != 1 {
			t.Fatalf("run %d: devices page fetched %d times, want 1", i, calls)
		}
	}
}
