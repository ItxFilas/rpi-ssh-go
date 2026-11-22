package rtc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"rpi-ssh-go/pkg/connect"
	"rpi-ssh-go/pkg/rtc/rtctest"

	"github.com/pion/webrtc/v4"
)

// newBashSession returns a session whose remote end is a real bash in dir.
func newBashSession(t *testing.T, dir string) *Session {
	t.Helper()
	pc1, _, dc1, dc2 := createLoopbackPair(t)
	rtctest.StartBashShell(t, dc2, dir)
	sess := AttachSession("test-pi", "dev-test", pc1, dc1)
	t.Cleanup(sess.Close)
	return sess
}

// Output that arrives while no command is running (a background job, the
// echo of a keepalive) belongs to nobody. It must not pile up until the
// receive budget is exhausted and the session is closed.
func TestIdleOutputIsDiscarded(t *testing.T) {
	sess := newBashSession(t, t.TempDir())

	chunk := []byte(strings.Repeat("noise from a background job\n", 1024))
	for sent := 0; sent < 3*MaxRawReceiveBudget; sent += len(chunk) {
		sess.enqueueOutput(chunk)
	}

	if sess.IsClosed() {
		t.Fatalf("idle output closed the session: %v", sess.outputCloseError())
	}
	if queued := len(sess.OutputChan); queued != 0 {
		t.Fatalf("%d chunks of idle output were queued", queued)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := sess.Exec(ctx, "echo still-alive", 1000)
	if err != nil || res.Stdout != "still-alive" {
		t.Fatalf("session unusable after idle output: %+v, %v", res, err)
	}
}

// A command too long for one terminal line is uploaded and run as a script in
// the same shell, so it behaves like any other command.
func TestLongCommandRunsAsScript(t *testing.T) {
	sess := newBashSession(t, t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	countTempFiles := func() string {
		res, err := sess.Exec(ctx, `ls "${TMPDIR:-/tmp}" | grep -c '^mcp_cmd\.' || true`, 1000)
		if err != nil {
			t.Fatalf("counting temp files failed: %v", err)
		}
		return res.Stdout
	}
	before := countTempFiles()

	var sb strings.Builder
	for i := 0; i < 900; i++ {
		fmt.Fprintf(&sb, "v%d=%d\n", i, i)
	}
	sb.WriteString("LONG_COMMAND_STATE=kept\n")
	sb.WriteString(`echo "first $v0 last $v899 <tag> & 'quotes'"` + "\n")
	sb.WriteString("(exit 7)\n")
	command := sb.String()
	if len(command) <= MaxEnvelopeBytes {
		t.Fatalf("test command is only %d bytes", len(command))
	}

	res, err := sess.Exec(ctx, command, 1000)
	if err != nil {
		t.Fatalf("long command failed: %v", err)
	}
	if res.Stdout != `first 0 last 899 <tag> & 'quotes'` {
		t.Errorf("unexpected stdout %q", res.Stdout)
	}
	if res.ExitCode != 7 {
		t.Errorf("exit code = %d, want 7", res.ExitCode)
	}

	// It ran in the persistent shell, not in a child process.
	state, err := sess.Exec(ctx, "echo $LONG_COMMAND_STATE", 1000)
	if err != nil || state.Stdout != "kept" {
		t.Errorf("shell state set by the long command was lost: %+v, %v", state, err)
	}

	if after := countTempFiles(); after != before {
		t.Errorf("script file was left behind: %s temp files before, %s after", before, after)
	}
}

func TestOversizedCommandIsRejected(t *testing.T) {
	sess := newBashSession(t, t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := sess.Exec(ctx, strings.Repeat("echo padding;", MaxScriptCommandBytes/13+1), 1000)
	if !errors.Is(err, ErrCommandTooLong) {
		t.Fatalf("expected ErrCommandTooLong, got: %v", err)
	}
	if sess.IsClosed() {
		t.Errorf("session should stay open after rejecting a command before sending it")
	}
}

// When a command times out, what it printed until then is the best clue to
// why. It is returned together with the error.
func TestTimeoutReturnsPartialOutput(t *testing.T) {
	sess := newBashSession(t, t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	res, err := sess.Exec(ctx, "echo step one done; echo waiting for the lock; sleep 30", 1000)
	if err == nil {
		t.Fatalf("expected a timeout, got %+v", res)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected a deadline error, got: %v", err)
	}
	if !strings.Contains(res.Stdout, "step one done") || !strings.Contains(res.Stdout, "waiting for the lock") {
		t.Fatalf("output printed before the timeout was not returned: %q", res.Stdout)
	}
	if strings.Contains(res.Stdout, "__MCP_") {
		t.Errorf("partial output contains protocol markers: %q", res.Stdout)
	}
}

// answeringClient completes the signalling with a peer that can never be
// reached, so the data channel stays closed after the answer is applied.
type answeringClient struct {
	t      *testing.T
	mu     sync.Mutex
	answer string
	onPoll func()
}

func (a *answeringClient) ListDevices(ctx context.Context, forceRefresh bool) ([]connect.DeviceInfo, error) {
	return nil, nil
}

func (a *answeringClient) GetShellSessionInfo(ctx context.Context, deviceID string) (connect.ICEConfig, string, error) {
	return connect.ICEConfig{}, "csrf", nil
}

func (a *answeringClient) PostOffer(ctx context.Context, deviceID, sdp, csrfToken, referer string) (string, error) {
	// The answering peer lives on a network of its own.
	remote := rtctest.NewPeer(a.t, rtctest.NewRouter(a.t), "10.77.0.9")
	if err := remote.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdp}); err != nil {
		return "", err
	}
	answer, err := remote.CreateAnswer(nil)
	if err != nil {
		return "", err
	}
	gathered := webrtc.GatheringCompletePromise(remote)
	if err := remote.SetLocalDescription(answer); err != nil {
		return "", err
	}
	<-gathered
	a.mu.Lock()
	a.answer = remote.LocalDescription().SDP
	a.mu.Unlock()
	return "conn-1", nil
}

func (a *answeringClient) PollAnswer(ctx context.Context, deviceID, connID, referer string, maxDuration time.Duration) (string, error) {
	if a.onPoll != nil {
		a.onPoll()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.answer, nil
}

// If the connection dies while the data channel is still opening, connecting
// has failed. There is no point in waiting out the rest of the open timeout.
func TestConnectFailsFastWhenConnectionDiesBeforeChannelOpens(t *testing.T) {
	pool := NewPool(nil)
	pool.SetDeviceResolver(func(identifier string) (*connect.DeviceInfo, error) {
		return &connect.DeviceInfo{ID: "dev-dies", Name: "dying-pi", Online: true}, nil
	})

	var pcMu sync.Mutex
	var offering *webrtc.PeerConnection
	pool.SetPeerConnectionFactory(func(cfg connect.ICEConfig, mode NetworkMode) (*webrtc.PeerConnection, error) {
		pc := newVirtualTestPeer(t, newTestRouter(t), "10.77.0.1")
		pcMu.Lock()
		offering = pc
		pcMu.Unlock()
		return pc, nil
	})

	client := &answeringClient{t: t}
	client.onPoll = func() {
		go func() {
			time.Sleep(300 * time.Millisecond)
			pcMu.Lock()
			pc := offering
			pcMu.Unlock()
			_ = pc.Close()
		}()
	}
	pool.SetClient(client)
	t.Setenv("RPI_NETWORK_MODE", "direct")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	start := time.Now()
	sess, err := pool.GetOrOpen(ctx, "dying-pi")
	elapsed := time.Since(start)
	if err == nil {
		sess.Close()
		t.Fatal("expected connecting to fail")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("connecting took %v to report a connection that died after 300ms: %v", elapsed, err)
	}
}
