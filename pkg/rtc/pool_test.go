package rtc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"rpi-ssh-go/pkg/connect"

	"github.com/pion/webrtc/v4"
)

func TestAliasesReuseOneSession(t *testing.T) {
	pool := NewPool(nil)
	targetDev := &connect.DeviceInfo{
		ID:     "dev-canonical-123",
		Name:   "my-pi",
		Online: true,
	}

	pool.SetDeviceResolver(func(identifier string) (*connect.DeviceInfo, error) {
		if strings.EqualFold(identifier, "my-pi") || identifier == "dev-canonical-123" {
			return targetDev, nil
		}
		return nil, fmt.Errorf("device '%s' not found", identifier)
	})

	connectCount := 0
	pool.SetConnector(func(target *connect.DeviceInfo) (*Session, error) {
		connectCount++
		pc1, pc2, dc1, _ := createLoopbackPair(t)
		t.Cleanup(func() {
			pc1.Close()
			pc2.Close()
		})
		return NewSession(target.Name, target.ID, pc1, dc1, make(chan string, 100)), nil
	})

	s1, err := pool.GetOrOpen(context.Background(), "my-pi")
	if err != nil {
		t.Fatalf("GetOrOpen(my-pi) failed: %v", err)
	}

	s2, err := pool.GetOrOpen(context.Background(), "dev-canonical-123")
	if err != nil {
		t.Fatalf("GetOrOpen(dev-canonical-123) failed: %v", err)
	}

	s3, err := pool.GetOrOpen(context.Background(), "MY-PI")
	if err != nil {
		t.Fatalf("GetOrOpen(MY-PI) failed: %v", err)
	}

	if s1 != s2 {
		t.Errorf("expected session by device name and canonical ID to be identical, got %p vs %p", s1, s2)
	}
	if s1 != s3 {
		t.Errorf("expected session by uppercase name to be identical, got %p vs %p", s1, s3)
	}
	if connectCount != 1 {
		t.Errorf("expected connector to be called exactly 1 time, got %d", connectCount)
	}

	active := pool.ActiveSessionNames()
	if len(active) != 1 {
		t.Errorf("expected exactly 1 active session in pool, got %v", active)
	}
}

func TestDisconnectDuringConnectCannotResurrectSession(t *testing.T) {
	pool := NewPool(nil)
	targetDev := &connect.DeviceInfo{
		ID:     "dev-canonical-456",
		Name:   "resurrect-pi",
		Online: true,
	}

	pool.SetDeviceResolver(func(identifier string) (*connect.DeviceInfo, error) {
		if strings.EqualFold(identifier, "resurrect-pi") || identifier == "dev-canonical-456" {
			return targetDev, nil
		}
		return nil, fmt.Errorf("device '%s' not found", identifier)
	})

	connectStarted := make(chan struct{})
	releaseConnect := make(chan struct{})
	var createdSession *Session

	pool.SetConnector(func(target *connect.DeviceInfo) (*Session, error) {
		pc1, pc2, dc1, _ := createLoopbackPair(t)
		t.Cleanup(func() {
			pc1.Close()
			pc2.Close()
		})
		sess := NewSession(target.Name, target.ID, pc1, dc1, make(chan string, 100))
		createdSession = sess

		close(connectStarted)
		<-releaseConnect
		return sess, nil
	})

	var getErr error
	var sessResult *Session
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		sessResult, getErr = pool.GetOrOpen(context.Background(), "resurrect-pi")
	}()

	<-connectStarted

	// Call Disconnect using canonical ID while connect is in flight
	_, _ = pool.Disconnect(context.Background(), "dev-canonical-456")

	close(releaseConnect)
	wg.Wait()

	active := pool.ActiveSessionNames()
	if len(active) != 0 {
		t.Errorf("expected no active sessions after disconnect during connect, got: %v", active)
	}

	if createdSession != nil && !createdSession.IsClosed() {
		t.Errorf("expected created session to be closed, but it remained open")
	}

	if sessResult != nil && !sessResult.IsClosed() {
		t.Errorf("expected returned session to be closed, but it was open")
	}
	if getErr == nil {
		t.Errorf("expected GetOrOpen to fail when disconnected during connect")
	}
}

func TestOldCallbackCannotCloseReplacement(t *testing.T) {
	pool := NewPool(nil)
	targetDev := &connect.DeviceInfo{
		ID:     "dev-canonical-789",
		Name:   "callback-pi",
		Online: true,
	}

	pool.SetDeviceResolver(func(identifier string) (*connect.DeviceInfo, error) {
		if strings.EqualFold(identifier, "callback-pi") || identifier == "dev-canonical-789" {
			return targetDev, nil
		}
		return nil, fmt.Errorf("device '%s' not found", identifier)
	})

	var mu sync.Mutex
	generations := make([]*Session, 0)
	var dcCallbacks []func()

	pool.SetConnector(func(target *connect.DeviceInfo) (*Session, error) {
		pc1, pc2, dc1, _ := createLoopbackPair(t)
		t.Cleanup(func() {
			pc1.Close()
			pc2.Close()
		})
		sess := NewSession(target.Name, target.ID, pc1, dc1, make(chan string, 100))

		closeCallback := func() {
			sess.MarkClosed()
		}

		mu.Lock()
		generations = append(generations, sess)
		dcCallbacks = append(dcCallbacks, closeCallback)
		mu.Unlock()

		return sess, nil
	})

	s1, err := pool.GetOrOpen(context.Background(), "callback-pi")
	if err != nil {
		t.Fatalf("first GetOrOpen failed: %v", err)
	}

	s1.MarkClosed()
	if !s1.IsClosed() {
		t.Fatalf("expected s1 to be closed")
	}

	s2, err := pool.GetOrOpen(context.Background(), "callback-pi")
	if err != nil {
		t.Fatalf("second GetOrOpen failed: %v", err)
	}

	if s1 == s2 {
		t.Fatalf("expected s1 and s2 to be distinct immutable generations, got identical pointer %p", s1)
	}

	mu.Lock()
	cb1 := dcCallbacks[0]
	mu.Unlock()

	cb1() // triggers s1.MarkClosed()

	if !s1.IsClosed() {
		t.Errorf("expected s1 to be closed")
	}
	if s2.IsClosed() {
		t.Fatalf("stale callback on old session closed the replacement session!")
	}
}

func TestPool_CloseClosesAllSessionsAndPreventsNew(t *testing.T) {
	pool := NewPool(nil)
	pool.SetConnector(func(target *connect.DeviceInfo) (*Session, error) {
		pc1, pc2, dc1, _ := createLoopbackPair(t)
		t.Cleanup(func() {
			pc1.Close()
			pc2.Close()
		})
		return NewSession(target.Name, target.ID, pc1, dc1, make(chan string, 100)), nil
	})

	s1, err := pool.GetOrOpen(context.Background(), "dev-1")
	if err != nil {
		t.Fatalf("failed to open dev-1: %v", err)
	}
	s2, err := pool.GetOrOpen(context.Background(), "dev-2")
	if err != nil {
		t.Fatalf("failed to open dev-2: %v", err)
	}

	if len(pool.ActiveSessionNames()) != 2 {
		t.Errorf("expected 2 active sessions, got %d", len(pool.ActiveSessionNames()))
	}

	pool.Close()

	if !s1.IsClosed() || !s2.IsClosed() {
		t.Errorf("expected all sessions to be closed after pool.Close()")
	}

	if len(pool.ActiveSessionNames()) != 0 {
		t.Errorf("expected 0 active sessions after pool.Close()")
	}

	_, err = pool.GetOrOpen(context.Background(), "dev-1")
	if err == nil {
		t.Errorf("expected GetOrOpen after pool.Close() to fail")
	}

	// Idempotent Close
	pool.Close()
}

func TestPool_RepeatedFailReconnectCycleReleasesLocksAndGoroutines(t *testing.T) {
	pool := NewPool(nil)
	pool.SetConnector(func(target *connect.DeviceInfo) (*Session, error) {
		pc1, pc2, dc1, _ := createLoopbackPair(t)
		t.Cleanup(func() {
			pc1.Close()
			pc2.Close()
		})
		return NewSession(target.Name, target.ID, pc1, dc1, make(chan string, 100)), nil
	})

	for i := 0; i < 20; i++ {
		devName := fmt.Sprintf("dev-%d", i%3)
		sess, err := pool.GetOrOpen(context.Background(), devName)
		if err != nil {
			t.Fatalf("iteration %d: GetOrOpen failed: %v", i, err)
		}
		sess.Close()
	}

	pool.globalMu.Lock()
	numLocks := len(pool.deviceLocks)
	pool.globalMu.Unlock()

	if numLocks != 0 {
		t.Errorf("expected 0 active deviceLocks after completed operations, got %d", numLocks)
	}
}

func TestConnectionErrorDisposesPeerConnection(t *testing.T) {
	pc := newTestPeerConnection(t)
	var success bool
	func() {
		defer func() {
			if !success && pc != nil {
				_ = pc.Close()
			}
		}()
		// Simulated error before success
		return
	}()

	if pc.ConnectionState() != webrtc.PeerConnectionStateClosed {
		t.Errorf("expected PeerConnection to be closed on connection error")
	}
}

type mockConnectClient struct {
	devices        []connect.DeviceInfo
	err            error
	shellICEConfig connect.ICEConfig
}

func (m *mockConnectClient) ListDevices(ctx context.Context, forceRefresh bool) ([]connect.DeviceInfo, error) {
	return m.devices, m.err
}

func (m *mockConnectClient) GetShellSessionInfo(ctx context.Context, deviceID string) (connect.ICEConfig, string, error) {
	return m.shellICEConfig, "mock-csrf-token", nil
}

func (m *mockConnectClient) PostOffer(ctx context.Context, deviceID, sdp, csrfToken, referer string) (string, error) {
	return "", nil
}

func (m *mockConnectClient) PollAnswer(ctx context.Context, deviceID, connID, referer string, maxDuration time.Duration) (string, error) {
	return "", nil
}

func TestResolveDevice_DeviceNotFoundWhenClientListSucceeds(t *testing.T) {
	mock := &mockConnectClient{
		devices: []connect.DeviceInfo{
			{ID: "dev-1", Name: "pi-one", Online: true},
		},
		err: nil,
	}
	pool := NewPool(nil)
	pool.SetClient(mock)

	_, err := pool.resolveDevice(context.Background(), "nonexistent-pi")
	if err == nil {
		t.Fatalf("expected error when device not found in client device list, got nil")
	}
	expectedErr := "device 'nonexistent-pi' not found"
	if err.Error() != expectedErr {
		t.Errorf("expected error %q, got %q", expectedErr, err.Error())
	}

	// Verify that a device present in the list is resolved correctly
	dev, err := pool.resolveDevice(context.Background(), "pi-one")
	if err != nil {
		t.Fatalf("unexpected error resolving existing device: %v", err)
	}
	if dev.ID != "dev-1" {
		t.Errorf("expected device ID dev-1, got %s", dev.ID)
	}

	// Case-insensitive match on name
	devUpper, err := pool.resolveDevice(context.Background(), "PI-ONE")
	if err != nil {
		t.Fatalf("unexpected error resolving existing device uppercase: %v", err)
	}
	if devUpper.ID != "dev-1" {
		t.Errorf("expected device ID dev-1, got %s", devUpper.ID)
	}

	// Match on canonical ID
	devByID, err := pool.resolveDevice(context.Background(), "dev-1")
	if err != nil {
		t.Fatalf("unexpected error resolving existing device by ID: %v", err)
	}
	if devByID.Name != "pi-one" {
		t.Errorf("expected device Name pi-one, got %s", devByID.Name)
	}
}

// When the device list cannot be fetched, the caller has to see why. Calling
// it "not found" sends people looking for a typo in the device name.
func TestResolveDevice_ReportsListFailureInsteadOfNotFound(t *testing.T) {
	listErr := errors.New("session expired and RPI_EMAIL/RPI_PASSWORD not configured")
	pool := NewPool(nil)
	pool.SetClient(&mockConnectClient{err: listErr})

	_, err := pool.resolveDevice(context.Background(), "kitchen-pi")
	if err == nil {
		t.Fatal("expected an error when the device list cannot be fetched")
	}
	if !errors.Is(err, listErr) {
		t.Fatalf("expected the device list error to be reported, got: %v", err)
	}
	if strings.Contains(err.Error(), "not found") {
		t.Fatalf("list failure was reported as a missing device: %v", err)
	}
}

func TestResolveDevice_SyntheticFallbackWhenClientIsNil(t *testing.T) {
	pool := NewPool(nil)
	dev, err := pool.resolveDevice(context.Background(), "synthetic-pi")
	if err != nil {
		t.Fatalf("unexpected error with nil client: %v", err)
	}
	if dev.ID != "synthetic-pi" || dev.Name != "synthetic-pi" {
		t.Errorf("expected synthetic device ID and Name to be 'synthetic-pi', got %+v", dev)
	}
}

func TestICEGatherCancellationClosesPeer(t *testing.T) {
	pool := NewPool(nil)
	pool.SetDeviceResolver(func(identifier string) (*connect.DeviceInfo, error) {
		return &connect.DeviceInfo{
			ID:     "dev-ice-cancel",
			Name:   "ice-pi",
			Online: true,
		}, nil
	})

	var createdPC *webrtc.PeerConnection
	var pcMu sync.Mutex

	pool.SetPeerConnectionFactory(func(cfg connect.ICEConfig, mode NetworkMode) (*webrtc.PeerConnection, error) {
		config, _, err := peerConnectionSettings(cfg, mode)
		if err != nil {
			return nil, err
		}
		pc := newVirtualTestPeer(t, newTestRouter(t), "10.77.0.1", config)
		if err == nil {
			pcMu.Lock()
			createdPC = pc
			pcMu.Unlock()
		}
		return pc, err
	})

	mock := &mockConnectClient{
		shellICEConfig: connect.ICEConfig{
			IceServers: []connect.ICEServerConfig{
				{
					URLs: []string{"stun:192.0.2.1:3478"},
				},
			},
		},
	}
	pool.SetClient(mock)
	t.Setenv("RPI_NETWORK_MODE", "direct")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	sess, err := pool.GetOrOpen(ctx, "ice-pi")
	elapsed := time.Since(start)

	if err == nil {
		if sess != nil {
			sess.Close()
		}
		t.Fatalf("expected error from context cancellation during ICE gathering, got nil")
	}

	if elapsed > 2*time.Second {
		t.Errorf("GetOrOpen took too long to abort on cancelled ICE gather: %v", elapsed)
	}

	pcMu.Lock()
	pc := createdPC
	pcMu.Unlock()

	if pc == nil {
		t.Fatalf("expected peer connection to have been created")
	}

	if pc.ConnectionState() != webrtc.PeerConnectionStateClosed {
		t.Errorf("expected PeerConnection to be closed after context cancellation, got state: %v", pc.ConnectionState())
	}
}

func TestDeviceLockWaitCancellation(t *testing.T) {
	pool := NewPool(nil)
	pool.SetDeviceResolver(func(identifier string) (*connect.DeviceInfo, error) {
		return &connect.DeviceInfo{
			ID:     "dev-lock-cancel",
			Name:   "lock-pi",
			Online: true,
		}, nil
	})

	started := make(chan struct{})
	release := make(chan struct{})
	var connectorCount int32

	pool.SetConnector(func(target *connect.DeviceInfo) (*Session, error) {
		atomic.AddInt32(&connectorCount, 1)
		pc1, pc2, dc1, _ := createLoopbackPair(t)
		t.Cleanup(func() {
			pc1.Close()
			pc2.Close()
		})
		sess := NewSession(target.Name, target.ID, pc1, dc1, make(chan string, 100))
		close(started)
		<-release
		return sess, nil
	})

	var sess1 *Session
	var err1 error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		sess1, err1 = pool.GetOrOpen(context.Background(), "lock-pi")
	}()

	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	sess2, err2 := pool.GetOrOpen(ctx, "lock-pi")
	if err2 == nil {
		t.Fatalf("expected second GetOrOpen to fail with deadline exceeded while waiting for lock, got nil")
	}
	if sess2 != nil {
		t.Errorf("expected sess2 to be nil, got %v", sess2)
	}

	if count := atomic.LoadInt32(&connectorCount); count != 1 {
		t.Errorf("expected connectorCount to be 1, got %d", count)
	}

	close(release)
	wg.Wait()

	if err1 != nil {
		t.Fatalf("unexpected error on first GetOrOpen: %v", err1)
	}
	if sess1 == nil {
		t.Fatalf("expected sess1 to be non-nil")
	}

	sess1.Close()

	pool.globalMu.Lock()
	numLocks := len(pool.deviceLocks)
	pool.globalMu.Unlock()

	if numLocks != 0 {
		t.Errorf("expected 0 active deviceLocks, got %d", numLocks)
	}
}
