package rtc

import (
	"bytes"
	"fmt"
	"strings"
	"unicode/utf8"
	"unsafe"
)

const (
	MaxRawReceiveBudget = 1048576 // 1 MiB (1 << 20)
	DefaultMaxChars     = 8000
	AbsoluteMaxChars    = 1000000
	MaxEnvelopeBytes    = 3500

	// MaxScriptCommandBytes is the longest command Exec accepts. Commands
	// that do not fit the terminal line are uploaded and run as a script.
	MaxScriptCommandBytes = 1 << 20

	// Initial buffer capacities; buffers grow on demand up to their limits.
	initialHeadCap = 256
	initialTailCap = 1024
)

type ansiState int

const (
	ansiStateNormal ansiState = iota
	ansiStateESC
	ansiStateCSI
	ansiStateOSC
	ansiStateCharset
)

type matchState int

const (
	matchStateSearchingStart matchState = iota
	matchStateCapturing
	matchStateReadingExitCode
	matchStateDone
)

type outputCollector struct {
	startMarker          string
	startMarkerBytes     []byte
	startMarkerRunes     []rune
	endMarkerPrefix      string
	endMarkerPrefixBytes []byte
	endMarkerPrefixRunes []rune
	maxChars             int
	maxBytes             int
	rawMode              bool

	matchState matchState

	// Normal mode (rawMode == false) state:
	ansiState              ansiState
	pendingCR              bool
	oscSawESC              bool
	ansiCount              int
	utf8Buf                []byte
	startWindow            []rune
	markerWindow           []rune
	pendingWhitespaceRunes []rune
	skipNewline            bool
	headLimit              int
	tailLimit              int
	headRunes              []rune
	tailRing               []rune
	tailIndex              int
	tailFull               bool
	totalRunes             int

	// Raw mode (rawMode == true) state:
	startWindowRaw []byte // unmatched tail carried over while searching for the start marker
	maxMarkerLen   int
	skipNewlineRaw int // 0: none, 1: saw start, check \r or \n, 2: saw \r, check \n
	rawStdout      []byte
	rawScan        int // rawStdout[:rawScan] is known to hold no marker start

	// Exit code parsing:
	exitCodeDigits      []byte
	exitCodeUnderscores int
	exitCode            int
}

func newOutputCollector(startMarker, endMarkerPrefix string, limit int, rawMode bool) *outputCollector {
	c := &outputCollector{
		startMarker:          startMarker,
		startMarkerBytes:     []byte(startMarker),
		startMarkerRunes:     []rune(startMarker),
		endMarkerPrefix:      endMarkerPrefix,
		endMarkerPrefixBytes: []byte(endMarkerPrefix),
		endMarkerPrefixRunes: []rune(endMarkerPrefix),
		rawMode:              rawMode,
		matchState:           matchStateSearchingStart,
		exitCodeDigits:       make([]byte, 0, 8),
	}

	if rawMode {
		c.maxBytes = limit
		c.rawStdout = make([]byte, 0, 1024)
		c.maxMarkerLen = max(len(c.startMarkerBytes), len(c.endMarkerPrefixBytes))
	} else {
		maxChars := limit
		if maxChars <= 0 {
			maxChars = DefaultMaxChars
		}
		if maxChars > AbsoluteMaxChars {
			maxChars = AbsoluteMaxChars
		}
		c.maxChars = maxChars
		c.headLimit = int(float64(maxChars) * 0.3)
		c.tailLimit = maxChars - c.headLimit
		c.headRunes = make([]rune, 0, min(c.headLimit, initialHeadCap))
		c.tailRing = make([]rune, 0, min(c.tailLimit, initialTailCap))
		c.startWindow = make([]rune, 0, len(c.startMarkerRunes)+1)
		maxMarkerLen := max(len(c.startMarkerRunes), len(c.endMarkerPrefixRunes))
		c.markerWindow = make([]rune, 0, maxMarkerLen+8)
		c.pendingWhitespaceRunes = make([]rune, 0, 8)
		c.utf8Buf = make([]byte, 0, 4)
	}

	return c
}

func (c *outputCollector) Feed(chunk []byte) (bool, error) {
	if c.rawMode {
		return c.feedRaw(chunk)
	}
	return c.feedNormal(chunk)
}

func (c *outputCollector) feedRaw(chunk []byte) (bool, error) {
	for len(chunk) > 0 && c.matchState != matchStateDone {
		switch c.matchState {
		case matchStateSearchingStart:
			chunk = c.rawSearchStart(chunk)

		case matchStateCapturing:
			rest, err := c.rawCapture(chunk)
			if err != nil {
				return false, err
			}
			chunk = rest

		case matchStateReadingExitCode:
			b := chunk[0]
			if b >= '0' && b <= '9' && c.exitCodeUnderscores == 0 && len(c.exitCodeDigits) < 5 {
				c.exitCodeDigits = append(c.exitCodeDigits, b)
				chunk = chunk[1:]
				continue
			}
			if b == '_' && len(c.exitCodeDigits) > 0 && c.exitCodeUnderscores < 2 {
				c.exitCodeUnderscores++
				if c.exitCodeUnderscores == 2 {
					exitCode := 0
					for _, d := range c.exitCodeDigits {
						exitCode = exitCode*10 + int(d-'0')
					}
					c.exitCode = exitCode
					c.matchState = matchStateDone
				}
				chunk = chunk[1:]
				continue
			}

			// Not an end marker after all: the bytes consumed so far are
			// ordinary output. Byte b is left for the capturing state.
			c.matchState = matchStateCapturing
			c.rawStdout = append(c.rawStdout, c.endMarkerPrefixBytes...)
			c.rawStdout = append(c.rawStdout, c.exitCodeDigits...)
			for u := 0; u < c.exitCodeUnderscores; u++ {
				c.rawStdout = append(c.rawStdout, '_')
			}
			c.exitCodeDigits = c.exitCodeDigits[:0]
			c.exitCodeUnderscores = 0
			c.rawScan = len(c.rawStdout)
			if c.maxBytes > 0 && len(c.rawStdout) > c.maxBytes {
				return false, ErrOutputLimit
			}
		}
	}

	return c.matchState == matchStateDone, nil
}

// rawSearchStart looks for the start marker and returns the bytes after it.
func (c *outputCollector) rawSearchStart(chunk []byte) []byte {
	c.startWindowRaw = append(c.startWindowRaw, chunk...)
	idx := bytes.Index(c.startWindowRaw, c.startMarkerBytes)
	if idx < 0 {
		// Keep only a tail that could still be the beginning of the marker.
		if keep := len(c.startMarkerBytes) - 1; len(c.startWindowRaw) > keep {
			n := copy(c.startWindowRaw, c.startWindowRaw[len(c.startWindowRaw)-keep:])
			c.startWindowRaw = c.startWindowRaw[:n]
		}
		return nil
	}
	after := len(c.startWindowRaw) - (idx + len(c.startMarkerBytes))
	c.startWindowRaw = c.startWindowRaw[:0]
	c.matchState = matchStateCapturing
	c.skipNewlineRaw = 1
	c.rawStdout = c.rawStdout[:0]
	c.rawScan = 0
	return chunk[len(chunk)-after:]
}

// rawCapture appends chunk to the output in bulk and scans only the new bytes
// (plus a marker-sized overlap) for a repeated start marker or the end marker.
// It returns the bytes that follow a matched marker, if any.
func (c *outputCollector) rawCapture(chunk []byte) ([]byte, error) {
	for c.skipNewlineRaw > 0 && len(chunk) > 0 {
		b := chunk[0]
		if c.skipNewlineRaw == 1 && b == '\r' {
			c.skipNewlineRaw = 2
			chunk = chunk[1:]
			continue
		}
		c.skipNewlineRaw = 0
		if b == '\n' {
			chunk = chunk[1:]
		}
	}
	if len(chunk) == 0 {
		return nil, nil
	}

	base := len(c.rawStdout)
	c.rawStdout = append(c.rawStdout, chunk...)
	from := max(base-(c.maxMarkerLen-1), c.rawScan)
	region := c.rawStdout[from:]
	startIdx := bytes.Index(region, c.startMarkerBytes)
	endIdx := bytes.Index(region, c.endMarkerPrefixBytes)

	// The marker that completes first wins, as in a byte-at-a-time scan.
	if startIdx >= 0 && (endIdx < 0 || startIdx+len(c.startMarkerBytes) < endIdx+len(c.endMarkerPrefixBytes)) {
		// Repeated start marker (e.g. terminal echo): discard what came before.
		consumed := from + startIdx + len(c.startMarkerBytes) - base
		c.rawStdout = c.rawStdout[:0]
		c.rawScan = 0
		c.skipNewlineRaw = 1
		return chunk[consumed:], nil
	}

	if endIdx >= 0 {
		outLen := from + endIdx
		consumed := outLen + len(c.endMarkerPrefixBytes) - base
		c.rawStdout = c.rawStdout[:outLen]
		c.rawScan = outLen
		if c.maxBytes > 0 && outLen > c.maxBytes {
			return nil, ErrOutputLimit
		}
		c.matchState = matchStateReadingExitCode
		c.exitCodeDigits = c.exitCodeDigits[:0]
		c.exitCodeUnderscores = 0
		return chunk[consumed:], nil
	}

	c.rawScan = max(c.rawScan, len(c.rawStdout)-(c.maxMarkerLen-1))
	// The last maxMarkerLen bytes may still turn out to be a marker.
	if c.maxBytes > 0 && len(c.rawStdout)-c.maxMarkerLen > c.maxBytes {
		return nil, ErrOutputLimit
	}
	return nil, nil
}

func (c *outputCollector) feedNormal(chunk []byte) (bool, error) {
	for i := 0; i < len(chunk); i++ {
		b := chunk[i]
		if c.matchState == matchStateDone {
			break
		}

		// Only a continuation byte can complete a buffered partial sequence.
		// Anything else ends it, so emit what is buffered first; otherwise
		// every later rune (including the end marker) would lag one byte.
		if len(c.utf8Buf) > 0 && b&0xC0 != 0x80 {
			c.flushUTF8()
		}

		// Handle pending carriage return for CRLF normalization
		if c.pendingCR {
			c.pendingCR = false
			if b == '\n' {
				c.emitRune('\n')
				continue
			}
			c.emitRune('\n')
			// Fall through to process byte b
		}

		if b == '\r' {
			c.pendingCR = true
			continue
		}

		switch c.ansiState {
		case ansiStateNormal:
			if b == 0x1b {
				c.ansiState = ansiStateESC
				c.ansiCount = 0
				continue
			}
			// Strip C0 control characters and DEL, preserving \t and \n
			if (b < 0x20 && b != '\t' && b != '\n') || b == 0x7f {
				continue
			}

			// UTF-8 decoding
			if len(c.utf8Buf) == 0 && b < 0x80 {
				c.emitRune(rune(b))
			} else {
				c.utf8Buf = append(c.utf8Buf, b)
				// Invalid bytes decode as a one-byte U+FFFD, which can leave
				// further complete runes behind them in the buffer.
				for len(c.utf8Buf) > 0 && utf8.FullRune(c.utf8Buf) {
					r, size := utf8.DecodeRune(c.utf8Buf)
					copy(c.utf8Buf, c.utf8Buf[size:])
					c.utf8Buf = c.utf8Buf[:len(c.utf8Buf)-size]
					c.emitRune(r)
				}
			}

		case ansiStateESC:
			c.ansiCount++
			if b == '[' {
				c.ansiState = ansiStateCSI
				continue
			}
			if b == ']' {
				c.ansiState = ansiStateOSC
				c.oscSawESC = false
				continue
			}
			if b == '(' || b == ')' {
				c.ansiState = ansiStateCharset
				continue
			}
			if b == '=' || b == '>' {
				c.ansiState = ansiStateNormal
				continue
			}
			if b == 0x1b {
				// Consecutive ESC, stay in ESC state
				continue
			}
			// Unrecognized 2-byte escape, return to normal
			c.ansiState = ansiStateNormal

		case ansiStateCSI:
			c.ansiCount++
			// Parameter bytes: 0x30..0x3F (0-9:;<=>?)
			// Intermediate bytes: 0x20..0x2F (space through /)
			// Final byte: 0x40..0x7E (@ through ~)
			if b >= 0x40 && b <= 0x7e {
				c.ansiState = ansiStateNormal
				continue
			}
			if (b >= 0x30 && b <= 0x3f) || (b >= 0x20 && b <= 0x2f) {
				if c.ansiCount > 256 {
					c.ansiState = ansiStateNormal
				}
				continue
			}
			if b == 0x1b {
				c.ansiState = ansiStateESC
				c.ansiCount = 0
				continue
			}
			c.ansiState = ansiStateNormal

		case ansiStateOSC:
			c.ansiCount++
			if b == 0x07 {
				c.ansiState = ansiStateNormal
				continue
			}
			if b == 0x1b {
				c.oscSawESC = true
				continue
			}
			if c.oscSawESC {
				c.oscSawESC = false
				if b == '\\' {
					c.ansiState = ansiStateNormal
					continue
				}
				if b == '[' {
					c.ansiState = ansiStateCSI
					continue
				}
				if b == ']' {
					c.ansiState = ansiStateOSC
					continue
				}
				c.ansiState = ansiStateNormal
				continue
			}
			if c.ansiCount > 4096 {
				c.ansiState = ansiStateNormal
			}

		case ansiStateCharset:
			// Discard 1 byte
			c.ansiState = ansiStateNormal
		}
	}

	return c.matchState == matchStateDone, nil
}

// flushUTF8 emits whatever is buffered as an incomplete UTF-8 sequence. Each
// undecodable byte becomes U+FFFD.
func (c *outputCollector) flushUTF8() {
	for off := 0; off < len(c.utf8Buf); {
		r, size := utf8.DecodeRune(c.utf8Buf[off:])
		off += size
		c.emitRune(r)
	}
	c.utf8Buf = c.utf8Buf[:0]
}

func (c *outputCollector) emitRune(r rune) {
	switch c.matchState {
	case matchStateSearchingStart:
		c.startWindow = append(c.startWindow, r)
		if len(c.startWindow) > len(c.startMarkerRunes) {
			copy(c.startWindow, c.startWindow[1:])
			c.startWindow = c.startWindow[:len(c.startWindow)-1]
		}
		if hasSuffixRunes(c.startWindow, c.startMarkerRunes) {
			c.matchState = matchStateCapturing
			c.skipNewline = true
			c.markerWindow = c.markerWindow[:0]
		}

	case matchStateCapturing:
		c.emitRuneCapturing(r)

	case matchStateReadingExitCode:
		if r >= '0' && r <= '9' {
			if c.exitCodeUnderscores == 0 && len(c.exitCodeDigits) < 5 {
				c.exitCodeDigits = append(c.exitCodeDigits, byte(r))
				return
			}
		} else if r == '_' {
			if len(c.exitCodeDigits) > 0 && c.exitCodeUnderscores < 2 {
				c.exitCodeUnderscores++
				if c.exitCodeUnderscores == 2 {
					exitCode := 0
					for _, d := range c.exitCodeDigits {
						exitCode = exitCode*10 + int(d-'0')
					}
					c.exitCode = exitCode
					c.matchState = matchStateDone
					c.pendingWhitespaceRunes = c.pendingWhitespaceRunes[:0]
				}
				return
			}
		}

		// Unexpected rune: invalid sequence / non-match.
		// Reset state back to capturing and flush accumulated pending whitespace, prefix, and digits.
		c.matchState = matchStateCapturing
		for _, w := range c.pendingWhitespaceRunes {
			c.appendOutputRune(w)
		}
		c.pendingWhitespaceRunes = c.pendingWhitespaceRunes[:0]
		for _, pr := range c.endMarkerPrefixRunes {
			c.appendOutputRune(pr)
		}
		for _, db := range c.exitCodeDigits {
			c.appendOutputRune(rune(db))
		}
		for u := 0; u < c.exitCodeUnderscores; u++ {
			c.appendOutputRune('_')
		}
		c.exitCodeDigits = c.exitCodeDigits[:0]
		c.exitCodeUnderscores = 0

		c.emitRuneCapturing(r)
	}
}

func (c *outputCollector) emitRuneCapturing(r rune) {
	if c.skipNewline {
		c.skipNewline = false
		if r == '\n' {
			return
		}
	}

	c.markerWindow = append(c.markerWindow, r)

	// Check for repeated start marker (e.g. command echo)
	if hasSuffixRunes(c.markerWindow, c.startMarkerRunes) {
		c.resetOutput()
		c.markerWindow = c.markerWindow[:0]
		c.skipNewline = true
		return
	}

	// Check for end marker prefix
	if hasSuffixRunes(c.markerWindow, c.endMarkerPrefixRunes) {
		prefixLen := len(c.markerWindow) - len(c.endMarkerPrefixRunes)
		// In normal mode, trim trailing whitespace before end marker (matching TrimSpace behavior)
		wsStart := prefixLen
		for wsStart > 0 && (c.markerWindow[wsStart-1] == '\n' || c.markerWindow[wsStart-1] == '\r' ||
			c.markerWindow[wsStart-1] == ' ' || c.markerWindow[wsStart-1] == '\t') {
			wsStart--
		}
		for i := 0; i < wsStart; i++ {
			c.appendOutputRune(c.markerWindow[i])
		}
		c.pendingWhitespaceRunes = append(c.pendingWhitespaceRunes[:0], c.markerWindow[wsStart:prefixLen]...)
		c.markerWindow = c.markerWindow[:0]
		c.matchState = matchStateReadingExitCode
		c.exitCodeDigits = c.exitCodeDigits[:0]
		c.exitCodeUnderscores = 0
		return
	}

	maxMarkerLen := max(len(c.startMarkerRunes), len(c.endMarkerPrefixRunes))
	if len(c.markerWindow) > maxMarkerLen {
		oldest := c.markerWindow[0]
		copy(c.markerWindow, c.markerWindow[1:])
		c.markerWindow = c.markerWindow[:len(c.markerWindow)-1]
		c.appendOutputRune(oldest)
	}
}

func (c *outputCollector) resetOutput() {
	c.headRunes = c.headRunes[:0]
	c.tailRing = c.tailRing[:0]
	c.tailIndex = 0
	c.tailFull = false
	c.totalRunes = 0
	if c.pendingWhitespaceRunes != nil {
		c.pendingWhitespaceRunes = c.pendingWhitespaceRunes[:0]
	}
}

func (c *outputCollector) appendOutputRune(r rune) {
	c.totalRunes++
	if len(c.headRunes) < c.headLimit {
		c.headRunes = append(c.headRunes, r)
		return
	}

	if len(c.tailRing) < c.tailLimit {
		c.tailRing = append(c.tailRing, r)
		return
	}

	c.tailRing[c.tailIndex] = r
	c.tailIndex = (c.tailIndex + 1) % c.tailLimit
	c.tailFull = true
}

// Partial returns what has been captured so far for a command that did not
// reach its end marker. The exit code is unknown and reported as -1. Raw
// captures are all-or-nothing and yield no text.
//
// It consumes the collector: runes held back while they could still have
// been part of a marker are released into the output.
func (c *outputCollector) Partial(device string) ExecResult {
	res := ExecResult{Device: device, ExitCode: -1}
	if c.rawMode || c.matchState == matchStateSearchingStart || c.matchState == matchStateDone {
		return res
	}

	c.flushUTF8()
	for _, r := range c.pendingWhitespaceRunes {
		c.appendOutputRune(r)
	}
	c.pendingWhitespaceRunes = c.pendingWhitespaceRunes[:0]
	for _, r := range c.markerWindow {
		c.appendOutputRune(r)
	}
	c.markerWindow = c.markerWindow[:0]

	collected := c.Result()
	// A terminal echoes the command line, markers included, before the
	// command prints anything. Seeing the end marker's prefix in the text
	// means only that echo arrived; it is not output.
	if strings.Contains(collected.Stdout, c.endMarkerPrefix) {
		return res
	}
	res.Stdout = collected.Stdout
	res.Truncated = collected.Truncated
	return res
}

func (c *outputCollector) Result() ExecResult {
	if c.rawMode {
		// rawStdout is handed over without a copy; the collector is done
		// with it once the end marker has been seen.
		stdout := unsafe.String(unsafe.SliceData(c.rawStdout), len(c.rawStdout))
		c.rawStdout = nil
		return ExecResult{
			ExitCode:  c.exitCode,
			Stdout:    stdout,
			Truncated: false,
		}
	}

	if c.totalRunes <= c.maxChars {
		allRunes := make([]rune, 0, c.totalRunes)
		allRunes = append(allRunes, c.headRunes...)
		if !c.tailFull {
			allRunes = append(allRunes, c.tailRing...)
		} else {
			allRunes = append(allRunes, c.tailRing[c.tailIndex:]...)
			allRunes = append(allRunes, c.tailRing[:c.tailIndex]...)
		}
		return ExecResult{
			ExitCode:  c.exitCode,
			Stdout:    strings.TrimSpace(string(allRunes)),
			Truncated: false,
		}
	}

	// Output exceeded maxChars: truncate head 30% and tail 70%
	headStr := string(c.headRunes)
	tailRunes := make([]rune, 0, c.tailLimit)
	if !c.tailFull {
		tailRunes = append(tailRunes, c.tailRing...)
	} else {
		tailRunes = append(tailRunes, c.tailRing[c.tailIndex:]...)
		tailRunes = append(tailRunes, c.tailRing[:c.tailIndex]...)
	}
	tailStr := string(tailRunes)
	omitted := c.totalRunes - c.maxChars

	stdout := fmt.Sprintf("%s\n\n--- [Output truncated: %d characters truncated to save tokens] ---\n\n%s",
		headStr, omitted, tailStr)

	return ExecResult{
		ExitCode:  c.exitCode,
		Stdout:    stdout,
		Truncated: true,
	}
}

func hasSuffixRunes(buf, suffix []rune) bool {
	if len(buf) < len(suffix) {
		return false
	}
	sub := buf[len(buf)-len(suffix):]
	for i, r := range suffix {
		if sub[i] != r {
			return false
		}
	}
	return true
}
