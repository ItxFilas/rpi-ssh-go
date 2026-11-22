package terminal

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
)

var ansiEscapeRe = regexp.MustCompile(
	`\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)` + // OSC sequences (window title, hyperlinks, etc.) terminated by BEL or ST
		`|\x1b\[[0-9:;<=>?]*[ -/]*[@-~]` + // CSI escape sequences (colors, cursor, display clear, private modes)
		`|\x1b[()][A-Za-z0-9]|\x1b[=>]` + // Character set and keypad mode escapes
		`|[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]` + // C0 control chars (including BEL \x07, BS \x08) and DEL, preserving \t, \n, \r
		`|\a`, // Standalone BEL
)

// CleanTerminalOutput strips ANSI escape codes and normalizes newlines.
func CleanTerminalOutput(raw string) string {
	cleaned := ansiEscapeRe.ReplaceAllString(raw, "")
	cleaned = strings.ReplaceAll(cleaned, "\r\n", "\n")
	return strings.ReplaceAll(cleaned, "\r", "\n")
}

// TruncateOutput preserves head (30%) and tail (70%) of long output up to maxChars,
// appending a concise token-omission indicator in the middle.
func TruncateOutput(text string, maxChars int) string {
	runes := []rune(text)
	if maxChars <= 0 || len(runes) <= maxChars {
		return text
	}
	headSize := int(float64(maxChars) * 0.3)
	tailSize := maxChars - headSize
	omitted := len(runes) - maxChars
	return fmt.Sprintf("%s\n\n--- [Output truncated: %d characters truncated to save tokens] ---\n\n%s",
		string(runes[:headSize]), omitted, string(runes[len(runes)-tailSize:]))
}

// ShellQuote returns a safely single-quoted string for POSIX bash.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shellReservedWords open or continue a compound command. Spliced into the
// envelope line unencoded they would swallow the end marker.
var shellReservedWords = map[string]bool{
	"if": true, "then": true, "else": true, "elif": true, "fi": true,
	"case": true, "esac": true, "for": true, "select": true, "while": true,
	"until": true, "do": true, "done": true, "in": true, "function": true,
	"time": true, "coproc": true,
}

// isPlainCommandByte reports whether b can appear in a command that is sent
// to the interactive shell as typed. Everything else (quotes, expansions,
// globs, grouping, history expansion, control characters) is encoded.
func isPlainCommandByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	}
	switch b {
	case ' ', '_', '.', '/', ':', '=', ',', '+', '@', '%', '~', '-':
		return true
	}
	return false
}

// NeedsEncoding reports whether cmd must be base64-wrapped before it is typed
// into the interactive shell. Only commands made of plain words are sent as
// they are: an allow-list, because the envelope line is parsed as a whole and
// any construct the command leaves open would take the end marker with it.
func NeedsEncoding(cmd string) bool {
	for i := 0; i < len(cmd); i++ {
		if !isPlainCommandByte(cmd[i]) {
			return true
		}
	}
	for _, word := range strings.Fields(cmd) {
		if shellReservedWords[word] {
			return true
		}
	}
	return false
}

// WrapCommand wraps rawCmd inside a boundary envelope with startMarker and endMarkerPrefix.
// Anything but a command of plain words is base64-encoded and evaluated, so
// quoting, syntax errors and unfinished constructs stay inside the eval and
// the exact exit code is still reported.
// Keep the envelope on one terminal line so interactive prompts cannot enter stdout.
// Stdin is /dev/null: nobody can answer a prompt, so reads see end-of-file
// instead of blocking until the timeout.
func WrapCommand(startMarker, endMarkerPrefix, rawCmd string) string {
	if NeedsEncoding(rawCmd) {
		b64 := base64.StdEncoding.EncodeToString([]byte(rawCmd))
		return fmt.Sprintf("echo '%s'; eval \"$((printf '%%s' '%s') | base64 -d)\" </dev/null; __exit=$?; echo \"%s${__exit}__\"\n",
			startMarker, b64, endMarkerPrefix)
	}
	return fmt.Sprintf("echo '%s'; %s </dev/null; __exit=$?; echo \"%s${__exit}__\"\n",
		startMarker, rawCmd, endMarkerPrefix)
}
