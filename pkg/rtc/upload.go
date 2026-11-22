package rtc

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"rpi-ssh-go/pkg/terminal"
)

const (
	MaxInFlightDecodedBytes = 32 * 1024 // 32 KiB decoded in-flight limit
	MaxLocalBufferBytes     = 64 * 1024 // 64 KiB local WebRTC buffer limit
	MaxPayloadLineDecoded   = 1024      // 1,024 decoded bytes per line
	MaxMessageBytes         = 16 * 1024 // 16 KiB max WebRTC message limit
)

// AppendFile streams content to the remote staging file using the bounded receiver protocol.
// It acquires the single-operation execution gate, suppresses keepalives during reception,
// streams line-delimited base64 data under 32 KiB in-flight and 64 KiB local buffer backpressure,
// and completes only on verified receiver completion.
func (s *Session) AppendFile(ctx context.Context, stagingPath string, content []byte) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if s.IsClosed() {
		return fmt.Errorf("session is closed")
	}

	gate := s.gate()
	select {
	case gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.closeChan:
		return fmt.Errorf("session is closed")
	}
	defer func() { <-gate }()

	if ctx.Err() != nil {
		return ctx.Err()
	}
	if s.IsClosed() {
		return fmt.Errorf("session is closed")
	}

	// Coordinate keepalive: pause keepalive during streaming reception
	wasKeepaliveRunning := s.IsKeepaliveRunning()
	s.mu.Lock()
	keepaliveInterval := s.keepaliveInterval
	s.mu.Unlock()
	if wasKeepaliveRunning {
		s.StopKeepalive()
	}
	defer func() {
		if wasKeepaliveRunning && !s.IsClosed() {
			s.StartKeepalive(keepaliveInterval)
		}
	}()

	// Start reading output, dropping anything left over in the channel.
	defer s.beginCollecting()()

	token, err := generateUploadToken()
	if err != nil {
		return fmt.Errorf("generate upload token: %w", err)
	}

	cmd, err := terminal.UploadReceiverCommand(stagingPath, token, len(content))
	if err != nil {
		return fmt.Errorf("generate receiver command: %w", err)
	}

	if len(cmd) > MaxEnvelopeBytes {
		return ErrCommandTooLong
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Dispatch receiver command over DataChannel
	if err := s.sendTextLocked(cmd); err != nil {
		s.Close()
		return fmt.Errorf("send receiver command over WebRTC failed: %w", err)
	}

	proto, err := newUploadProtocol(token, len(content))
	if err != nil {
		s.Close()
		return fmt.Errorf("init upload protocol: %w", err)
	}

	// All receive paths must stop immediately on a terminal receiver failure.
	feedOutput := func(chunk string) error {
		s.consumedOutput(chunk)
		if err := proto.Feed([]byte(chunk)); err != nil {
			s.Close()
			return fmt.Errorf("upload protocol error: %w", err)
		}
		if proto.IsDone() && proto.ExitCode() != 0 {
			s.Close()
			return fmt.Errorf("upload receiver failed with exit code %d", proto.ExitCode())
		}
		return nil
	}

	// Helper to consume one output chunk from OutputChan
	readOutput := func() error {
		select {
		case <-ctx.Done():
			s.Close()
			return fmt.Errorf("upload cancelled or timed out: %w", ctx.Err())
		case <-s.closeChan:
			return s.outputCloseError()
		case chunk, ok := <-s.OutputChan:
			if !ok {
				s.Close()
				return fmt.Errorf("session output channel closed")
			}
			if err := feedOutput(chunk); err != nil {
				return err
			}
			return nil
		}
	}

	// Helper to drain any already-queued output without blocking
	drainAvailable := func() error {
		for {
			select {
			case <-ctx.Done():
				s.Close()
				return fmt.Errorf("upload cancelled or timed out: %w", ctx.Err())
			case <-s.closeChan:
				return s.outputCloseError()
			case chunk, ok := <-s.OutputChan:
				if !ok {
					s.Close()
					return fmt.Errorf("session output channel closed")
				}
				if err := feedOutput(chunk); err != nil {
					return err
				}
			default:
				return nil
			}
		}
	}

	// 1. Wait for READY
	for !proto.IsReady() {
		if err := readOutput(); err != nil {
			return err
		}
	}

	// 2. Stream payload lines with bounded backpressure
	sentDecodedBytes := 0
	for sentDecodedBytes < len(content) {
		if ctx.Err() != nil {
			s.Close()
			return fmt.Errorf("upload cancelled or timed out: %w", ctx.Err())
		}

		if err := drainAvailable(); err != nil {
			return err
		}

		// Build batch message up to 16 KiB and up to 32 KiB in flight
		var msgBuilder strings.Builder
		batchDecodedBytes := 0

		for (sentDecodedBytes + batchDecodedBytes) < len(content) {
			chunkEnd := (sentDecodedBytes + batchDecodedBytes) + MaxPayloadLineDecoded
			if chunkEnd > len(content) {
				chunkEnd = len(content)
			}
			chunkLen := chunkEnd - (sentDecodedBytes + batchDecodedBytes)

			// Backpressure check 1: In-flight decoded bytes <= 32 KiB
			if (sentDecodedBytes+batchDecodedBytes+chunkLen)-proto.AcknowledgedBytes() > MaxInFlightDecodedBytes {
				if batchDecodedBytes > 0 {
					break
				}
				for (sentDecodedBytes+chunkLen)-proto.AcknowledgedBytes() > MaxInFlightDecodedBytes {
					if err := readOutput(); err != nil {
						return err
					}
				}
			}

			chunk := content[sentDecodedBytes+batchDecodedBytes : chunkEnd]
			line := base64.StdEncoding.EncodeToString(chunk) + "\n"

			// Backpressure check 2: Message size <= 16 KiB
			if msgBuilder.Len() > 0 && msgBuilder.Len()+len(line) > MaxMessageBytes {
				break
			}

			msgBuilder.WriteString(line)
			batchDecodedBytes += chunkLen
		}

		if msgBuilder.Len() == 0 {
			continue
		}

		msg := msgBuilder.String()
		msgLen := uint64(len(msg))

		// Share the buffer wait with focused cancellation/error tests.
		if err := s.waitUploadBuffer(ctx, msgLen, func() uint64 {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.DataChannel != nil {
				return s.DataChannel.BufferedAmount()
			}
			return 0
		}, feedOutput); err != nil {
			return err
		}

		if ctx.Err() != nil {
			s.Close()
			return fmt.Errorf("upload cancelled or timed out: %w", ctx.Err())
		}

		if err := s.sendTextLocked(msg); err != nil {
			s.Close()
			return fmt.Errorf("send payload over WebRTC failed: %w", err)
		}

		sentDecodedBytes += batchDecodedBytes
	}

	// 3. Wait for final completion
	for !proto.IsDone() {
		if err := readOutput(); err != nil {
			return err
		}
	}

	if proto.ExitCode() != 0 {
		return fmt.Errorf("upload receiver failed with exit code %d", proto.ExitCode())
	}

	return nil
}

// waitUploadBuffer keeps consuming receiver events while waiting for SCTP space.
func (s *Session) waitUploadBuffer(ctx context.Context, msgLen uint64, buffered func() uint64, feed func(string) error) error {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for buffered()+msgLen > MaxLocalBufferBytes {
		select {
		case <-ctx.Done():
			s.Close()
			return fmt.Errorf("upload cancelled or timed out: %w", ctx.Err())
		case <-s.closeChan:
			return s.outputCloseError()
		case chunk, ok := <-s.OutputChan:
			if !ok {
				s.Close()
				return fmt.Errorf("session output channel closed")
			}
			if err := feed(chunk); err != nil {
				return err
			}
		case <-ticker.C:
		}
	}
	return nil
}
