package rtc

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"rpi-ssh-go/pkg/connect"
	"rpi-ssh-go/pkg/rtc/rtctest"

	"github.com/pion/transport/v5/vnet"
	"github.com/pion/webrtc/v4"
)

// All test peers use Pion's in-process network, never OS listeners.
func newTestRouter(t *testing.T) *vnet.Router { return rtctest.NewRouter(t) }

func newVirtualTestPeer(t *testing.T, router *vnet.Router, ip string, configs ...webrtc.Configuration) *webrtc.PeerConnection {
	return rtctest.NewPeer(t, router, ip, configs...)
}

func newTestPeerConnection(t *testing.T) *webrtc.PeerConnection {
	return newVirtualTestPeer(t, newTestRouter(t), "10.77.0.1")
}

// Create two real WebRTC peers connected entirely through an in-memory router.
func createLoopbackPair(t *testing.T) (*webrtc.PeerConnection, *webrtc.PeerConnection, *webrtc.DataChannel, *webrtc.DataChannel) {
	return rtctest.LoopbackPair(t)
}

func TestSession_StateTracking_ActiveAndClosed(t *testing.T) {
	pc := newTestPeerConnection(t)
	defer pc.Close()

	sess := NewSession("test-dev", "dev-123", pc, nil, make(chan string, 10))

	if sess.IsClosed() {
		t.Errorf("expected session to be open, got closed")
	}

	sess.Close()
	if !sess.IsClosed() {
		t.Errorf("expected session to be closed after Close(), got open")
	}

	// Nil PeerConnection should be treated as closed
	sessNilPC := NewSession("test-dev", "dev-123", nil, nil, nil)
	if !sessNilPC.IsClosed() {
		t.Errorf("expected session with nil PeerConnection to be closed")
	}
}

func TestSession_IdempotentClose(t *testing.T) {
	pc := newTestPeerConnection(t)
	sess := NewSession("test-dev", "dev-123", pc, nil, make(chan string, 10))

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sess.Close()
		}()
	}
	wg.Wait()

	if !sess.IsClosed() {
		t.Errorf("expected session to be closed after concurrent Close calls")
	}
}

func TestSession_SimulatedConnectionFailureDetection(t *testing.T) {
	tests := []struct {
		name       string
		pcState    webrtc.PeerConnectionState
		iceState   webrtc.ICEConnectionState
		dcState    webrtc.DataChannelState
		wantClosed bool
	}{
		{
			name:       "connected and healthy",
			pcState:    webrtc.PeerConnectionStateConnected,
			iceState:   webrtc.ICEConnectionStateConnected,
			dcState:    webrtc.DataChannelStateOpen,
			wantClosed: false,
		},
		{
			// Disconnected is transient: ICE recovers or moves on to failed.
			name:       "peer connection disconnected",
			pcState:    webrtc.PeerConnectionStateDisconnected,
			iceState:   webrtc.ICEConnectionStateConnected,
			dcState:    webrtc.DataChannelStateOpen,
			wantClosed: false,
		},
		{
			name:       "peer connection failed",
			pcState:    webrtc.PeerConnectionStateFailed,
			iceState:   webrtc.ICEConnectionStateConnected,
			dcState:    webrtc.DataChannelStateOpen,
			wantClosed: true,
		},
		{
			name:       "peer connection closed",
			pcState:    webrtc.PeerConnectionStateClosed,
			iceState:   webrtc.ICEConnectionStateConnected,
			dcState:    webrtc.DataChannelStateOpen,
			wantClosed: true,
		},
		{
			name:       "ICE connection disconnected",
			pcState:    webrtc.PeerConnectionStateConnected,
			iceState:   webrtc.ICEConnectionStateDisconnected,
			dcState:    webrtc.DataChannelStateOpen,
			wantClosed: false,
		},
		{
			name:       "ICE connection failed",
			pcState:    webrtc.PeerConnectionStateConnected,
			iceState:   webrtc.ICEConnectionStateFailed,
			dcState:    webrtc.DataChannelStateOpen,
			wantClosed: true,
		},
		{
			name:       "ICE connection closed",
			pcState:    webrtc.PeerConnectionStateConnected,
			iceState:   webrtc.ICEConnectionStateClosed,
			dcState:    webrtc.DataChannelStateOpen,
			wantClosed: true,
		},
		{
			name:       "DataChannel closing",
			pcState:    webrtc.PeerConnectionStateConnected,
			iceState:   webrtc.ICEConnectionStateConnected,
			dcState:    webrtc.DataChannelStateClosing,
			wantClosed: true,
		},
		{
			name:       "DataChannel closed",
			pcState:    webrtc.PeerConnectionStateConnected,
			iceState:   webrtc.ICEConnectionStateConnected,
			dcState:    webrtc.DataChannelStateClosed,
			wantClosed: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pc := newTestPeerConnection(t)
			defer pc.Close()

			sess := NewSession("test-dev", "dev-123", pc, nil, make(chan string, 10))
			sess.SetStateOverride(func() (webrtc.PeerConnectionState, webrtc.ICEConnectionState, webrtc.DataChannelState) {
				return tt.pcState, tt.iceState, tt.dcState
			})

			got := sess.IsClosed()
			if got != tt.wantClosed {
				t.Errorf("IsClosed() = %v, want %v", got, tt.wantClosed)
			}
		})
	}
}

func TestSession_HeartbeatLifecycleAndNoGoroutineLeak(t *testing.T) {
	pc := newTestPeerConnection(t)
	defer pc.Close()

	sess := NewSession("test-dev", "dev-123", pc, nil, make(chan string, 10))

	initialGoroutines := runtime.NumGoroutine()

	sess.StartKeepalive(20 * time.Millisecond)

	if !sess.IsKeepaliveRunning() {
		t.Errorf("expected keepalive to be running")
	}

	time.Sleep(60 * time.Millisecond)

	sess.Close()

	if sess.IsKeepaliveRunning() {
		t.Errorf("expected keepalive to stop after Close()")
	}

	deadline := time.Now().Add(500 * time.Millisecond)
	leaked := true
	for time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		if runtime.NumGoroutine() <= initialGoroutines+2 {
			leaked = false
			break
		}
	}

	if leaked {
		t.Errorf("potential goroutine leak: initial=%d, current=%d", initialGoroutines, runtime.NumGoroutine())
	}
}

func TestSession_KeepaliveSendsPayloadOverDataChannel(t *testing.T) {
	pc1, pc2, dc1, dc2 := createLoopbackPair(t)
	defer pc1.Close()
	defer pc2.Close()

	received := make(chan string, 10)
	dc2.OnMessage(func(msg webrtc.DataChannelMessage) {
		received <- string(msg.Data)
	})

	sess := NewSession("test-dev", "dev-123", pc1, dc1, make(chan string, 10))
	sess.SetKeepalivePayload("\x00")
	sess.StartKeepalive(30 * time.Millisecond)
	defer sess.Close()

	select {
	case payload := <-received:
		if payload != "\x00" {
			t.Errorf("expected keepalive payload '\\x00', got: %q", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for keepalive payload on remote data channel")
	}
}

func TestSession_ExecWithLoopback(t *testing.T) {
	pc1, pc2, dc1, dc2 := createLoopbackPair(t)
	defer pc1.Close()
	defer pc2.Close()

	outputChan := make(chan string, 100)
	dc1.OnMessage(func(msg webrtc.DataChannelMessage) {
		outputChan <- string(msg.Data)
	})

	sess := NewSession("test-dev", "dev-123", pc1, dc1, outputChan)
	defer sess.Close()

	// Simulate remote Raspberry Pi shell
	dc2.OnMessage(func(msg webrtc.DataChannelMessage) {
		cmd := string(msg.Data)
		// Extract runID markers from wrappedCmd
		startRe := regexp.MustCompile(`__MCP_BEGIN_([a-f0-9]+)__`)
		if m := startRe.FindStringSubmatch(cmd); len(m) > 1 {
			runID := m[1]
			reply := fmt.Sprintf("__MCP_BEGIN_%s__\nHello Raspberry Pi\n__MCP_END_%s_0__\n", runID, runID)
			_ = dc2.SendText(reply)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res, err := sess.Exec(ctx, "echo 'Hello Raspberry Pi'", 1000)
	if err != nil {
		t.Fatalf("Exec failed: %v", err)
	}

	if res.Stdout != "Hello Raspberry Pi" {
		t.Errorf("expected stdout 'Hello Raspberry Pi', got: %v", res.Stdout)
	}
	if res.ExitCode != 0 {
		t.Errorf("expected exit_code 0, got: %v", res.ExitCode)
	}
}

func TestSession_ClosedSessionRejectsExec(t *testing.T) {
	pc1, pc2, dc1, _ := createLoopbackPair(t)
	defer pc1.Close()
	defer pc2.Close()

	outputChan := make(chan string, 100)
	sess := NewSession("test-dev", "dev-123", pc1, dc1, outputChan)

	// Close the session
	sess.Close()

	// Closed session must immediately reject Exec without running commands
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_, err := sess.Exec(ctx, "echo 'test'", 1000)
	if err == nil {
		t.Fatalf("expected error when executing command on closed session, got nil")
	}
	if !strings.Contains(err.Error(), "session is closed") {
		t.Errorf("expected error to contain 'session is closed', got: %v", err)
	}
}

func TestPool_DisconnectAndActiveNames(t *testing.T) {
	pool := NewPool(nil)

	pc1 := newTestPeerConnection(t)
	defer pc1.Close()
	pc2 := newTestPeerConnection(t)
	defer pc2.Close()

	sess1 := NewSession("dev-alpha", "id-1", pc1, nil, nil)
	sess2 := NewSession("dev-beta", "id-2", pc2, nil, nil)

	pool.globalMu.Lock()
	pool.sessions["dev-alpha"] = sess1
	pool.sessions["dev-beta"] = sess2
	pool.globalMu.Unlock()

	active := pool.ActiveSessionNames()
	if len(active) != 2 {
		t.Errorf("expected 2 active sessions, got: %v", active)
	}

	// Disconnect dev-alpha
	msg, err := pool.Disconnect(context.Background(), "dev-alpha")
	if err != nil {
		t.Fatalf("unexpected error disconnecting: %v", err)
	}
	if !strings.Contains(msg, "Disconnected persistent session") {
		t.Errorf("unexpected disconnect msg: %s", msg)
	}
	if !sess1.IsClosed() {
		t.Errorf("expected sess1 to be closed after Disconnect")
	}

	active = pool.ActiveSessionNames()
	if len(active) != 1 || active[0] != "dev-beta" {
		t.Errorf("expected only dev-beta active, got: %v", active)
	}

	// Disconnecting nonexistent device
	msg, err = pool.Disconnect(context.Background(), "dev-gamma")
	if err != nil {
		t.Fatalf("unexpected error disconnecting: %v", err)
	}
	if !strings.Contains(msg, "No active session found") {
		t.Errorf("unexpected nonexistent disconnect msg: %s", msg)
	}
}

func TestSession_ConfigurePTY(t *testing.T) {
	pc1, pc2, dc1, dc2 := createLoopbackPair(t)
	defer pc1.Close()
	defer pc2.Close()

	received := make(chan string, 10)
	dc2.OnMessage(func(msg webrtc.DataChannelMessage) {
		received <- string(msg.Data)
	})

	sess := NewSession("test-dev", "dev-123", pc1, dc1, make(chan string, 10))
	defer sess.Close()

	if DefaultPTYCols != 160 || DefaultPTYRows != 40 {
		t.Errorf("expected default PTY dimensions 160x40, got %dx%d", DefaultPTYCols, DefaultPTYRows)
	}

	if err := sess.ConfigurePTY(160, 40); err != nil {
		t.Fatalf("ConfigurePTY failed: %v", err)
	}

	select {
	case cmd := <-received:
		expected := "stty cols 160 rows 40 2>/dev/null || true\n"
		if cmd != expected {
			t.Errorf("expected PTY config cmd %q, got %q", expected, cmd)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for PTY config command on data channel")
	}

	// Test fallback to defaults if 0 provided
	if err := sess.ConfigurePTY(0, 0); err != nil {
		t.Fatalf("ConfigurePTY(0, 0) failed: %v", err)
	}

	select {
	case cmd := <-received:
		expected := "stty cols 160 rows 40 2>/dev/null || true\n"
		if cmd != expected {
			t.Errorf("expected fallback PTY config cmd %q, got %q", expected, cmd)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for fallback PTY config command")
	}
}

func TestSession_ExecComplexEscaping(t *testing.T) {
	pc1, pc2, dc1, dc2 := createLoopbackPair(t)
	defer pc1.Close()
	defer pc2.Close()

	outputChan := make(chan string, 100)
	dc1.OnMessage(func(msg webrtc.DataChannelMessage) {
		outputChan <- string(msg.Data)
	})

	sess := NewSession("test-dev", "dev-123", pc1, dc1, outputChan)
	defer sess.Close()

	receivedCmdChan := make(chan string, 1)

	// Simulate remote Raspberry Pi shell responding to wrapped command
	dc2.OnMessage(func(msg webrtc.DataChannelMessage) {
		cmd := string(msg.Data)
		receivedCmdChan <- cmd

		startRe := regexp.MustCompile(`__MCP_BEGIN_([a-f0-9]+)__`)
		if m := startRe.FindStringSubmatch(cmd); len(m) > 1 {
			runID := m[1]
			reply := fmt.Sprintf("__MCP_BEGIN_%s__\ncomplex_output_value\n__MCP_END_%s_0__\n", runID, runID)
			_ = dc2.SendText(reply)
		}
	})

	complexCmd := `awk '{print $1}' /etc/hosts`
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res, err := sess.Exec(ctx, complexCmd, 1000)
	if err != nil {
		t.Fatalf("Exec failed: %v", err)
	}

	if res.Stdout != "complex_output_value" {
		t.Errorf("expected stdout 'complex_output_value', got: %v", res.Stdout)
	}
	if res.ExitCode != 0 {
		t.Errorf("expected exit_code 0, got: %v", res.ExitCode)
	}

	select {
	case dispatched := <-receivedCmdChan:
		if !strings.Contains(dispatched, "base64 -d") {
			t.Errorf("expected complex command with quotes to be base64-wrapped, got: %s", dispatched)
		}
	case <-time.After(1 * time.Second):
		t.Fatalf("timed out waiting for dispatched command")
	}
}

func TestCloseAfterMarkClosedClosesTransport(t *testing.T) {
	pc := newTestPeerConnection(t)
	sess := NewSession("test-dev", "dev-123", pc, nil, make(chan string, 10))

	sess.MarkClosed()
	if !sess.IsClosed() {
		t.Fatalf("expected session to be closed after MarkClosed")
	}

	sess.Close()

	if state := pc.ConnectionState(); state != webrtc.PeerConnectionStateClosed {
		t.Fatalf("expected PeerConnection state to be Closed after Close(), got %v", state)
	}
}

func TestCloseConcurrentWithCallbackDoesNotDeadlock(t *testing.T) {
	pc1, pc2, dc1, _ := createLoopbackPair(t)
	defer pc2.Close()

	outChan := make(chan string, 100)
	sess := NewSession("test-dev", "dev-123", pc1, dc1, outChan)

	pc1.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		sess.MarkClosed()
	})
	dc1.OnClose(func() {
		sess.MarkClosed()
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			wg.Add(2)
			go func() {
				defer wg.Done()
				sess.MarkClosed()
			}()
			go func() {
				defer wg.Done()
				sess.Close()
			}()
		}
		wg.Wait()
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("deadlock detected: Close concurrent with callbacks did not finish within 3 seconds")
	}
}

func TestCancelledQueuedExecNeverSends(t *testing.T) {
	pc1, pc2, dc1, dc2 := createLoopbackPair(t)
	defer pc1.Close()
	defer pc2.Close()

	outChan := make(chan string, 100)
	sess := NewSession("test-dev", "dev-123", pc1, dc1, outChan)
	defer sess.Close()

	var receivedMu sync.Mutex
	var receivedMessages []string
	firstDispatched := make(chan struct{})

	dc2.OnMessage(func(msg webrtc.DataChannelMessage) {
		receivedMu.Lock()
		defer receivedMu.Unlock()
		receivedMessages = append(receivedMessages, string(msg.Data))
		select {
		case <-firstDispatched:
		default:
			close(firstDispatched)
		}
	})

	// Active command 1 occupies the execution gate
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	go func() {
		_, _ = sess.Exec(ctx1, "echo 'first_cmd'", 1000)
	}()

	// Wait until command 1 is dispatched and holds the execution gate
	select {
	case <-firstDispatched:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first command dispatch")
	}

	// Command 2 is queued behind command 1
	ctx2, cancel2 := context.WithCancel(context.Background())
	cmd2ErrChan := make(chan error, 1)

	go func() {
		_, err := sess.Exec(ctx2, "echo 'second_cmd_never_send'", 1000)
		cmd2ErrChan <- err
	}()

	// Cancel command 2 while it is waiting in queue for the execution gate
	time.Sleep(30 * time.Millisecond)
	cancel2()

	select {
	case err := <-cmd2ErrChan:
		if err == nil {
			t.Fatal("expected error on cancelled queued exec, got nil")
		}
		if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "context canceled") {
			t.Errorf("expected context.Canceled error, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued exec did not return after cancellation")
	}

	// Release command 1
	cancel1()

	// Verify that command 2 never sent any bytes to the remote data channel
	receivedMu.Lock()
	defer receivedMu.Unlock()
	for _, msg := range receivedMessages {
		if strings.Contains(msg, "second_cmd_never_send") {
			t.Errorf("found queued command payload in received messages: %s", msg)
		}
	}
}

func TestTimeoutInvalidatesSession(t *testing.T) {
	pool := NewPool(nil)
	targetDev := &connect.DeviceInfo{
		ID:     "dev-canonical-123",
		Name:   "my-pi",
		Online: true,
	}
	pool.SetDeviceResolver(func(id string) (*connect.DeviceInfo, error) {
		return targetDev, nil
	})

	var connectCount int
	pool.SetConnector(func(target *connect.DeviceInfo) (*Session, error) {
		connectCount++
		pc1, pc2, dc1, _ := createLoopbackPair(t)
		t.Cleanup(func() {
			pc1.Close()
			pc2.Close()
		})
		return NewSession(target.Name, target.ID, pc1, dc1, make(chan string, 100)), nil
	})

	sess1, err := pool.GetOrOpen(context.Background(), "my-pi")
	if err != nil {
		t.Fatalf("first GetOrOpen failed: %v", err)
	}

	// Execute command with short timeout; dc2 does not respond, so it times out
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err = sess1.Exec(ctx, "echo test", 1000)
	if err == nil {
		t.Fatalf("expected Exec to fail due to timeout, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "deadline exceeded") {
		t.Errorf("expected deadline exceeded error, got: %v", err)
	}

	// Verify session is closed/invalidated
	if !sess1.IsClosed() {
		t.Errorf("expected session to be marked closed after timeout")
	}

	// Subsequent GetOrOpen must create a new session
	sess2, err := pool.GetOrOpen(context.Background(), "my-pi")
	if err != nil {
		t.Fatalf("second GetOrOpen failed: %v", err)
	}

	if sess1 == sess2 {
		t.Errorf("expected GetOrOpen to create a new session after timeout, but got identical instance")
	}
	if connectCount != 2 {
		t.Errorf("expected connectCount to be 2, got %d", connectCount)
	}
}

func TestCancelledActiveExecClosesBeforeReuse(t *testing.T) {
	pc1, pc2, dc1, dc2 := createLoopbackPair(t)
	defer pc1.Close()
	defer pc2.Close()

	outChan := make(chan string, 100)
	sess := NewSession("test-dev", "dev-123", pc1, dc1, outChan)
	defer sess.Close()

	commandDispatched := make(chan struct{})
	dc2.OnMessage(func(msg webrtc.DataChannelMessage) {
		select {
		case <-commandDispatched:
		default:
			close(commandDispatched)
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	errChan := make(chan error, 1)
	go func() {
		_, err := sess.Exec(ctx, "sleep 10", 1000)
		errChan <- err
	}()

	// Wait until command is actually dispatched over the network
	select {
	case <-commandDispatched:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for command dispatch")
	}

	// Cancel context while command is active in-flight
	cancel()

	select {
	case err := <-errChan:
		if err == nil {
			t.Fatal("expected Exec to return error on cancellation, got nil")
		}
		if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "context canceled") {
			t.Errorf("expected error wrapping context.Canceled, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Exec did not return after context cancellation")
	}

	// Assert session is closed
	if !sess.IsClosed() {
		t.Errorf("expected session to be closed after in-flight cancellation")
	}

	// Subsequent command on same session must fail immediately without hanging
	ctx2, cancel2 := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel2()
	_, err := sess.Exec(ctx2, "echo next", 1000)
	if err == nil {
		t.Fatal("expected subsequent Exec on cancelled session to fail, got nil")
	}
	if !strings.Contains(err.Error(), "session is closed") {
		t.Errorf("expected 'session is closed' error, got: %v", err)
	}
}

func TestAmbiguousSendFailureIsNotRetried(t *testing.T) {
	pc1, pc2, dc1, dc2 := createLoopbackPair(t)
	defer pc2.Close()

	var receivedCount int32
	dc2.OnMessage(func(msg webrtc.DataChannelMessage) {
		atomic.AddInt32(&receivedCount, 1)
	})

	sess := NewSession("test-dev", "dev-123", pc1, dc1, make(chan string, 10))
	defer sess.Close()

	// Close dc1 directly so sendTextLocked fails
	_ = dc1.Close()
	// Allow any close events to propagate
	time.Sleep(20 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := sess.Exec(ctx, "touch /tmp/critical_side_effect", 1000)
	if err == nil {
		t.Fatal("expected Exec to fail on closed datachannel, got nil")
	}

	// Must be marked closed
	if !sess.IsClosed() {
		t.Errorf("expected session to be marked closed after send failure")
	}

	// Verify no replay was attempted
	if count := atomic.LoadInt32(&receivedCount); count != 0 {
		t.Errorf("expected 0 commands received on remote channel, got %d", count)
	}
}
