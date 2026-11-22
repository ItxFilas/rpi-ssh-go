package rtc

import (
	"context"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

var tokenRe = regexp.MustCompile(`__MCP_UP_READY_([a-zA-Z0-9_-]+)__`)

func extractTokenFromReceiverCmd(cmd string) string {
	idx1 := strings.Index(cmd, "printf '%s' '")
	if idx1 < 0 {
		return ""
	}
	sub := cmd[idx1+len("printf '%s' '"):]
	idx2 := strings.Index(sub, "' | base64 -d")
	if idx2 < 0 {
		return ""
	}
	b64 := sub[:idx2]
	decoded, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return ""
	}
	m := tokenRe.FindSubmatch(decoded)
	if len(m) > 1 {
		return string(m[1])
	}
	return ""
}

func setupTestSession(t *testing.T) (*Session, *webrtc.DataChannel, func()) {
	t.Helper()
	pc1, pc2, dc1, dc2 := createLoopbackPair(t)

	outputChan := make(chan string, 1000)
	sess := NewSession("test-device", "dev-123", pc1, dc1, outputChan)

	dc1.OnMessage(func(msg webrtc.DataChannelMessage) {
		sess.enqueueOutput(msg.Data)
	})

	cleanup := func() {
		sess.Close()
		pc1.Close()
		pc2.Close()
	}

	return sess, dc2, cleanup
}

func TestAppendFileWaitsForReady(t *testing.T) {
	sess, dc2, cleanup := setupTestSession(t)
	defer cleanup()

	var mu sync.Mutex
	var receivedBeforeReady []string
	readySent := false
	var token string
	readyChan := make(chan struct{})

	dc2.OnMessage(func(msg webrtc.DataChannelMessage) {
		mu.Lock()
		defer mu.Unlock()
		text := string(msg.Data)

		if token == "" {
			token = extractTokenFromReceiverCmd(text)
			if token != "" {
				// Don't send READY immediately; verify that no payload is sent
				go func(tok string) {
					time.Sleep(50 * time.Millisecond)
					mu.Lock()
					readySent = true
					mu.Unlock()
					_ = dc2.SendText(fmt.Sprintf("__MCP_UP_READY_%s__\n", tok))
					close(readyChan)
				}(token)
				return
			}
		}

		if !readySent {
			receivedBeforeReady = append(receivedBeforeReady, text)
		} else {
			// Once ready, acknowledge and complete
			b64 := strings.TrimSpace(text)
			dec, _ := base64.StdEncoding.DecodeString(b64)
			_ = dc2.SendText(fmt.Sprintf("__MCP_UP_ACK_%s_%d__\n", token, len(dec)))
			_ = dc2.SendText(fmt.Sprintf("__MCP_UP_DONE_%s_0__\n", token))
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	content := []byte("hello world waiting for ready")
	err := sess.AppendFile(ctx, "/tmp/test.txt", content)
	if err != nil {
		t.Fatalf("AppendFile failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(receivedBeforeReady) > 0 {
		t.Errorf("received payload before READY was sent: %v", receivedBeforeReady)
	}
}

func TestAppendFileBoundsInFlight(t *testing.T) {
	sess, dc2, cleanup := setupTestSession(t)
	defer cleanup()

	// 128 KiB payload
	payloadSize := 128 * 1024
	content := make([]byte, payloadSize)
	for i := range content {
		content[i] = byte(i % 256)
	}

	var mu sync.Mutex
	var token string
	var totalSentDecoded int
	var lastAcked int
	var maxInFlight int
	var maxMessageLen int

	dc2.OnMessage(func(msg webrtc.DataChannelMessage) {
		mu.Lock()
		defer mu.Unlock()
		text := string(msg.Data)

		if len(text) > maxMessageLen {
			maxMessageLen = len(text)
		}

		if token == "" {
			token = extractTokenFromReceiverCmd(text)
			if token != "" {
				_ = dc2.SendText(fmt.Sprintf("__MCP_UP_READY_%s__\n", token))
				return
			}
		}

		// Decode payload lines
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			dec, err := base64.StdEncoding.DecodeString(line)
			if err == nil && len(dec) > 0 {
				totalSentDecoded += len(dec)
				inFlight := totalSentDecoded - lastAcked
				if inFlight > maxInFlight {
					maxInFlight = inFlight
				}

				// Acknowledge in 32 KiB increments
				if inFlight >= 32*1024 || totalSentDecoded == payloadSize {
					lastAcked = totalSentDecoded
					_ = dc2.SendText(fmt.Sprintf("__MCP_UP_ACK_%s_%d__\n", token, lastAcked))
					if lastAcked == payloadSize {
						_ = dc2.SendText(fmt.Sprintf("__MCP_UP_DONE_%s_0__\n", token))
					}
				}
			}
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := sess.AppendFile(ctx, "/tmp/large.bin", content)
	if err != nil {
		t.Fatalf("AppendFile failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	// Assert decoded in flight <= 32 KiB (+ 1 line tolerance = 33 KiB)
	if maxInFlight > 33*1024 {
		t.Errorf("max in flight decoded bytes %d exceeded 32 KiB limit", maxInFlight)
	}
	// Assert message sizes <= 16 KiB
	if maxMessageLen > 16*1024 {
		t.Errorf("max message size %d exceeded 16 KiB limit", maxMessageLen)
	}
}

func TestAppendFileCancellation(t *testing.T) {
	t.Run("CancelWaitingForReady", func(t *testing.T) {
		sess, dc2, cleanup := setupTestSession(t)
		defer cleanup()

		// Remote never sends READY
		dc2.OnMessage(func(msg webrtc.DataChannelMessage) {})

		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(50 * time.Millisecond)
			cancel()
		}()

		err := sess.AppendFile(ctx, "/tmp/cancel.txt", []byte("data"))
		if err == nil {
			t.Fatalf("expected error on cancellation, got nil")
		}
		if !strings.Contains(err.Error(), "context canceled") && !strings.Contains(err.Error(), "canceled") {
			t.Errorf("expected context cancellation error, got %v", err)
		}
		if !sess.IsClosed() {
			t.Errorf("expected session to be closed after cancellation")
		}
	})

	t.Run("CancelMidStreamNoLaterData", func(t *testing.T) {
		sess, dc2, cleanup := setupTestSession(t)
		defer cleanup()

		var mu sync.Mutex
		var token string
		var receivedCount int
		var dataAfterCancel int
		cancelled := int32(0)

		ctx, cancel := context.WithCancel(context.Background())

		dc2.OnMessage(func(msg webrtc.DataChannelMessage) {
			mu.Lock()
			defer mu.Unlock()
			text := string(msg.Data)

			if token == "" {
				token = extractTokenFromReceiverCmd(text)
				if token != "" {
					_ = dc2.SendText(fmt.Sprintf("__MCP_UP_READY_%s__\n", token))
					return
				}
			}

			receivedCount++
			if atomic.LoadInt32(&cancelled) == 1 {
				dataAfterCancel++
			}
			if receivedCount == 3 {
				atomic.StoreInt32(&cancelled, 1)
				cancel()
			}
		})

		content := make([]byte, 64*1024)
		err := sess.AppendFile(ctx, "/tmp/cancel_mid.bin", content)
		if err == nil {
			t.Fatalf("expected error on mid-stream cancel, got nil")
		}
		if !sess.IsClosed() {
			t.Errorf("expected session to be closed after mid-stream cancel")
		}

		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		defer mu.Unlock()
		if dataAfterCancel != 0 {
			t.Errorf("received %d payload messages after cancellation", dataAfterCancel)
		}
		if receivedCount != 3 {
			t.Errorf("expected one 32 KiB window (3 batches), got %d messages", receivedCount)
		}
	})
}

func TestAppendFileExcludesExecAndKeepalive(t *testing.T) {
	sess, dc2, cleanup := setupTestSession(t)
	defer cleanup()

	sess.StartKeepalive(10 * time.Millisecond)

	var mu sync.Mutex
	var receivedDuringUpload []string
	var token string
	uploadActive := true
	receiverCmdSeen := make(chan struct{})
	var onceReceiver sync.Once

	dc2.OnMessage(func(msg webrtc.DataChannelMessage) {
		mu.Lock()
		defer mu.Unlock()
		text := string(msg.Data)

		if uploadActive {
			receivedDuringUpload = append(receivedDuringUpload, text)
		}

		if token == "" {
			token = extractTokenFromReceiverCmd(text)
			if token != "" {
				onceReceiver.Do(func() { close(receiverCmdSeen) })
				// Sleep a little before sending ready so concurrent exec/keepalive have opportunities to attempt send
				time.Sleep(30 * time.Millisecond)
				_ = dc2.SendText(fmt.Sprintf("__MCP_UP_READY_%s__\n", token))
				return
			}
		}

		// Check if this is the exec command after upload completed
		if strings.Contains(text, "__MCP_BEGIN_") {
			startRe := regexp.MustCompile(`__MCP_BEGIN_([a-f0-9]+)__`)
			if m := startRe.FindStringSubmatch(text); len(m) > 1 {
				runID := m[1]
				_ = dc2.SendText(fmt.Sprintf("__MCP_BEGIN_%s__\nconcurrent\n__MCP_END_%s_0__\n", runID, runID))
				return
			}
		}

		// Acknowledge payload line slowly so keepalive/exec would have had a chance to interleave
		time.Sleep(10 * time.Millisecond)
		b64 := strings.TrimSpace(text)
		dec, _ := base64.StdEncoding.DecodeString(b64)
		_ = dc2.SendText(fmt.Sprintf("__MCP_UP_ACK_%s_%d__\n", token, len(dec)))
		_ = dc2.SendText(fmt.Sprintf("__MCP_UP_DONE_%s_0__\n", token))
	})

	content := []byte("payload data to protect against concurrency")
	appendDone := make(chan struct{})
	var appendErr error

	go func() {
		appendErr = sess.AppendFile(context.Background(), "/tmp/excl.txt", content)
		close(appendDone)
	}()

	// Wait until AppendFile has acquired gate and sent receiver command
	select {
	case <-receiverCmdSeen:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for AppendFile to send receiver command")
	}

	// Concurrent Exec while AppendFile is active
	execDone := make(chan struct{})
	var execRes ExecResult
	var execErr error

	go func() {
		execCtx, execCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer execCancel()
		execRes, execErr = sess.Exec(execCtx, "echo concurrent", 100)
		close(execDone)
	}()

	// Wait for AppendFile to finish
	select {
	case <-appendDone:
		if appendErr != nil {
			t.Fatalf("AppendFile failed: %v", appendErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for AppendFile to finish")
	}

	mu.Lock()
	uploadActive = false
	mu.Unlock()

	// Wait for Exec to complete
	select {
	case <-execDone:
		if execErr != nil {
			t.Fatalf("Exec failed: %v", execErr)
		}
		if !strings.Contains(execRes.Stdout, "concurrent") {
			t.Errorf("Exec did not produce expected output, got: %q", execRes.Stdout)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for Exec to complete after AppendFile")
	}

	mu.Lock()
	defer mu.Unlock()

	// Assert NO keepalive payload ("\x00") and NO exec command in receivedDuringUpload
	for _, msg := range receivedDuringUpload {
		if msg == "\x00" {
			t.Errorf("found NUL keepalive message during upload reception")
		}
		if strings.Contains(msg, "echo concurrent") {
			t.Errorf("found concurrent Exec command during upload reception")
		}
	}
}

func TestAppendFileReceiverFailure(t *testing.T) {
	t.Run("NonzeroExitCode", func(t *testing.T) {
		sess, dc2, cleanup := setupTestSession(t)
		defer cleanup()

		var token string
		dc2.OnMessage(func(msg webrtc.DataChannelMessage) {
			text := string(msg.Data)
			if token == "" {
				token = extractTokenFromReceiverCmd(text)
				if token != "" {
					_ = dc2.SendText(fmt.Sprintf("__MCP_UP_READY_%s__\n", token))
					return
				}
			}
			// Emit failure DONE marker
			_ = dc2.SendText(fmt.Sprintf("__MCP_UP_DONE_%s_1__\n", token))
		})

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		err := sess.AppendFile(ctx, "/tmp/fail.txt", []byte("some content"))
		if err == nil {
			t.Fatalf("expected error on receiver exit code 1, got nil")
		}
		if !strings.Contains(err.Error(), "exit code 1") {
			t.Errorf("expected exit code 1 in error, got: %v", err)
		}
	})

	t.Run("MalformedProtocolEventClosesSession", func(t *testing.T) {
		sess, dc2, cleanup := setupTestSession(t)
		defer cleanup()

		var token string
		dc2.OnMessage(func(msg webrtc.DataChannelMessage) {
			text := string(msg.Data)
			if token == "" {
				token = extractTokenFromReceiverCmd(text)
				if token != "" {
					_ = dc2.SendText(fmt.Sprintf("__MCP_UP_READY_%s__\n", token))
					return
				}
			}
			// Send malformed ACK event with non-numeric offset
			_ = dc2.SendText(fmt.Sprintf("__MCP_UP_ACK_%s_badnumber__\n", token))
		})

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		err := sess.AppendFile(ctx, "/tmp/malformed.txt", []byte("some content"))
		if err == nil {
			t.Fatalf("expected error on malformed protocol event, got nil")
		}
		if !sess.IsClosed() {
			t.Errorf("expected session to be closed on malformed protocol error")
		}
	})
}

func TestAppendFileEarlyFailureStopsPayload(t *testing.T) {
	sess, dc, cleanup := setupTestSession(t)
	defer cleanup()
	var payload atomic.Int32
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		token := extractTokenFromReceiverCmd(string(msg.Data))
		if token != "" {
			_ = dc.SendText(fmt.Sprintf("__MCP_UP_READY_%s__\n__MCP_UP_DONE_%s_1__\n", token, token))
			return
		}
		payload.Add(int32(len(msg.Data)))
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := sess.AppendFile(ctx, "/tmp/failure", make([]byte, 65536))
	if err == nil || !strings.Contains(err.Error(), "exit code 1") {
		t.Fatalf("expected receiver failure, got %v; sent %d bytes", err, payload.Load())
	}
	if payload.Load() != 0 {
		t.Fatalf("sent %d bytes after failure", payload.Load())
	}
	if !sess.IsClosed() {
		t.Fatal("failed receiver session must close")
	}
}

func TestAppendFileCancellationPhases(t *testing.T) {
	t.Run("WaitingForGate", func(t *testing.T) {
		sess, dc, cleanup := setupTestSession(t)
		defer cleanup()
		var messages atomic.Int32
		dc.OnMessage(func(webrtc.DataChannelMessage) { messages.Add(1) })
		gate := sess.gate()
		gate <- struct{}{}
		defer func() { <-gate }()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		if err := sess.AppendFile(ctx, "/tmp/not-sent", []byte("x")); err == nil {
			t.Fatal("expected cancellation")
		}
		if messages.Load() != 0 {
			t.Fatal("receiver dispatched while gate busy")
		}
		if sess.IsClosed() {
			t.Fatal("queued cancellation should leave active session intact")
		}
	})
	t.Run("WaitingForCompletion", func(t *testing.T) {
		sess, dc, cleanup := setupTestSession(t)
		defer cleanup()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var token string
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			if token == "" {
				token = extractTokenFromReceiverCmd(string(msg.Data))
				_ = dc.SendText(fmt.Sprintf("__MCP_UP_READY_%s__\n", token))
				return
			}
			_ = dc.SendText(fmt.Sprintf("__MCP_UP_ACK_%s_1__\n", token))
			cancel()
		})
		if err := sess.AppendFile(ctx, "/tmp/no-completion", []byte("x")); err == nil {
			t.Fatal("expected cancellation")
		}
		if !sess.IsClosed() {
			t.Fatal("must close incomplete receiver session")
		}
	})
}

func TestUploadBufferWaitCancellation(t *testing.T) {
	sess, _, cleanup := setupTestSession(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := sess.waitUploadBuffer(ctx, 1024, func() uint64 { return MaxLocalBufferBytes }, func(string) error { return nil })
	if err == nil || !sess.IsClosed() {
		t.Fatalf("buffer cancellation must fail and close: %v", err)
	}
}
func TestUploadBufferWaitPropagatesReceiverFailure(t *testing.T) {
	sess, _, cleanup := setupTestSession(t)
	defer cleanup()
	// Output is only queued while an operation is reading it.
	defer sess.beginCollecting()()
	sess.enqueueOutput([]byte("failure"))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := sess.waitUploadBuffer(ctx, 1024, func() uint64 { return MaxLocalBufferBytes }, func(string) error { return fmt.Errorf("receiver failure") })
	if err == nil || err.Error() != "receiver failure" {
		t.Fatalf("lost receiver error: %v", err)
	}
}
