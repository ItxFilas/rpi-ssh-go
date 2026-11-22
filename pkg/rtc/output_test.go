package rtc

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"rpi-ssh-go/pkg/terminal"

	"github.com/pion/webrtc/v4"
)

func TestSplitMarkersAndANSI(t *testing.T) {
	startMarker := "__MCP_BEGIN_split123__"
	endMarkerPrefix := "__MCP_END_split123_"
	exitCode := 42
	fullStream := fmt.Sprintf("\x1b[32m%s\x1b[0m\r\n\x1b[31mHello \x1b[1;34mWorld\x1b[0m\r\n%s%d__\n",
		startMarker, endMarkerPrefix, exitCode)

	expectedStdout := "Hello World"

	endMarkerFull := fmt.Sprintf("%s%d__", endMarkerPrefix, exitCode)
	endMarkerIdx := strings.Index(fullStream, endMarkerFull)
	if endMarkerIdx == -1 {
		t.Fatalf("end marker not found in test stream")
	}
	endMarkerEnd := endMarkerIdx + len(endMarkerFull)

	for splitOffset := 1; splitOffset < len(fullStream); splitOffset++ {
		t.Run(fmt.Sprintf("split_at_%d", splitOffset), func(t *testing.T) {
			collector := newOutputCollector(startMarker, endMarkerPrefix, 1000, false)

			chunk1 := []byte(fullStream[:splitOffset])
			chunk2 := []byte(fullStream[splitOffset:])

			done1, err1 := collector.Feed(chunk1)
			if err1 != nil {
				t.Fatalf("unexpected error on chunk1 at offset %d: %v", splitOffset, err1)
			}

			if splitOffset < endMarkerEnd {
				if done1 {
					t.Fatalf("collector reported done prematurely on chunk1 at offset %d (endMarkerEnd=%d)",
						splitOffset, endMarkerEnd)
				}
				done2, err2 := collector.Feed(chunk2)
				if err2 != nil {
					t.Fatalf("unexpected error on chunk2 at offset %d: %v", splitOffset, err2)
				}
				if !done2 {
					t.Fatalf("collector did not complete after chunk2 at offset %d", splitOffset)
				}
			} else {
				if !done1 {
					t.Fatalf("collector should be done after chunk1 at offset %d (endMarkerEnd=%d)",
						splitOffset, endMarkerEnd)
				}
				done2, err2 := collector.Feed(chunk2)
				if err2 != nil {
					t.Fatalf("unexpected error on trailing chunk2 at offset %d: %v", splitOffset, err2)
				}
				if !done2 {
					t.Fatalf("collector should remain done after chunk2 at offset %d", splitOffset)
				}
			}

			res := collector.Result()
			if res.ExitCode != exitCode {
				t.Errorf("expected exit code %d, got %d", exitCode, res.ExitCode)
			}
			if res.Stdout != expectedStdout {
				t.Errorf("expected stdout %q, got %q", expectedStdout, res.Stdout)
			}
		})
	}
}

func TestCommandEchoDoesNotTriggerPrematureCompletion(t *testing.T) {
	startMarker := "__MCP_BEGIN_echo123__"
	endMarkerPrefix := "__MCP_END_echo123_"

	t.Run("EchoWithLiteralExitVariableInNormalMode", func(t *testing.T) {
		collector := newOutputCollector(startMarker, endMarkerPrefix, 8000, false)

		// Simulated virtual terminal output where command line echo contains ${__exit}__
		// followed by real command execution and real end marker.
		echoLine := fmt.Sprintf("echo \"%s${__exit}__\"\r\n", endMarkerPrefix)
		realOutput := "actual output line 1\r\nactual output line 2\r\n"
		endLine := fmt.Sprintf("%s0__\r\n", endMarkerPrefix)

		fullStream := startMarker + "\r\n" + echoLine + realOutput + endLine

		// Feed in 1-byte increments to test state machine transitions at every byte boundary
		for i := 0; i < len(fullStream); i++ {
			done, err := collector.Feed([]byte{fullStream[i]})
			if err != nil {
				t.Fatalf("unexpected error at byte %d (%q): %v", i, fullStream[i], err)
			}
			// Before reaching the terminal '__' of endLine, done must be false
			if i < len(fullStream)-3 && done {
				t.Fatalf("collector prematurely completed at byte offset %d (%q)", i, fullStream[:i+1])
			}
		}

		res := collector.Result()
		if res.ExitCode != 0 {
			t.Errorf("expected exit code 0, got %d", res.ExitCode)
		}
		if !strings.Contains(res.Stdout, "actual output line 1") {
			t.Errorf("expected stdout to contain actual output, got %q", res.Stdout)
		}
	})

	t.Run("EchoWithRepeatedStartMarkerCleanReset", func(t *testing.T) {
		collector := newOutputCollector(startMarker, endMarkerPrefix, 8000, false)

		// When the shell echoes the wrapped script:
		// Line 1 echo: echo '__MCP_BEGIN_echo123__'
		// Line 4 echo: echo "__MCP_END_echo123_${__exit}__"
		// Command execution line 1: prints __MCP_BEGIN_echo123__
		// Command execution line 2: actual command output
		// Command execution line 4: prints __MCP_END_echo123_0__
		stream := fmt.Sprintf("echo '%s'\r\necho \"%s${__exit}__\"\r\n%s\r\nreal command result\r\n%s0__\r\n",
			startMarker, endMarkerPrefix, startMarker, endMarkerPrefix)

		done, err := collector.Feed([]byte(stream))
		if err != nil {
			t.Fatalf("unexpected feed error: %v", err)
		}
		if !done {
			t.Fatalf("expected collector to complete")
		}

		res := collector.Result()
		if res.ExitCode != 0 {
			t.Errorf("expected exit code 0, got %d", res.ExitCode)
		}
		if res.Stdout != "real command result" {
			t.Errorf("expected stdout %q, got %q", "real command result", res.Stdout)
		}
	})

	t.Run("EchoInRawMode", func(t *testing.T) {
		collector := newOutputCollector(startMarker, endMarkerPrefix, 8000, true)

		echoLine := fmt.Sprintf("echo \"%s${__exit}__\"\r\n", endMarkerPrefix)
		realData := "raw binary or text data\n"
		endLine := fmt.Sprintf("%s0__\r\n", endMarkerPrefix)

		fullStream := startMarker + "\n" + echoLine + realData + endLine

		// Split chunk right inside the echoed ${__exit}__
		splitIdx := strings.Index(fullStream, "${__exit}") + 3
		chunk1 := []byte(fullStream[:splitIdx])
		chunk2 := []byte(fullStream[splitIdx:])

		done1, err1 := collector.Feed(chunk1)
		if err1 != nil {
			t.Fatalf("chunk1 feed error: %v", err1)
		}
		if done1 {
			t.Fatalf("collector prematurely completed on chunk1 with echoed ${__exit}")
		}

		done2, err2 := collector.Feed(chunk2)
		if err2 != nil {
			t.Fatalf("chunk2 feed error: %v", err2)
		}
		if !done2 {
			t.Fatalf("collector should be done after chunk2")
		}

		res := collector.Result()
		if res.ExitCode != 0 {
			t.Errorf("expected exit code 0, got %d", res.ExitCode)
		}
		if !strings.Contains(res.Stdout, realData) {
			t.Errorf("expected raw stdout to contain real data, got %q", res.Stdout)
		}
	})

	t.Run("NonDigitAndInvalidExitCodes", func(t *testing.T) {
		invalidCases := []struct {
			name     string
			badTail  string
			goodCode int
		}{
			{"LettersInsteadOfDigits", "abc__", 42},
			{"NoDigitsBeforeUnderscores", "__", 12},
			{"OverFiveDigits", "123456__", 7},
			{"DigitAfterUnderscore", "1_2__", 0},
			{"DollarBrace", "${__exit}__", 255},
		}

		for _, tc := range invalidCases {
			t.Run(tc.name, func(t *testing.T) {
				collector := newOutputCollector(startMarker, endMarkerPrefix, 8000, false)

				stream := fmt.Sprintf("%s\noutput line\n%s%s\nmore output\n%s%d__\n",
					startMarker, endMarkerPrefix, tc.badTail, endMarkerPrefix, tc.goodCode)

				// Feed first part including invalid sequence
				idx := strings.Index(stream, "more output")
				chunk1 := []byte(stream[:idx])
				chunk2 := []byte(stream[idx:])

				done1, err1 := collector.Feed(chunk1)
				if err1 != nil {
					t.Fatalf("unexpected error on chunk1: %v", err1)
				}
				if done1 {
					t.Fatalf("collector prematurely completed on invalid sequence %q", tc.badTail)
				}

				done2, err2 := collector.Feed(chunk2)
				if err2 != nil {
					t.Fatalf("unexpected error on chunk2: %v", err2)
				}
				if !done2 {
					t.Fatalf("collector failed to complete on valid end marker")
				}

				res := collector.Result()
				if res.ExitCode != tc.goodCode {
					t.Errorf("expected exit code %d, got %d", tc.goodCode, res.ExitCode)
				}
				if !strings.Contains(res.Stdout, "output line") || !strings.Contains(res.Stdout, "more output") {
					t.Errorf("expected output to be preserved, got %q", res.Stdout)
				}
			})
		}
	})
}

func TestUnicodeTruncation(t *testing.T) {
	startMarker := "__MCP_BEGIN_unicode__"
	endMarkerPrefix := "__MCP_END_unicode_"

	// 1. Multi-byte UTF-8 split across chunks without truncation
	t.Run("SplitMultiByteRune", func(t *testing.T) {
		collector := newOutputCollector(startMarker, endMarkerPrefix, 100, false)

		// Czech and Japanese multi-byte text: "Příliš žluťoučký kůň 日本語"
		text := "Příliš žluťoučký kůň 日本語"
		stream := fmt.Sprintf("%s\n%s\n%s0__\n", startMarker, text, endMarkerPrefix)
		raw := []byte(stream)

		// Split directly across a 3-byte Japanese rune boundary
		idx := strings.Index(stream, "日")
		if idx == -1 {
			t.Fatalf("rune 日 not found in stream")
		}

		// Split 1 byte into the 3-byte rune
		splitAt := idx + 1
		chunk1 := raw[:splitAt]
		chunk2 := raw[splitAt:]

		done1, err1 := collector.Feed(chunk1)
		if err1 != nil || done1 {
			t.Fatalf("chunk1 err=%v done=%v", err1, done1)
		}

		done2, err2 := collector.Feed(chunk2)
		if err2 != nil || !done2 {
			t.Fatalf("chunk2 err=%v done=%v", err2, done2)
		}

		res := collector.Result()
		if res.Stdout != text {
			t.Errorf("expected stdout %q, got %q", text, res.Stdout)
		}
		if res.Truncated {
			t.Errorf("expected Truncated=false, got true")
		}
		if !utf8.ValidString(res.Stdout) {
			t.Errorf("output contains invalid UTF-8 sequences")
		}
	})

	// 2. Truncation by rune count preserving exact Unicode characters (head 30%, tail 70%)
	t.Run("TruncationByRuneCount", func(t *testing.T) {
		maxChars := 10 // head 3, tail 7
		collector := newOutputCollector(startMarker, endMarkerPrefix, maxChars, false)

		// 20 Japanese runes (each 3 bytes in UTF-8): "一二三四五六七八九十甲乙丙丁戊己庚辛壬癸"
		runes := []rune("一二三四五六七八九十甲乙丙丁戊己庚辛壬癸")
		text := string(runes)
		stream := fmt.Sprintf("%s\n%s\n%s0__\n", startMarker, text, endMarkerPrefix)

		done, err := collector.Feed([]byte(stream))
		if err != nil || !done {
			t.Fatalf("Feed err=%v done=%v", err, done)
		}

		res := collector.Result()
		if !res.Truncated {
			t.Errorf("expected Truncated=true, got false")
		}
		if !utf8.ValidString(res.Stdout) {
			t.Errorf("output contains invalid UTF-8 sequences")
		}

		expectedHead := string(runes[:3])
		expectedTail := string(runes[len(runes)-7:])
		if !strings.HasPrefix(res.Stdout, expectedHead) {
			t.Errorf("expected head prefix %q, got %q", expectedHead, res.Stdout)
		}
		if !strings.HasSuffix(res.Stdout, expectedTail) {
			t.Errorf("expected tail suffix %q, got %q", expectedTail, res.Stdout)
		}

		omitted := len(runes) - maxChars // 20 - 10 = 10
		omittedMsg := fmt.Sprintf("%d characters truncated", omitted)
		if !strings.Contains(res.Stdout, omittedMsg) {
			t.Errorf("expected omission message containing %q, got: %s", omittedMsg, res.Stdout)
		}
	})
}

// While a command is running, output that cannot be queued is an error: the
// result would be silently incomplete otherwise.
func TestOutputOverflowFailsInsteadOfDropping(t *testing.T) {
	s := NewSession("test", "test", nil, nil, make(chan string, 2))
	defer s.beginCollecting()()
	s.enqueueOutput([]byte(strings.Repeat("x", MaxRawReceiveBudget)))
	if len(s.OutputChan) != 1 {
		t.Fatal("queue rejected its exact budget")
	}
	chunk := <-s.OutputChan
	s.consumedOutput(chunk)
	s.enqueueOutput([]byte(strings.Repeat("x", MaxRawReceiveBudget)))
	s.enqueueOutput([]byte("overflow"))
	if !errors.Is(s.outputCloseError(), ErrOutputOverflow) {
		t.Fatal("queue overflow not reported")
	}
	s.Close()
}

func TestLongEnvelopeRejectedBeforeSend(t *testing.T) {
	pc1, pc2, dc1, dc2 := createLoopbackPair(t)
	defer pc1.Close()
	defer pc2.Close()

	outputChan := make(chan string, 100)
	dc1.OnMessage(func(msg webrtc.DataChannelMessage) {
		outputChan <- string(msg.Data)
	})

	sess := NewSession("test-dev", "dev-123", pc1, dc1, outputChan)
	defer sess.Close()

	var bytesReceived int
	dc2.OnMessage(func(msg webrtc.DataChannelMessage) {
		bytesReceived += len(msg.Data)
	})

	// Create command that causes wrapped envelope to exceed 3,500 bytes
	longCmd := strings.Repeat("echo 'a';", 800) // ~8,000 bytes

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// ExecRaw sends exactly one terminal line and rejects what does not
	// fit. (Exec runs long commands as a script instead; see
	// TestLongCommandRunsAsScript.)
	_, err := sess.ExecRaw(ctx, longCmd, 1000)
	if err != ErrCommandTooLong {
		t.Fatalf("expected ErrCommandTooLong, got: %v", err)
	}

	// Sleep briefly to ensure zero bytes arrived
	time.Sleep(50 * time.Millisecond)
	if bytesReceived > 0 {
		t.Fatalf("expected zero bytes sent on over-limit envelope, got %d bytes", bytesReceived)
	}

	if sess.IsClosed() {
		t.Errorf("session should remain open after pre-send envelope rejection")
	}
}

func TestLargeOutputKeepsBoundedMemory(t *testing.T) {
	startMarker := "__MCP_BEGIN_mem__"
	endMarkerPrefix := "__MCP_END_mem_"

	collector := newOutputCollector(startMarker, endMarkerPrefix, 8000, false)
	_, _ = collector.Feed([]byte(startMarker + "\n"))

	// Force GC and take baseline heap
	runtime.GC()
	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)

	// Feed 900 KiB in 1 KiB chunks
	chunk := []byte(strings.Repeat("X", 1024))
	for i := 0; i < 900; i++ {
		_, err := collector.Feed(chunk)
		if err != nil {
			t.Fatalf("unexpected error feeding chunk %d: %v", i, err)
		}
	}

	_, err := collector.Feed([]byte("\n" + endMarkerPrefix + "0__\n"))
	if err != nil {
		t.Fatalf("unexpected error on end marker: %v", err)
	}

	runtime.GC()
	var m2 runtime.MemStats
	runtime.ReadMemStats(&m2)

	// Total retained memory delta should be far below the 900 KiB payload (e.g. < 200 KiB)
	if m2.HeapAlloc > m1.HeapAlloc {
		growth := m2.HeapAlloc - m1.HeapAlloc
		if growth > 250*1024 {
			t.Errorf("memory grew by %d bytes for 900 KiB payload; expected bounded retention", growth)
		}
	}

	res := collector.Result()
	if !res.Truncated {
		t.Errorf("expected Truncated=true")
	}
}

func TestSession_ExecRaw(t *testing.T) {
	pc1, pc2, dc1, dc2 := createLoopbackPair(t)
	defer pc1.Close()
	defer pc2.Close()

	outputChan := make(chan string, 100)
	dc1.OnMessage(func(msg webrtc.DataChannelMessage) {
		outputChan <- string(msg.Data)
	})

	sess := NewSession("test-dev", "dev-123", pc1, dc1, outputChan)
	defer sess.Close()

	t.Run("PreservesWhitespaceAndEmptyOutput", func(t *testing.T) {
		dc2.OnMessage(func(msg webrtc.DataChannelMessage) {
			cmd := string(msg.Data)
			if strings.Contains(cmd, "empty_test") {
				// Simulate empty output (between start and end markers)
				startMarker := extractMarker(cmd, "__MCP_BEGIN_([a-f0-9]+)__")
				reply := fmt.Sprintf("__MCP_BEGIN_%s__\n__MCP_END_%s_0__\n", startMarker, startMarker)
				_ = dc2.SendText(reply)
			} else if strings.Contains(cmd, "whitespace_test") {
				startMarker := extractMarker(cmd, "__MCP_BEGIN_([a-f0-9]+)__")
				// Whitespace with leading/trailing spaces and newlines
				reply := fmt.Sprintf("__MCP_BEGIN_%s__\n  hello \t world\n  __MCP_END_%s_0__\n", startMarker, startMarker)
				_ = dc2.SendText(reply)
			}
		})

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		// 1. Empty download test
		resEmpty, err := sess.ExecRaw(ctx, "empty_test", 10000)
		if err != nil {
			t.Fatalf("ExecRaw empty failed: %v", err)
		}
		if resEmpty.Stdout != "" {
			t.Errorf("expected empty stdout, got %q", resEmpty.Stdout)
		}

		// 2. Whitespace preservation test
		resWS, err := sess.ExecRaw(ctx, "whitespace_test", 10000)
		if err != nil {
			t.Fatalf("ExecRaw whitespace failed: %v", err)
		}
		expectedWS := "  hello \t world\n  "
		if resWS.Stdout != expectedWS {
			t.Errorf("expected stdout %q, got %q", expectedWS, resWS.Stdout)
		}
	})

	t.Run("EnforcesMaxBytesWithErrOutputLimit", func(t *testing.T) {
		dc2.OnMessage(func(msg webrtc.DataChannelMessage) {
			cmd := string(msg.Data)
			startMarker := extractMarker(cmd, "__MCP_BEGIN_([a-f0-9]+)__")
			// Reply with 500 bytes of output
			reply := fmt.Sprintf("__MCP_BEGIN_%s__\n%s__MCP_END_%s_0__\n",
				startMarker, strings.Repeat("A", 500), startMarker)
			_ = dc2.SendText(reply)
		})

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		// Request maxBytes=200, but payload is 500 bytes
		_, err := sess.ExecRaw(ctx, "limit_test", 200)
		if err != ErrOutputLimit {
			t.Fatalf("expected ErrOutputLimit, got: %v", err)
		}
	})
}

func extractMarker(cmd, pattern string) string {
	parts := strings.Split(cmd, "__MCP_BEGIN_")
	if len(parts) > 1 {
		sub := parts[1]
		idx := strings.Index(sub, "__")
		if idx != -1 {
			return sub[:idx]
		}
	}
	return "test"
}

func BenchmarkOutputStreaming(b *testing.B) {
	sizes := []struct {
		name string
		size int
	}{
		{"16KiB", 16 * 1024},
		{"64KiB", 64 * 1024},
		{"256KiB", 256 * 1024},
		{"1MiB", 1024*1024 - 200}, // stay safely below 1 MiB budget
	}

	startMarker := "__MCP_BEGIN_bench__"
	endMarkerPrefix := "__MCP_END_bench_"

	chunkSize := 1024

	for _, tc := range sizes {
		b.Run(tc.name, func(b *testing.B) {
			payload := strings.Repeat("A", tc.size)
			stream := fmt.Sprintf("%s\n%s\n%s0__\n", startMarker, payload, endMarkerPrefix)
			rawBytes := []byte(stream)

			b.ResetTimer()
			b.SetBytes(int64(tc.size))

			for i := 0; i < b.N; i++ {
				collector := newOutputCollector(startMarker, endMarkerPrefix, 8000, false)
				for offset := 0; offset < len(rawBytes); offset += chunkSize {
					end := offset + chunkSize
					if end > len(rawBytes) {
						end = len(rawBytes)
					}
					done, err := collector.Feed(rawBytes[offset:end])
					if err != nil {
						b.Fatalf("feed error: %v", err)
					}
					if done {
						break
					}
				}
				_ = collector.Result()
			}
		})
	}
}

// BenchmarkOldAccumulatedCleaning benchmarks the previous O(N^2) accumulated regex re-cleaning
// implementation on 256 KiB in 1 KiB chunks for direct comparison.
func BenchmarkOldAccumulatedCleaning(b *testing.B) {
	size := 256 * 1024
	chunkSize := 1024
	payload := strings.Repeat("A", size)
	startMarker := "__MCP_BEGIN_old__"
	endMarkerPrefix := "__MCP_END_old_"
	stream := fmt.Sprintf("%s\n%s\n%s0__\n", startMarker, payload, endMarkerPrefix)
	rawBytes := []byte(stream)

	b.ResetTimer()
	b.SetBytes(int64(size))

	for i := 0; i < b.N; i++ {
		var buf strings.Builder
		for offset := 0; offset < len(rawBytes); offset += chunkSize {
			end := offset + chunkSize
			if end > len(rawBytes) {
				end = len(rawBytes)
			}
			buf.Write(rawBytes[offset:end])
			// This was the old path: CleanTerminalOutput on entire buffer every chunk
			_ = terminal.CleanTerminalOutput(buf.String())
		}
	}
}

func TestStreamingBeyondReceiveQueueBudget(t *testing.T) {
	for _, raw := range []bool{false, true} {
		c := newOutputCollector("BEGIN", "END_", 2*MaxRawReceiveBudget+100, raw)
		if _, err := c.Feed([]byte("BEGIN\n")); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2048; i++ {
			if _, err := c.Feed([]byte(strings.Repeat("x", 1024))); err != nil {
				t.Fatalf("raw=%v chunk=%d: %v", raw, i, err)
			}
		}
		done, err := c.Feed([]byte("END_0__"))
		if err != nil || !done {
			t.Fatalf("done=%v err=%v", done, err)
		}
		if raw && len(c.Result().Stdout) != 2*MaxRawReceiveBudget {
			t.Fatal("raw output damaged")
		}
		if !raw && !c.Result().Truncated {
			t.Fatal("large display output should be truncated")
		}
	}
}
