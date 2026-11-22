package terminal

import (
	"bytes"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestCleanTerminalOutput(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "Strips ANSI color codes",
			input:    "\x1b[31mRed Text\x1b[0m and \x1b[1;32mGreen\x1b[0m",
			expected: "Red Text and Green",
		},
		{
			name:     "Normalizes carriage returns",
			input:    "Line 1\r\nLine 2\rLine 3\nLine 4",
			expected: "Line 1\nLine 2\nLine 3\nLine 4",
		},
		{
			name:     "Preserves UTF-8 international characters",
			input:    "\x1b[34mPříliš žluťoučký kůň úpěl ďábelské ódy\x1b[0m",
			expected: "Příliš žluťoučký kůň úpěl ďábelské ódy",
		},
		{
			name:     "Strips terminal control and bell characters",
			input:    "Beep\x07\x08Backspace\x1b[2KClear",
			expected: "BeepBackspaceClear",
		},
		{
			name:     "Strips OSC 0 window title terminated by BEL",
			input:    "\x1b]0;pi@raspberrypi: ~\x07output text",
			expected: "output text",
		},
		{
			name:     "Strips OSC 0 window title terminated by ST (ESC backslash)",
			input:    "\x1b]0;pi@raspberrypi: ~\x1b\\output text",
			expected: "output text",
		},
		{
			name:     "Strips OSC 8 hyperlink terminated by BEL",
			input:    "\x1b]8;;https://raspberrypi.com\x07Raspberry Pi Site\x1b]8;;\x07",
			expected: "Raspberry Pi Site",
		},
		{
			name:     "Strips OSC 8 hyperlink terminated by ST",
			input:    "\x1b]8;;https://raspberrypi.com\x1b\\Raspberry Pi Site\x1b]8;;\x1b\\",
			expected: "Raspberry Pi Site",
		},
		{
			name:     "Strips standalone BEL bell escapes",
			input:    "Alert\a\x07Sound",
			expected: "AlertSound",
		},
		{
			name:     "Strips CSI 25h/l cursor visibility and 2004h bracketed paste modes",
			input:    "\x1b[?2004hprompt$ \x1b[?25lhidden\x1b[?25h",
			expected: "prompt$ hidden",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			actual := CleanTerminalOutput(tc.input)
			if actual != tc.expected {
				t.Errorf("Expected %q, got %q", tc.expected, actual)
			}
		})
	}
}

func TestTruncateOutput(t *testing.T) {
	short := "Hello world"
	if TruncateOutput(short, 100) != short {
		t.Errorf("Expected short string to remain unchanged")
	}

	long := strings.Repeat("A", 1000) + strings.Repeat("Z", 1000)
	truncated := TruncateOutput(long, 500)

	if len(truncated) >= len(long) {
		t.Errorf("Truncated string should be shorter than original")
	}
	if !strings.Contains(truncated, "characters truncated") && !strings.Contains(truncated, "Output truncated:") {
		t.Errorf("Expected omission marker in truncated output, got: %s", truncated)
	}
	if !strings.HasPrefix(truncated, "AAAA") || !strings.HasSuffix(truncated, "ZZZZ") {
		t.Errorf("Expected head and tail to be preserved in truncated output")
	}
}

func TestNeedsEncoding(t *testing.T) {
	tests := []struct {
		name string
		cmd  string
		want bool
	}{
		{"Simple command", "ls -la /var/log", false},
		{"Simple command with flags", "uname -mrs", false},
		{"Simple ps aux", "ps aux", false},
		{"Single quote awk", "awk '{print $1}' /etc/hosts", true},
		{"Double quote echo", `echo "hello world"`, true},
		{"Multiline script", "echo line1\necho line2", true},
		{"Carriage return", "echo line1\recho line2", true},
		{"Backticks", "echo `date`", true},
		{"Variable expansion", "echo $HOME", true},
		{"Backslash escaping", "echo foo\\ bar", true},
		{"Heredoc", "cat << 'EOF'\nhello\nEOF", true},
		{"Pipes and semicolons", "echo a | grep a; echo b", true},
		{"Shell comment", "echo hello # comment", true},
		{"Control chars", "echo \x01\x02", true},
		{"Date format", "date +%s", false},
		{"Assignment", "FOO=bar", false},
		{"Home path", "ls ~/projects", false},
		{"History expansion", "echo done!!", true},
		{"Subshell parentheses", "echo (x)", true},
		{"Brace group", "{ ls", true},
		{"Open if construct", "if true", true},
		{"Open while construct", "while true", true},
		{"Reserved word as argument", "echo done", true},
		{"Glob", "ls *.txt", true},
		{"Bracket test", "[ -f x ]", true},
		{"Quick substitution", "^old^new", true},
		{"Tab", "echo\ta", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := NeedsEncoding(tc.cmd)
			if got != tc.want {
				t.Errorf("NeedsEncoding(%q) = %v, want %v", tc.cmd, got, tc.want)
			}
		})
	}
}

func TestShellQuote(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"simple", "'simple'"},
		{"path with spaces", "'path with spaces'"},
		{"bob's file.txt", "'bob'\\''s file.txt'"},
		{"", "''"},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := ShellQuote(tc.input)
			if got != tc.want {
				t.Errorf("ShellQuote(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestWrapCommand_Structure(t *testing.T) {
	startMarker := "__MCP_BEGIN_test123__"
	endMarkerPrefix := "__MCP_END_test123_"

	// 1. Simple command should not use base64
	simpleCmd := "ls -la"
	wrappedSimple := WrapCommand(startMarker, endMarkerPrefix, simpleCmd)
	if !strings.Contains(wrappedSimple, startMarker) {
		t.Errorf("expected wrappedSimple to contain startMarker")
	}
	if !strings.Contains(wrappedSimple, endMarkerPrefix) {
		t.Errorf("expected wrappedSimple to contain endMarkerPrefix")
	}
	if strings.Contains(wrappedSimple, "base64") {
		t.Errorf("simple command should not use base64 encoding")
	}

	// 2. Complex command with quotes and newlines should use base64
	complexCmd := `awk '{print $1}' file.txt`
	wrappedComplex := WrapCommand(startMarker, endMarkerPrefix, complexCmd)
	if !strings.Contains(wrappedComplex, startMarker) {
		t.Errorf("expected wrappedComplex to contain startMarker")
	}
	if !strings.Contains(wrappedComplex, endMarkerPrefix) {
		t.Errorf("expected wrappedComplex to contain endMarkerPrefix")
	}
	if !strings.Contains(wrappedComplex, "base64 -d") {
		t.Errorf("complex command should use base64 decoding wrapper")
	}
	if strings.Contains(wrappedComplex, `awk '{print $1}'`) {
		t.Errorf("complex command body should be base64-encoded, not present as raw unescaped text")
	}
}

func TestWrapCommand_ExecutionWithBash(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not found in PATH, skipping live execution test")
	}

	tests := []struct {
		name         string
		cmd          string
		wantExitCode int
		wantContains string
	}{
		{
			name:         "Simple echo command",
			cmd:          "echo hello_world",
			wantExitCode: 0,
			wantContains: "hello_world",
		},
		{
			name:         "Single quoted awk",
			cmd:          "printf 'alpha beta gamma\\n' | awk '{print $2}'",
			wantExitCode: 0,
			wantContains: "beta",
		},
		{
			name:         "Multiline shell script",
			cmd:          "printf 'line1\\n'\nprintf 'line2\\n'",
			wantExitCode: 0,
			wantContains: "line1\nline2",
		},
		{
			name:         "Python inline command with quotes and newlines",
			cmd:          "python3 -c \"import sys; sys.stdout.write('py_hello\\\\n'); sys.exit(0)\" 2>/dev/null || python -c \"import sys; sys.stdout.write('py_hello\\\\n'); sys.exit(0)\"",
			wantExitCode: 0,
			wantContains: "py_hello",
		},
		{
			name:         "Command with non-zero exit code 42",
			cmd:          "sh -c 'exit 42'",
			wantExitCode: 42,
			wantContains: "",
		},
		{
			name:         "Command with non-zero exit code 127",
			cmd:          "non_existent_command_xyz_12345 2>/dev/null",
			wantExitCode: 127,
			wantContains: "",
		},
	}

	startMarker := "__MCP_BEGIN_live__"
	endMarkerPrefix := "__MCP_END_live_"
	endPattern := regexp.MustCompile(endMarkerPrefix + `(\d+)__`)

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := WrapCommand(startMarker, endMarkerPrefix, tc.cmd)

			outBytes, err := exec.Command(bashPath, "-c", wrapped).CombinedOutput()
			// Note: bash itself might exit with 0 even if inner command exited with 42,
			// because echo of endMarker ran successfully afterwards.
			outStr := string(outBytes)

			cleaned := CleanTerminalOutput(outStr)

			matches := endPattern.FindStringSubmatch(cleaned)
			if len(matches) < 2 {
				t.Fatalf("end marker not found in output: %q (raw: %q, err: %v)", cleaned, outStr, err)
			}

			var exitCode int
			var code int
			for _, r := range matches[1] {
				code = code*10 + int(r-'0')
			}
			exitCode = code

			if exitCode != tc.wantExitCode {
				t.Errorf("exit code = %d, want %d (output: %q)", exitCode, tc.wantExitCode, cleaned)
			}

			if tc.wantContains != "" && !strings.Contains(cleaned, tc.wantContains) {
				t.Errorf("output %q does not contain expected substring %q", cleaned, tc.wantContains)
			}
		})
	}
}

func TestCommentPreservesCompletionMarker(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not found in PATH, skipping live execution test")
	}

	startMarker := "__MCP_BEGIN_comment__"
	endMarkerPrefix := "__MCP_END_comment_"
	endPattern := regexp.MustCompile(endMarkerPrefix + `(\d+)__`)

	tests := []struct {
		name         string
		cmd          string
		wantExitCode int
		wantContains string
	}{
		{
			name:         "Trailing comment after echo",
			cmd:          "echo hello # comment",
			wantExitCode: 0,
			wantContains: "hello",
		},
		{
			name:         "Trailing comment after complex command",
			cmd:          "printf 'alpha\\n' # a trailing shell comment",
			wantExitCode: 0,
			wantContains: "alpha",
		},
		{
			name:         "Trailing comment after failing command",
			cmd:          "sh -c 'exit 7' # exit failure comment",
			wantExitCode: 7,
			wantContains: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := WrapCommand(startMarker, endMarkerPrefix, tc.cmd)

			outBytes, err := exec.Command(bashPath, "-c", wrapped).CombinedOutput()
			outStr := string(outBytes)
			cleaned := CleanTerminalOutput(outStr)

			matches := endPattern.FindStringSubmatch(cleaned)
			if len(matches) < 2 {
				t.Fatalf("end marker was swallowed or missing in output: %q (raw: %q, err: %v)", cleaned, outStr, err)
			}

			var exitCode int
			for _, r := range matches[1] {
				exitCode = exitCode*10 + int(r-'0')
			}

			if exitCode != tc.wantExitCode {
				t.Errorf("exit code = %d, want %d (output: %q)", exitCode, tc.wantExitCode, cleaned)
			}
			if tc.wantContains != "" && !strings.Contains(cleaned, tc.wantContains) {
				t.Errorf("output %q does not contain %q", cleaned, tc.wantContains)
			}
		})
	}
}

// A command that is a syntax error or an unfinished construct must still
// print the end marker, otherwise the caller waits for the full timeout.
func TestSyntaxErrorsStillPrintCompletionMarker(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not found in PATH, skipping live execution test")
	}

	startMarker := "__MCP_BEGIN_syntax__"
	endMarkerPrefix := "__MCP_END_syntax_"
	endPattern := regexp.MustCompile(endMarkerPrefix + `(\d+)__`)

	for _, cmd := range []string{"echo (x)", "if true", "while true", "{ ls", "echo 'unterminated"} {
		t.Run(cmd, func(t *testing.T) {
			wrapped := WrapCommand(startMarker, endMarkerPrefix, cmd)
			outBytes, _ := exec.Command(bashPath, "-c", wrapped).CombinedOutput()
			cleaned := CleanTerminalOutput(string(outBytes))

			matches := endPattern.FindStringSubmatch(cleaned)
			if len(matches) < 2 {
				t.Fatalf("end marker missing, command would hang until timeout: %q", cleaned)
			}
			if matches[1] == "0" {
				t.Errorf("syntax error reported exit code 0: %q", cleaned)
			}
		})
	}
}

// Commands must not be able to wait on terminal input: nobody can answer a
// prompt, so reading stdin has to hit end-of-file straight away.
func TestCommandStdinIsNotTheTerminal(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not found in PATH, skipping live execution test")
	}

	startMarker := "__MCP_BEGIN_stdin__"
	endMarkerPrefix := "__MCP_END_stdin_"

	for _, cmd := range []string{"cat", "cat; echo 'after'"} {
		t.Run(cmd, func(t *testing.T) {
			wrapped := WrapCommand(startMarker, endMarkerPrefix, cmd)

			// Stdin is a pipe that never delivers data or end-of-file,
			// like an idle terminal.
			pr, pw, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer pr.Close()
			defer pw.Close()

			c := exec.Command(bashPath, "-c", wrapped)
			c.Stdin = pr
			var out bytes.Buffer
			c.Stdout = &out
			c.Stderr = &out
			if err := c.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- c.Wait() }()

			select {
			case <-done:
			case <-time.After(10 * time.Second):
				_ = c.Process.Kill()
				t.Fatalf("command blocked reading stdin; output so far: %q", out.String())
			}
			if !strings.Contains(out.String(), endMarkerPrefix+"0__") {
				t.Errorf("expected successful completion marker, got %q", out.String())
			}
		})
	}
}

func TestWrapCommandUsesOneTerminalLine(t *testing.T) {
	for _, cmd := range []string{"echo hello", "echo hello # comment", "printf 'one\\n'\nprintf 'two\\n'"} {
		wrapped := WrapCommand("BEGIN", "END_", cmd)
		if strings.Count(wrapped, "\n") != 1 || !strings.HasSuffix(wrapped, "\n") {
			t.Fatalf("interactive shell would echo prompts between markers: %q", wrapped)
		}
	}
}
