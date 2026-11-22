package rtc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unsafe"

	"rpi-ssh-go/pkg/terminal"

	"github.com/pion/webrtc/v4"
)

type ExecResult struct {
	Device    string
	ExitCode  int
	Stdout    string
	Truncated bool
}

var (
	ErrOutputOverflow = errors.New("command output exceeded receive buffer limit")
	ErrCommandTooLong = errors.New("command envelope exceeded line limit")
	ErrOutputLimit    = errors.New("raw output exceeded requested byte limit")
)

const (
	DefaultKeepaliveInterval = 45 * time.Second
	DefaultKeepalivePayload  = "\x00"

	DefaultPTYCols = 160
	DefaultPTYRows = 40
)

type StateOverrideFunc func() (webrtc.PeerConnectionState, webrtc.ICEConnectionState, webrtc.DataChannelState)

type Session struct {
	DeviceName     string
	DeviceID       string
	PeerConnection *webrtc.PeerConnection
	DataChannel    *webrtc.DataChannel
	OutputChan     chan string

	mu                sync.Mutex
	execGate          chan struct{}
	closed            bool
	transportDisposed bool
	queuedOutputBytes int
	collecting        bool // a command or transfer is reading OutputChan
	outputError       error
	closeChan         chan struct{}

	keepaliveInterval time.Duration
	keepaliveCancel   chan struct{}
	keepaliveDone     chan struct{}
	keepalivePayload  string

	stateOverride StateOverrideFunc
}

func NewSession(deviceName, deviceID string, pc *webrtc.PeerConnection, dc *webrtc.DataChannel, outputChan chan string) *Session {
	return &Session{
		DeviceName:        deviceName,
		DeviceID:          deviceID,
		PeerConnection:    pc,
		DataChannel:       dc,
		OutputChan:        outputChan,
		closeChan:         make(chan struct{}),
		execGate:          make(chan struct{}, 1),
		keepalivePayload:  DefaultKeepalivePayload,
		keepaliveInterval: DefaultKeepaliveInterval,
	}
}

// sessionOutputQueue is the number of unread data channel messages a session
// holds before the receive budget is considered exhausted.
const sessionOutputQueue = 1000

// AttachSession creates a session for the shell behind dc and routes the
// channel's output and close events into it.
func AttachSession(deviceName, deviceID string, pc *webrtc.PeerConnection, dc *webrtc.DataChannel) *Session {
	s := NewSession(deviceName, deviceID, pc, dc, make(chan string, sessionOutputQueue))
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		s.enqueueOutput(msg.Data)
	})
	dc.OnClose(func() {
		s.MarkClosed()
	})
	return s
}

func (s *Session) gate() chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.execGate == nil {
		s.execGate = make(chan struct{}, 1)
	}
	return s.execGate
}

func randomID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Session) SetStateOverride(fn StateOverrideFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stateOverride = fn
}

func (s *Session) SetKeepalivePayload(p string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keepalivePayload = p
}

func (s *Session) IsClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.isClosedLocked()
}

// isClosedLocked reports whether the session is unusable. "Disconnected" is
// not: ICE reports it for a short loss of connectivity and recovers on its
// own, or moves on to "failed" when it does not. Dropping the session then
// would throw away the shell's state for a network blip.
func (s *Session) isClosedLocked() bool {
	if s.closed {
		return true
	}

	if s.stateOverride != nil {
		pcState, iceState, dcState := s.stateOverride()
		if pcState == webrtc.PeerConnectionStateFailed ||
			pcState == webrtc.PeerConnectionStateClosed {
			s.markClosedLocked()
			return true
		}
		if iceState == webrtc.ICEConnectionStateFailed ||
			iceState == webrtc.ICEConnectionStateClosed {
			s.markClosedLocked()
			return true
		}
		if dcState == webrtc.DataChannelStateClosing ||
			dcState == webrtc.DataChannelStateClosed {
			s.markClosedLocked()
			return true
		}
		return false
	}

	if s.PeerConnection == nil {
		return true
	}

	pcState := s.PeerConnection.ConnectionState()
	if pcState == webrtc.PeerConnectionStateFailed ||
		pcState == webrtc.PeerConnectionStateClosed {
		s.markClosedLocked()
		return true
	}

	iceState := s.PeerConnection.ICEConnectionState()
	if iceState == webrtc.ICEConnectionStateFailed ||
		iceState == webrtc.ICEConnectionStateClosed {
		s.markClosedLocked()
		return true
	}

	if s.DataChannel != nil {
		dcState := s.DataChannel.ReadyState()
		if dcState == webrtc.DataChannelStateClosing ||
			dcState == webrtc.DataChannelStateClosed {
			s.markClosedLocked()
			return true
		}
	}

	return false
}

func (s *Session) MarkClosed() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markClosedLocked()
}

func (s *Session) markClosedLocked() {
	if !s.closed {
		s.closed = true
		select {
		case <-s.closeChan:
		default:
			close(s.closeChan)
		}
		s.stopKeepaliveLocked()
	}
}

func (s *Session) StartKeepalive(interval time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startKeepaliveLocked(interval)
}

func (s *Session) startKeepaliveLocked(interval time.Duration) {
	if s.closed {
		return
	}
	if interval <= 0 {
		interval = DefaultKeepaliveInterval
	}
	s.stopKeepaliveLocked()

	cancel := make(chan struct{})
	done := make(chan struct{})
	s.keepaliveCancel = cancel
	s.keepaliveDone = done
	s.keepaliveInterval = interval

	payload := s.keepalivePayload
	if payload == "" {
		payload = DefaultKeepalivePayload
	}

	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-cancel:
				return
			case <-ticker.C:
				s.mu.Lock()
				if s.closed || s.isClosedLocked() {
					s.mu.Unlock()
					return
				}
				dc := s.DataChannel
				s.mu.Unlock()

				// If command execution is currently active, channel is not idle
				gate := s.gate()
				select {
				case gate <- struct{}{}:
					if dc != nil && dc.ReadyState() == webrtc.DataChannelStateOpen {
						if err := dc.SendText(payload); err != nil {
							<-gate
							s.MarkClosed()
							return
						}
					}
					<-gate
				default:
					continue
				}
			}
		}
	}()
}

func (s *Session) StopKeepalive() {
	s.mu.Lock()
	done := s.keepaliveDone
	s.stopKeepaliveLocked()
	s.mu.Unlock()

	if done != nil {
		select {
		case <-done:
		case <-time.After(1 * time.Second):
		}
	}
}

func (s *Session) stopKeepaliveLocked() {
	if s.keepaliveCancel != nil {
		select {
		case <-s.keepaliveCancel:
		default:
			close(s.keepaliveCancel)
		}
		s.keepaliveCancel = nil
	}
}

func (s *Session) IsKeepaliveRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.keepaliveCancel == nil {
		return false
	}
	select {
	case <-s.keepaliveCancel:
		return false
	default:
		return true
	}
}

func (s *Session) Close() {
	var pc *webrtc.PeerConnection
	var dc *webrtc.DataChannel
	var outChan chan string
	var done chan struct{}

	s.mu.Lock()
	s.markClosedLocked()
	if !s.transportDisposed {
		s.transportDisposed = true
		pc = s.PeerConnection
		dc = s.DataChannel
		outChan = s.OutputChan
		done = s.keepaliveDone
	}
	s.mu.Unlock()

	// Wait briefly for keepalive goroutine to stop outside locks
	if done != nil {
		select {
		case <-done:
		case <-time.After(100 * time.Millisecond):
		}
	}

	// Dispose transport outside locks to avoid deadlocks with Pion callbacks
	if dc != nil {
		_ = dc.Close()
	}
	if pc != nil {
		_ = pc.Close()
	}
	if outChan != nil {
	drainLoop:
		for len(outChan) > 0 {
			select {
			case chunk := <-outChan:
				s.consumedOutput(chunk)
			default:
				break drainLoop
			}
		}
	}
}

// enqueueOutput bounds unread bytes, not the lifetime output of a command.
// Never block a Pion callback or close its transport while holding s.mu.
func (s *Session) enqueueOutput(data []byte) {
	s.mu.Lock()
	if s.closed || s.OutputChan == nil {
		s.mu.Unlock()
		return
	}
	// Nobody reads output between commands (background jobs, keepalive
	// echo). Queueing it would exhaust the budget and close the session.
	if !s.collecting {
		s.mu.Unlock()
		return
	}
	if s.queuedOutputBytes+len(data) <= MaxRawReceiveBudget {
		select {
		case s.OutputChan <- string(data):
			s.queuedOutputBytes += len(data)
			s.mu.Unlock()
			return
		default:
		}
	}
	s.outputError = ErrOutputOverflow
	s.markClosedLocked()
	s.mu.Unlock()
	go s.Close()
}

// beginCollecting marks the start of an operation that reads OutputChan and
// discards anything left over from before it. It returns the function that
// ends the operation. The caller must hold the execution gate.
func (s *Session) beginCollecting() (end func()) {
	s.mu.Lock()
	s.collecting = true
	s.mu.Unlock()

	if s.OutputChan != nil {
	drainLoop:
		for len(s.OutputChan) > 0 {
			select {
			case chunk := <-s.OutputChan:
				s.consumedOutput(chunk)
			default:
				break drainLoop
			}
		}
	}

	return func() {
		s.mu.Lock()
		s.collecting = false
		s.mu.Unlock()
	}
}

func (s *Session) consumedOutput(chunk string) {
	s.mu.Lock()
	s.queuedOutputBytes = max(0, s.queuedOutputBytes-len(chunk))
	s.mu.Unlock()
}
func (s *Session) outputCloseError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.outputError != nil {
		return s.outputError
	}
	return fmt.Errorf("session closed while waiting for command output")
}

func (s *Session) sendTextLocked(text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("session is closed")
	}
	if s.DataChannel == nil {
		return nil
	}
	return s.DataChannel.SendText(text)
}

// ConfigurePTY sends an stty command over the WebRTC DataChannel to adjust virtual terminal dimensions.
func (s *Session) ConfigurePTY(cols, rows int) error {
	if cols <= 0 {
		cols = DefaultPTYCols
	}
	if rows <= 0 {
		rows = DefaultPTYRows
	}
	return s.sendTextLocked(fmt.Sprintf("stty cols %d rows %d 2>/dev/null || true\n", cols, rows))
}

// InitPTY configures the terminal to the default 160x40 dimension.
func (s *Session) InitPTY() error {
	return s.ConfigurePTY(DefaultPTYCols, DefaultPTYRows)
}

// Exec executes a command on the remote shell over the WebRTC DataChannel.
// It acquires the single-operation gate in a context-aware manner, ensuring that
// if the context is cancelled before dispatch, no bytes are sent.
// If the command times out or is cancelled post-dispatch, the session is invalidated
// and closed to prevent subsequent commands from interacting with dirty shell state.
// Note that session closure does not guarantee remote process termination, and
// invalidating the session may reset persistent shell state.
func (s *Session) Exec(ctx context.Context, command string, maxChars int) (ExecResult, error) {
	if envelopeFits(command) {
		return s.execInternal(ctx, command, maxChars, false)
	}
	return s.execScript(ctx, command, maxChars)
}

// envelopeFits reports whether command, wrapped in its markers, fits on one
// terminal line.
func envelopeFits(command string) bool {
	runID := randomID()
	wrapped := terminal.WrapCommand(fmt.Sprintf("__MCP_BEGIN_%s__", runID), fmt.Sprintf("__MCP_END_%s_", runID), command)
	return len(wrapped) <= MaxEnvelopeBytes
}

// execScript runs a command that is too long for one terminal line: it is
// uploaded to a temporary file and sourced, so it runs in the persistent
// shell exactly as a short command would. The file is removed by the same
// command line that runs it.
func (s *Session) execScript(ctx context.Context, command string, maxChars int) (ExecResult, error) {
	if len(command) > MaxScriptCommandBytes {
		return ExecResult{}, fmt.Errorf("%w: command is %d bytes, the limit is %d", ErrCommandTooLong, len(command), MaxScriptCommandBytes)
	}

	created, err := s.execInternal(ctx, "mktemp -t mcp_cmd.XXXXXXXXXX", 4096, false)
	if err != nil {
		return ExecResult{}, fmt.Errorf("create script file for long command: %w", err)
	}
	scriptPath := strings.TrimSpace(created.Stdout)
	if created.ExitCode != 0 || !strings.HasPrefix(scriptPath, "/") || strings.ContainsAny(scriptPath, "\r\n") {
		return ExecResult{}, fmt.Errorf("create script file for long command: exit code %d: %s", created.ExitCode, scriptPath)
	}
	quoted := terminal.ShellQuote(scriptPath)

	if err := s.AppendFile(ctx, scriptPath, []byte(command)); err != nil {
		// A failed transfer removes its own file when it can; the session
		// may be gone, so this is only a second attempt.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = s.execInternal(cleanupCtx, "rm -f -- "+quoted, 1000, false)
		return ExecResult{}, fmt.Errorf("upload long command: %w", err)
	}

	run := fmt.Sprintf(". %s; __mcp_rc=$?; rm -f -- %s; (exit $__mcp_rc)", quoted, quoted)
	return s.execInternal(ctx, run, maxChars, false)
}

// ExecRaw executes a command on the remote shell and captures intact raw stdout
// without rune truncation. It enforces maxBytes by returning ErrOutputLimit if
// output exceeds maxBytes. Both paths use the same framing and gate serialization.
func (s *Session) ExecRaw(ctx context.Context, command string, maxBytes int) (ExecResult, error) {
	return s.execInternal(ctx, command, maxBytes, true)
}

func (s *Session) execInternal(ctx context.Context, command string, limit int, rawMode bool) (ExecResult, error) {
	if ctx.Err() != nil {
		return ExecResult{}, ctx.Err()
	}

	if s.IsClosed() {
		return ExecResult{}, fmt.Errorf("session is closed")
	}

	gate := s.gate()
	select {
	case gate <- struct{}{}:
	case <-ctx.Done():
		return ExecResult{}, ctx.Err()
	case <-s.closeChan:
		return ExecResult{}, fmt.Errorf("session is closed")
	}
	defer func() { <-gate }()

	if ctx.Err() != nil {
		return ExecResult{}, ctx.Err()
	}

	if s.IsClosed() {
		return ExecResult{}, fmt.Errorf("session is closed")
	}

	// Start reading output, dropping anything left over in the channel.
	defer s.beginCollecting()()

	runID := randomID()
	startMarker := fmt.Sprintf("__MCP_BEGIN_%s__", runID)
	endMarkerPrefix := fmt.Sprintf("__MCP_END_%s_", runID)

	wrappedCmd := terminal.WrapCommand(startMarker, endMarkerPrefix, command)

	// Enforce conservative 3,500 byte PTY line limit before send
	if len(wrappedCmd) > MaxEnvelopeBytes {
		return ExecResult{}, ErrCommandTooLong
	}

	if ctx.Err() != nil {
		return ExecResult{}, ctx.Err()
	}

	if err := s.sendTextLocked(wrappedCmd); err != nil {
		s.Close()
		// Do NOT replay after ambiguous send failure.
		return ExecResult{}, fmt.Errorf("send command over WebRTC failed: %w", err)
	}

	collector := newOutputCollector(startMarker, endMarkerPrefix, limit, rawMode)

	for {
		select {
		case <-ctx.Done():
			s.Close()
			// What the command printed so far is the best clue to why it
			// did not finish; hand it back with the error.
			return collector.Partial(s.DeviceName), fmt.Errorf("command execution cancelled or timed out: %w", ctx.Err())
		case <-s.closeChan:
			return collector.Partial(s.DeviceName), s.outputCloseError()
		case chunk, ok := <-s.OutputChan:
			if !ok {
				s.Close()
				return ExecResult{}, fmt.Errorf("session output channel closed")
			}
			s.consumedOutput(chunk)
			// View the chunk as bytes without copying; the collector only
			// reads it and copies whatever it keeps.
			done, err := collector.Feed(unsafe.Slice(unsafe.StringData(chunk), len(chunk)))
			if err != nil {
				s.Close()
				return ExecResult{}, err
			}
			if done {
				res := collector.Result()
				res.Device = s.DeviceName
				return res, nil
			}
		}
	}
}
