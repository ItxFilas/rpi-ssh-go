package terminal

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUploadReceiverRoundTrip(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not found on system")
	}

	testCases := []struct {
		name       string
		pathSuffix string
		content    []byte
	}{
		{
			name:       "AllByteValuesBinary64KiB",
			pathSuffix: "all_bytes.bin",
			content: func() []byte {
				data := make([]byte, 65536)
				for i := range data {
					data[i] = byte(i % 256)
				}
				return data
			}(),
		},
		{
			name:       "EmptyPayload",
			pathSuffix: "empty.bin",
			content:    []byte{},
		},
		{
			name:       "QuotedPathsWithSpacesAndSpecialChars",
			pathSuffix: "path with 'quotes' and spaces and -dashes.bin",
			content:    []byte("content for file with spaces and 'single' quotes"),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir, err := os.MkdirTemp("", "mcp_receiver_test_*")
			if err != nil {
				t.Fatalf("MkdirTemp failed: %v", err)
			}
			defer os.RemoveAll(tmpDir)

			stagingPath := filepath.Join(tmpDir, tc.pathSuffix)
			token := "0123456789abcdef0123456789abcdef"
			byteCount := len(tc.content)

			cmdStr, err := UploadReceiverCommand(stagingPath, token, byteCount)
			if err != nil {
				t.Fatalf("UploadReceiverCommand failed: %v", err)
			}

			cmd := exec.Command(bashPath, "-c", cmdStr)
			stdinPipe, err := cmd.StdinPipe()
			if err != nil {
				t.Fatalf("StdinPipe failed: %v", err)
			}
			var stdoutBuf bytes.Buffer
			cmd.Stdout = &stdoutBuf
			var stderrBuf bytes.Buffer
			cmd.Stderr = &stderrBuf

			if err := cmd.Start(); err != nil {
				t.Fatalf("cmd.Start failed: %v", err)
			}

			// Send content in line-delimited base64 chunks <= 1024 decoded bytes
			go func() {
				defer stdinPipe.Close()
				if len(tc.content) == 0 {
					return
				}
				for offset := 0; offset < len(tc.content); offset += 1024 {
					end := offset + 1024
					if end > len(tc.content) {
						end = len(tc.content)
					}
					chunk := tc.content[offset:end]
					b64 := base64.StdEncoding.EncodeToString(chunk)
					_, _ = fmt.Fprintf(stdinPipe, "%s\n", b64)
				}
			}()

			waitErr := cmd.Wait()
			stdoutStr := stdoutBuf.String()
			stderrStr := stderrBuf.String()

			if waitErr != nil {
				t.Fatalf("cmd.Wait failed: %v\nStdout: %s\nStderr: %s", waitErr, stdoutStr, stderrStr)
			}

			readyMarker := fmt.Sprintf("__MCP_UP_READY_%s__", token)
			doneMarker := fmt.Sprintf("__MCP_UP_DONE_%s_0__", token)

			if !strings.Contains(stdoutStr, readyMarker) {
				t.Errorf("stdout does not contain ready marker %q: %s", readyMarker, stdoutStr)
			}
			if !strings.Contains(stdoutStr, doneMarker) {
				t.Errorf("stdout does not contain done marker %q: %s", doneMarker, stdoutStr)
			}

			// Verify exact bytes on disk
			writtenBytes, err := os.ReadFile(stagingPath)
			if err != nil {
				t.Fatalf("ReadFile stagingPath failed: %v", err)
			}
			if !bytes.Equal(writtenBytes, tc.content) {
				t.Errorf("written bytes mismatch: expected %d bytes, got %d bytes", len(tc.content), len(writtenBytes))
			}
		})
	}
}

func TestUploadReceiverRestoresTTY(t *testing.T) {
	master, slave, err := openPTY()
	if err != nil {
		if errors.Is(err, errPTYUnsupported) {
			t.Skipf("skipping: %v", err)
		}
		t.Fatalf("openPTY failed: %v", err)
	}
	defer master.Close()
	defer slave.Close()

	initialEcho, err := getTerminalEcho(slave)
	if err != nil {
		t.Fatalf("getTerminalEcho failed: %v", err)
	}

	tmpDir, err := os.MkdirTemp("", "mcp_pty_test_*")
	if err != nil {
		t.Fatalf("MkdirTemp failed: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	stagingPath := filepath.Join(tmpDir, "test.bin")
	token := "0123456789abcdef0123456789abcdef"

	cmdStr, err := UploadReceiverCommand(stagingPath, token, 0)
	if err != nil {
		t.Fatalf("UploadReceiverCommand failed: %v", err)
	}

	cmd := exec.Command("bash", "-c", cmdStr)
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave

	if err := cmd.Run(); err != nil {
		t.Fatalf("cmd.Run failed: %v", err)
	}

	afterEcho, err := getTerminalEcho(slave)
	if err != nil {
		t.Fatalf("getTerminalEcho after run failed: %v", err)
	}
	if afterEcho != initialEcho {
		t.Errorf("terminal echo setting not restored: was %v, now %v", initialEcho, afterEcho)
	}
}

func TestUploadReceiverDecodeFailure(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not found on system")
	}

	tmpDir, err := os.MkdirTemp("", "mcp_receiver_fail_*")
	if err != nil {
		t.Fatalf("MkdirTemp failed: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	stagingPath := filepath.Join(tmpDir, "fail.bin")
	token := "0123456789abcdef0123456789abcdef"
	byteCount := 2048

	cmdStr, err := UploadReceiverCommand(stagingPath, token, byteCount)
	if err != nil {
		t.Fatalf("UploadReceiverCommand failed: %v", err)
	}

	cmd := exec.Command(bashPath, "-c", cmdStr)
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe failed: %v", err)
	}
	var stdoutBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf

	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start failed: %v", err)
	}

	// Send corrupt base64 line
	go func() {
		defer stdinPipe.Close()
		_, _ = fmt.Fprintln(stdinPipe, "!!!THIS_IS_CORRUPT_BASE64_NOT_VALID!!!")
	}()

	waitErr := cmd.Wait()
	stdoutStr := stdoutBuf.String()

	// Must fail with nonzero exit code or nonzero DONE marker
	doneSuccessMarker := fmt.Sprintf("__MCP_UP_DONE_%s_0__", token)
	if strings.Contains(stdoutStr, doneSuccessMarker) {
		t.Fatalf("corrupted input resulted in false success DONE marker: %s", stdoutStr)
	}

	if waitErr == nil && !strings.Contains(stdoutStr, fmt.Sprintf("__MCP_UP_DONE_%s_", token)) {
		t.Fatalf("expected command failure or non-zero DONE marker on decode error; waitErr was nil and no error DONE")
	}
}

func TestUploadReceiverRejectsFalseSuccess(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash fixture requires bash in PATH")
	}
	for _, tc := range []struct {
		name    string
		size    int
		input   string
		missing bool
	}{
		{"empty-write-failure", 0, "", true},
		{"short-valid-line", 1024, "YQ==\n", false},
		{"wrong-padding-length", 1024, strings.Repeat("A", 1368) + "\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.ToSlash(filepath.Join(t.TempDir(), "file"))
			if tc.missing {
				path = filepath.ToSlash(filepath.Join(t.TempDir(), "missing", "file"))
			}
			token := "0123456789abcdef0123456789abcdef"
			script, err := UploadReceiverCommand(path, token, tc.size)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, bashPath, "-c", script)
			cmd.Stdin = strings.NewReader(tc.input)
			out, err := cmd.CombinedOutput()
			if err == nil || strings.Contains(string(out), "DONE_"+token+"_0__") {
				t.Fatalf("false success: err=%v output=%s", err, out)
			}
			if !strings.Contains(string(out), "DONE_"+token+"_1__") {
				t.Fatalf("missing failure marker: %s", out)
			}
		})
	}
}

func TestUploadReceiverFailureConsumesPendingInput(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash fixture requires bash in PATH")
	}
	for _, diskFailure := range []bool{false, true} {
		t.Run(fmt.Sprint(diskFailure), func(t *testing.T) {
			path := filepath.ToSlash(filepath.Join(t.TempDir(), "file"))
			if diskFailure {
				path = filepath.ToSlash(t.TempDir())
			}
			token := "0123456789abcdef0123456789abcdef"
			script, err := UploadReceiverCommand(path, token, 1024)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			// After the receiver, emulate the persistent shell reading its next command.
			cmd := exec.CommandContext(ctx, bashPath, "-c", script+"; IFS= read -r next; eval \"$next\"")
			input := "YQ==\n"
			if diskFailure {
				input = base64.StdEncoding.EncodeToString(make([]byte, 1024)) + "\n"
			}
			cmd.Stdin = strings.NewReader(input + "echo PAYLOAD_ESCAPED_TO_SHELL\n")
			out, _ := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("receiver hung: %s", out)
			}
			if strings.Contains(string(out), "PAYLOAD_ESCAPED_TO_SHELL") {
				t.Fatalf("pending input escaped receiver: %s", out)
			}
			if !strings.Contains(string(out), "DONE_"+token+"_1__") {
				t.Fatalf("missing failure: %s", out)
			}
		})
	}
}

// A failed transfer must not leave its partial staging file behind: the
// session is usually gone by then, so nobody else can remove it.
func TestUploadReceiverRemovesStagingOnFailure(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash fixture requires bash in PATH")
	}
	token := "0123456789abcdef0123456789abcdef"

	for _, tc := range []struct {
		name  string
		input string
	}{
		{"corrupt payload", "!!!not base64!!!\n"},
		{"input ends early", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			staging := filepath.Join(t.TempDir(), ".staging.abc")
			if err := os.WriteFile(staging, nil, 0600); err != nil {
				t.Fatal(err)
			}
			script, err := UploadReceiverCommand(filepath.ToSlash(staging), token, 2048)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, bashPath, "-c", script)
			cmd.Stdin = strings.NewReader(tc.input)
			out, _ := cmd.CombinedOutput()
			if strings.Contains(string(out), "DONE_"+token+"_0__") {
				t.Fatalf("receiver reported success: %s", out)
			}
			if _, err := os.Stat(staging); !os.IsNotExist(err) {
				t.Fatalf("staging file left behind after failed transfer (stat err: %v); output: %s", err, out)
			}
		})
	}
}
