package rtc

import (
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
)

func TestGenerateUploadToken(t *testing.T) {
	tok1, err := generateUploadToken()
	if err != nil {
		t.Fatalf("generateUploadToken failed: %v", err)
	}
	if len(tok1) != 32 {
		t.Fatalf("expected 32 hex chars (128 bits), got %d: %q", len(tok1), tok1)
	}
	if _, err := hex.DecodeString(tok1); err != nil {
		t.Fatalf("token is not valid hex: %v", err)
	}

	tok2, err := generateUploadToken()
	if err != nil {
		t.Fatalf("generateUploadToken failed: %v", err)
	}
	if tok1 == tok2 {
		t.Fatalf("expected unique tokens, got duplicate %q", tok1)
	}
}

func TestUploadProtocolSplitMarkers(t *testing.T) {
	token := "0123456789abcdef0123456789abcdef"
	totalBytes := 100000

	t.Run("SplitByteByByte", func(t *testing.T) {
		p, err := newUploadProtocol(token, totalBytes)
		if err != nil {
			t.Fatalf("newUploadProtocol failed: %v", err)
		}

		stream := fmt.Sprintf("\r\nsome startup banner\r\n__MCP_UP_READY_%s__\r\n"+
			"__MCP_UP_ACK_%s_32768__\r\n"+
			"__MCP_UP_ACK_%s_65536__\r\n"+
			"__MCP_UP_ACK_%s_100000__\r\n"+
			"__MCP_UP_DONE_%s_0__\r\n", token, token, token, token, token)

		for i := 0; i < len(stream); i++ {
			if err := p.Feed([]byte{stream[i]}); err != nil {
				t.Fatalf("Feed byte %d (%q) failed: %v", i, stream[i], err)
			}
		}

		if !p.IsReady() {
			t.Errorf("expected IsReady() to be true")
		}
		if p.AcknowledgedBytes() != 100000 {
			t.Errorf("expected AcknowledgedBytes() == 100000, got %d", p.AcknowledgedBytes())
		}
		if !p.IsDone() {
			t.Errorf("expected IsDone() to be true")
		}
		if p.ExitCode() != 0 {
			t.Errorf("expected ExitCode() == 0, got %d", p.ExitCode())
		}
	})

	t.Run("EchoedStartupTextNoFalseReady", func(t *testing.T) {
		p, err := newUploadProtocol(token, totalBytes)
		if err != nil {
			t.Fatalf("newUploadProtocol failed: %v", err)
		}

		// Echoed command text with a DIFFERENT token or fake marker prefix
		echoNoise := "\x1b[?2004huser@rpi:~$ eval \"$(printf '%s' 'base64' | base64 -d)\"\r\n" +
			"__MCP_UP_READY_other_fake_token_1234567890abcdef__\r\n" +
			"echo __MCP_UP_READY_" + token[:10] + "\r\n"

		if err := p.Feed([]byte(echoNoise)); err != nil {
			t.Fatalf("Feed echo noise failed: %v", err)
		}
		if p.IsReady() {
			t.Fatalf("expected false READY to be prevented on echoed noise, but IsReady() was true")
		}

		// Now feed the real READY marker
		realReady := fmt.Sprintf("__MCP_UP_READY_%s__\r\n", token)
		if err := p.Feed([]byte(realReady)); err != nil {
			t.Fatalf("Feed real ready failed: %v", err)
		}
		if !p.IsReady() {
			t.Fatalf("expected IsReady() to be true after real marker")
		}
	})

	t.Run("MonotonicAcknowledgementEnforcement", func(t *testing.T) {
		p, err := newUploadProtocol(token, totalBytes)
		if err != nil {
			t.Fatalf("newUploadProtocol: %v", err)
		}
		_ = p.Feed([]byte(fmt.Sprintf("__MCP_UP_READY_%s__\n", token)))
		_ = p.Feed([]byte(fmt.Sprintf("__MCP_UP_ACK_%s_32768__\n", token)))

		// Non-monotonic ACK (regressing or equal)
		err = p.Feed([]byte(fmt.Sprintf("__MCP_UP_ACK_%s_16384__\n", token)))
		if err == nil {
			t.Errorf("expected error on regressing ACK offset")
		}
	})

	t.Run("AckExceedsTotalBytes", func(t *testing.T) {
		p, err := newUploadProtocol(token, 1000)
		if err != nil {
			t.Fatalf("newUploadProtocol: %v", err)
		}
		_ = p.Feed([]byte(fmt.Sprintf("__MCP_UP_READY_%s__\n", token)))
		err = p.Feed([]byte(fmt.Sprintf("__MCP_UP_ACK_%s_1001__\n", token)))
		if err == nil {
			t.Errorf("expected error on ACK exceeding totalBytes")
		}
	})

	t.Run("AckBeforeReady", func(t *testing.T) {
		p, err := newUploadProtocol(token, totalBytes)
		if err != nil {
			t.Fatalf("newUploadProtocol: %v", err)
		}
		err = p.Feed([]byte(fmt.Sprintf("__MCP_UP_ACK_%s_100__\n", token)))
		if err == nil {
			t.Errorf("expected error on ACK before READY")
		}
	})

	t.Run("DoneBeforeReady", func(t *testing.T) {
		p, err := newUploadProtocol(token, totalBytes)
		if err != nil {
			t.Fatalf("newUploadProtocol: %v", err)
		}
		err = p.Feed([]byte(fmt.Sprintf("__MCP_UP_DONE_%s_0__\n", token)))
		if err == nil {
			t.Errorf("expected error on DONE before READY")
		}
	})

	t.Run("DonePrematureSuccess", func(t *testing.T) {
		p, err := newUploadProtocol(token, totalBytes)
		if err != nil {
			t.Fatalf("newUploadProtocol: %v", err)
		}
		_ = p.Feed([]byte(fmt.Sprintf("__MCP_UP_READY_%s__\n", token)))
		_ = p.Feed([]byte(fmt.Sprintf("__MCP_UP_ACK_%s_32768__\n", token)))
		// Reported exit code 0 while only 32768 of 100000 bytes acknowledged
		err = p.Feed([]byte(fmt.Sprintf("__MCP_UP_DONE_%s_0__\n", token)))
		if err == nil {
			t.Errorf("expected error on DONE exit code 0 when bytes remain unacknowledged")
		}
	})

	t.Run("EmptyPayloadReadyDone", func(t *testing.T) {
		p, err := newUploadProtocol(token, 0)
		if err != nil {
			t.Fatalf("newUploadProtocol(0): %v", err)
		}
		stream := fmt.Sprintf("__MCP_UP_READY_%s__\n__MCP_UP_DONE_%s_0__\n", token, token)
		if err := p.Feed([]byte(stream)); err != nil {
			t.Fatalf("Feed empty payload stream failed: %v", err)
		}
		if !p.IsReady() || !p.IsDone() || p.ExitCode() != 0 {
			t.Errorf("expected ready=true, done=true, exitCode=0 for empty payload; got ready=%v, done=%v, exit=%d",
				p.IsReady(), p.IsDone(), p.ExitCode())
		}
	})

	t.Run("NonzeroExitCode", func(t *testing.T) {
		p, err := newUploadProtocol(token, totalBytes)
		if err != nil {
			t.Fatalf("newUploadProtocol: %v", err)
		}
		_ = p.Feed([]byte(fmt.Sprintf("__MCP_UP_READY_%s__\n", token)))
		_ = p.Feed([]byte(fmt.Sprintf("__MCP_UP_DONE_%s_1__\n", token)))
		if !p.IsDone() || p.ExitCode() != 1 {
			t.Errorf("expected done=true, exitCode=1, got done=%v, exit=%d", p.IsDone(), p.ExitCode())
		}
	})

	t.Run("MalformedEventSyntax", func(t *testing.T) {
		p, err := newUploadProtocol(token, totalBytes)
		if err != nil {
			t.Fatalf("newUploadProtocol: %v", err)
		}
		_ = p.Feed([]byte(fmt.Sprintf("__MCP_UP_READY_%s__\n", token)))
		err = p.Feed([]byte(fmt.Sprintf("__MCP_UP_ACK_%s_notanumber__\n", token)))
		if err == nil || !strings.Contains(err.Error(), "malformed") {
			t.Errorf("expected malformed error on invalid ACK number, got %v", err)
		}
	})
}
