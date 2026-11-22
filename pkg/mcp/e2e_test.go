package mcp

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"rpi-ssh-go/pkg/connect"
	"rpi-ssh-go/pkg/rtc"
	"rpi-ssh-go/pkg/rtc/rtctest"
)

// newBashPool returns a pool whose devices are real bash shells in dir,
// reached over an in-process WebRTC connection. connectDelay simulates the
// time a cold connect takes.
func newBashPool(t *testing.T, dir string, connectDelay time.Duration) *rtc.Pool {
	t.Helper()
	pool := rtc.NewPool(nil)
	pool.SetConnector(func(target *connect.DeviceInfo) (*rtc.Session, error) {
		time.Sleep(connectDelay)
		pc1, _, dc1, dc2 := rtctest.LoopbackPair(t)
		rtctest.StartBashShell(t, dc2, dir)
		return rtc.AttachSession(target.Name, target.ID, pc1, dc1), nil
	})
	t.Cleanup(pool.Close)
	return pool
}

// callTool sends one tools/call request through the server and returns the
// text of the result and whether it was flagged as an error.
func callTool(t *testing.T, server *Server, name string, args map[string]any) (string, bool) {
	t.Helper()
	req, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]any{"name": name, "arguments": args},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Closing stdin cancels running requests, so keep it open until the
	// response line has been written.
	rPipe, wPipe := io.Pipe()
	var outMu sync.Mutex
	var out bytes.Buffer
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.Serve(rPipe, &threadSafeBuffer{b: &out, mu: &outMu})
	}()
	if _, err := wPipe.Write(append(req, '\n')); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		outMu.Lock()
		answered := strings.Contains(out.String(), "\n")
		outMu.Unlock()
		if answered || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = wPipe.Close()
	if err := <-serveDone; err != nil {
		t.Fatalf("Serve failed: %v", err)
	}

	var resp struct {
		Error  *JSONRPCError `json:"error"`
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp); err != nil {
		t.Fatalf("failed to parse response %q: %v", out.String(), err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected JSON-RPC error: %+v", resp.Error)
	}
	if len(resp.Result.Content) == 0 {
		t.Fatalf("response has no content: %q", out.String())
	}
	return resp.Result.Content[0].Text, resp.Result.IsError
}

// execResult is the JSON payload ssh_exec returns on success.
type execResult struct {
	Device    string `json:"device"`
	ExitCode  int    `json:"exit_code"`
	Stdout    string `json:"stdout"`
	Truncated bool   `json:"truncated"`
}

func parseExecResult(t *testing.T, text string) execResult {
	t.Helper()
	var res execResult
	if err := json.Unmarshal([]byte(text), &res); err != nil {
		t.Fatalf("ssh_exec result is not JSON: %q", text)
	}
	return res
}

// timeout_seconds limits the command, not the connection: a cold connect that
// takes longer than the command timeout must not fail the call.
func TestExecTimeoutStartsAfterConnect(t *testing.T) {
	server := NewServer()
	pool := newBashPool(t, t.TempDir(), 1500*time.Millisecond)
	RegisterRPiTools(server, nil, pool)

	text, isError := callTool(t, server, "ssh_exec", map[string]any{
		"device_name":     "pi",
		"command":         "echo hello",
		"timeout_seconds": 1,
	})
	if isError {
		t.Fatalf("command failed although it finished well inside its timeout: %s", text)
	}
	if res := parseExecResult(t, text); res.Stdout != "hello" || res.ExitCode != 0 {
		t.Fatalf("unexpected result: %+v", res)
	}
}

// A timed-out command reports what it printed before the timeout.
func TestExecTimeoutReportsPartialOutput(t *testing.T) {
	server := NewServer()
	RegisterRPiTools(server, nil, newBashPool(t, t.TempDir(), 0))

	text, isError := callTool(t, server, "ssh_exec", map[string]any{
		"device_name":     "pi",
		"command":         "echo migration started; sleep 30",
		"timeout_seconds": 1,
	})
	if !isError {
		t.Fatalf("expected a timeout error, got: %s", text)
	}
	if !strings.Contains(text, "timed out") {
		t.Errorf("error should say the command timed out: %s", text)
	}
	if !strings.Contains(text, "migration started") {
		t.Errorf("error should include the output printed before the timeout: %s", text)
	}
}
