package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"rpi-ssh-go/pkg/rtc"
	"rpi-ssh-go/pkg/terminal"
)

type bashCommandExecutor struct {
	dir       string
	onCommand func(cmd string, res rtc.ExecResult)
}

func newBashExecutor(t *testing.T, dir string, onCmd func(cmd string, res rtc.ExecResult)) *bashCommandExecutor {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash fixture requires bash in PATH")
	}
	return &bashCommandExecutor{
		dir:       dir,
		onCommand: onCmd,
	}
}

func (b *bashCommandExecutor) Exec(ctx context.Context, command string, maxChars int) (rtc.ExecResult, error) {
	if ctx.Err() != nil {
		return rtc.ExecResult{}, ctx.Err()
	}
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	cmd.Dir = b.dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
			err = nil
		} else {
			return rtc.ExecResult{}, err
		}
	}
	out := stdout.String()
	if exitCode != 0 && out == "" {
		out = stderr.String()
	}
	res := rtc.ExecResult{
		Device:   "bash-fixture",
		ExitCode: exitCode,
		Stdout:   out,
	}
	if b.onCommand != nil {
		b.onCommand(command, res)
	}
	return res, nil
}

func (b *bashCommandExecutor) ExecRaw(ctx context.Context, command string, maxBytes int) (rtc.ExecResult, error) {
	res, err := b.Exec(ctx, command, maxBytes)
	if err != nil {
		return res, err
	}
	if maxBytes > 0 && len(res.Stdout) > maxBytes {
		return rtc.ExecResult{}, rtc.ErrOutputLimit
	}
	return res, nil
}

func (b *bashCommandExecutor) AppendFile(ctx context.Context, stagingPath string, content []byte) error {
	token := "0123456789abcdef0123456789abcdef"
	cmdStr, err := terminal.UploadReceiverCommand(stagingPath, token, len(content))
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "bash", "-c", cmdStr)
	cmd.Dir = b.dir
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Start(); err != nil {
		return err
	}

	go func() {
		defer stdinPipe.Close()
		if len(content) == 0 {
			return
		}
		for offset := 0; offset < len(content); offset += 1024 {
			end := offset + 1024
			if end > len(content) {
				end = len(content)
			}
			chunk := content[offset:end]
			b64 := base64.StdEncoding.EncodeToString(chunk)
			_, _ = fmt.Fprintf(stdinPipe, "%s\n", b64)
		}
	}()

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("receiver failed: %w (stdout: %s)", err, stdout.String())
	}
	return nil
}

type interceptingExecutor struct {
	underlying UploadExecutor
	onExec     func(ctx context.Context, cmd string) (*rtc.ExecResult, error)
	afterExec  func(ctx context.Context, cmd string, res rtc.ExecResult, err error)
	onAppend   func(ctx context.Context, stagingPath string, content []byte) error
}

func (i *interceptingExecutor) Exec(ctx context.Context, command string, maxChars int) (rtc.ExecResult, error) {
	if i.onExec != nil {
		if res, err := i.onExec(ctx, command); res != nil || err != nil {
			if res != nil {
				return *res, err
			}
			return rtc.ExecResult{}, err
		}
	}
	res, err := i.underlying.Exec(ctx, command, maxChars)
	if i.afterExec != nil {
		i.afterExec(ctx, command, res, err)
	}
	return res, err
}

func (i *interceptingExecutor) ExecRaw(ctx context.Context, command string, maxBytes int) (rtc.ExecResult, error) {
	if i.onExec != nil {
		if res, err := i.onExec(ctx, command); res != nil || err != nil {
			if res != nil {
				return *res, err
			}
			return rtc.ExecResult{}, err
		}
	}
	res, err := i.underlying.ExecRaw(ctx, command, maxBytes)
	if i.afterExec != nil {
		i.afterExec(ctx, command, res, err)
	}
	return res, err
}

func (i *interceptingExecutor) AppendFile(ctx context.Context, stagingPath string, content []byte) error {
	if i.onAppend != nil {
		if err := i.onAppend(ctx, stagingPath, content); err != nil {
			return err
		}
	}
	return i.underlying.AppendFile(ctx, stagingPath, content)
}

func TestConcurrentIdenticalUploadsHaveDistinctStaging(t *testing.T) {
	tmpDir := t.TempDir()
	identicalContent := bytes.Repeat([]byte("TransactionSafetyTestData1234567890\n"), 80) // ~2.8 KB (> 2 chunks)
	concurrency := 6

	var mu sync.Mutex
	stagingPaths := make(map[string]int)

	exec := newBashExecutor(t, tmpDir, func(cmd string, res rtc.ExecResult) {
		if strings.Contains(cmd, "mktemp") && res.ExitCode == 0 {
			stPath := strings.TrimSpace(res.Stdout)
			if stPath != "" {
				mu.Lock()
				stagingPaths[stPath]++
				mu.Unlock()
			}
		}
	})

	var wg sync.WaitGroup
	errs := make([]error, concurrency)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			destFile := fmt.Sprintf("concurrent_dest_%d.txt", idx)
			errs[idx] = uploadFile(context.Background(), exec, identicalContent, destFile)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Upload %d failed: %v", i, err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(stagingPaths) != concurrency {
		t.Fatalf("Expected %d distinct staging paths, got %d: %v", concurrency, len(stagingPaths), stagingPaths)
	}
	for p, count := range stagingPaths {
		if count > 1 {
			t.Errorf("Staging path %s was reused %d times", p, count)
		}
	}
}

func TestUploadTimeoutNeverSucceeds(t *testing.T) {
	tmpDir := t.TempDir()
	exec := newBashExecutor(t, tmpDir, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()
	time.Sleep(5 * time.Millisecond) // ensure context has expired

	content := []byte("some payload data")
	err := uploadFile(ctx, exec, content, "timeout_dest.txt")
	if err == nil {
		t.Fatal("Expected uploadFile to fail on timed out context, but claimed success")
	}

	// Verify destination file was never created
	if _, statErr := os.Stat(filepath.Join(tmpDir, "timeout_dest.txt")); !os.IsNotExist(statErr) {
		t.Errorf("Destination file exists after timeout failure: %v", statErr)
	}
}

func TestUploadHashMismatchPreservesDestination(t *testing.T) {
	tmpDir := t.TempDir()
	origContent := []byte("original protected destination content")
	destPath := filepath.Join(tmpDir, "dest_mismatch.txt")
	if err := os.WriteFile(destPath, origContent, 0644); err != nil {
		t.Fatalf("Failed to write initial destination: %v", err)
	}

	var mu sync.Mutex
	var createdStaging string

	exec := newBashExecutor(t, tmpDir, nil)
	tamperedExec := &interceptingExecutor{
		underlying: exec,
		afterExec: func(ctx context.Context, cmd string, res rtc.ExecResult, err error) {
			if strings.Contains(cmd, "mktemp") && res.ExitCode == 0 {
				mu.Lock()
				createdStaging = strings.TrimSpace(res.Stdout)
				mu.Unlock()
			}
		},
		onExec: func(ctx context.Context, cmd string) (*rtc.ExecResult, error) {
			if strings.Contains(cmd, "sha256sum") {
				// Tamper by returning mismatched checksum
				return &rtc.ExecResult{
					ExitCode: 0,
					Stdout:   "0000000000000000000000000000000000000000000000000000000000000000  staging\n",
				}, nil
			}
			return nil, nil
		},
	}

	err := uploadFile(context.Background(), tamperedExec, []byte("new corrupted upload content"), "dest_mismatch.txt")
	if err == nil {
		t.Fatal("Expected error on hash mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "mismatch") && !strings.Contains(err.Error(), "sha256") {
		t.Errorf("Expected error to mention mismatch/sha256, got: %v", err)
	}

	// Destination must be completely untouched
	readBack, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("Failed to read back destination: %v", err)
	}
	if !bytes.Equal(readBack, origContent) {
		t.Errorf("Destination was modified on hash mismatch! Got %q, want %q", string(readBack), string(origContent))
	}

	// Staging file must be cleaned up
	mu.Lock()
	stPath := createdStaging
	mu.Unlock()
	if stPath != "" {
		cleanPath := filepath.Join(tmpDir, filepath.FromSlash(strings.TrimPrefix(stPath, "./")))
		if _, statErr := os.Stat(cleanPath); !os.IsNotExist(statErr) {
			t.Errorf("Staging file %s was not cleaned up after hash mismatch!", cleanPath)
		}
	}
}

func TestUploadCancellationBeforePublishPreservesDestination(t *testing.T) {
	tmpDir := t.TempDir()
	origContent := []byte("original destination content before cancel")
	destPath := filepath.Join(tmpDir, "dest_cancel.txt")
	if err := os.WriteFile(destPath, origContent, 0644); err != nil {
		t.Fatalf("Failed to write initial destination: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var createdStaging string

	exec := newBashExecutor(t, tmpDir, nil)
	tamperedExec := &interceptingExecutor{
		underlying: exec,
		afterExec: func(c context.Context, cmd string, res rtc.ExecResult, err error) {
			if strings.Contains(cmd, "mktemp") && res.ExitCode == 0 {
				mu.Lock()
				createdStaging = strings.TrimSpace(res.Stdout)
				mu.Unlock()
			}
			if strings.Contains(cmd, "sha256sum") {
				// Cancel context right after sha256 check, before publish dispatch!
				cancel()
			}
		},
	}

	err := uploadFile(ctx, tamperedExec, []byte("never published content"), "dest_cancel.txt")
	if err == nil {
		t.Fatal("Expected error on cancelled context, got nil")
	}
	if strings.Contains(err.Error(), "upload publish outcome unknown") {
		t.Errorf("Cancellation before dispatch should not report outcome unknown, got: %v", err)
	}

	// Destination must be completely untouched
	readBack, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("Failed to read back destination: %v", err)
	}
	if !bytes.Equal(readBack, origContent) {
		t.Errorf("Destination was modified on cancellation! Got %q, want %q", string(readBack), string(origContent))
	}

	// Staging file must be cleaned up
	mu.Lock()
	stPath := createdStaging
	mu.Unlock()
	if stPath != "" {
		cleanPath := filepath.Join(tmpDir, filepath.FromSlash(strings.TrimPrefix(stPath, "./")))
		if _, statErr := os.Stat(cleanPath); !os.IsNotExist(statErr) {
			t.Errorf("Staging file %s was not cleaned up after cancellation: %v", cleanPath, statErr)
		}
	}
}

func TestUploadLostPublishAcknowledgementIsUnknown(t *testing.T) {
	tmpDir := t.TempDir()
	origContent := []byte("original pre-existing file")
	destPath := filepath.Join(tmpDir, "dest_unknown.txt")
	if err := os.WriteFile(destPath, origContent, 0644); err != nil {
		t.Fatalf("Failed to write destination: %v", err)
	}

	exec := newBashExecutor(t, tmpDir, nil)
	var mu sync.Mutex
	publishDispatched := 0

	tamperedExec := &interceptingExecutor{
		underlying: exec,
		onExec: func(ctx context.Context, cmd string) (*rtc.ExecResult, error) {
			if strings.HasPrefix(strings.TrimSpace(cmd), "mv -f") {
				mu.Lock()
				publishDispatched++
				mu.Unlock()
				// Simulate lost acknowledgement / network error after dispatch
				return nil, errors.New("network failure: connection reset by peer")
			}
			return nil, nil
		},
	}

	err := uploadFile(context.Background(), tamperedExec, []byte("data to publish"), "dest_unknown.txt")
	if err == nil {
		t.Fatal("Expected error on lost publish acknowledgement, got nil")
	}

	expectedSubstr := "upload publish outcome unknown: command was dispatched but acknowledgement was lost; destination may or may not be updated"
	if !strings.Contains(err.Error(), expectedSubstr) {
		t.Errorf("Expected error to contain %q, got %q", expectedSubstr, err.Error())
	}

	mu.Lock()
	dispatches := publishDispatched
	mu.Unlock()
	if dispatches != 1 {
		t.Errorf("Expected publish command to be dispatched exactly once with no automatic retry, got %d", dispatches)
	}
}

func TestEmptyAndWhitespaceUpload(t *testing.T) {
	tmpDir := t.TempDir()
	exec := newBashExecutor(t, tmpDir, nil)

	// 1. Empty content
	err := uploadFile(context.Background(), exec, []byte{}, "empty.txt")
	if err != nil {
		t.Fatalf("uploadFile failed for empty content: %v", err)
	}
	emptyBytes, err := os.ReadFile(filepath.Join(tmpDir, "empty.txt"))
	if err != nil {
		t.Fatalf("Failed to read back empty.txt: %v", err)
	}
	if len(emptyBytes) != 0 {
		t.Errorf("Expected 0 bytes, got %d bytes: %q", len(emptyBytes), string(emptyBytes))
	}

	// 2. Whitespace content
	wsData := []byte("   \n\t\r\n \v\f \n   \t\t\n")
	err = uploadFile(context.Background(), exec, wsData, "whitespace.txt")
	if err != nil {
		t.Fatalf("uploadFile failed for whitespace content: %v", err)
	}
	wsBytes, err := os.ReadFile(filepath.Join(tmpDir, "whitespace.txt"))
	if err != nil {
		t.Fatalf("Failed to read back whitespace.txt: %v", err)
	}
	if !bytes.Equal(wsBytes, wsData) {
		t.Errorf("Whitespace content mismatch! Expected %q, got %q", string(wsData), string(wsBytes))
	}
}

func TestQuotedPathUpload(t *testing.T) {
	tmpDir := t.TempDir()
	exec := newBashExecutor(t, tmpDir, nil)

	testCases := []struct {
		name     string
		dir      string
		filename string
		content  []byte
	}{
		{
			name:     "spaces in path and name",
			dir:      "folder with spaces",
			filename: "file with spaces.txt",
			content:  []byte("spaces content payload"),
		},
		{
			name:     "single quotes in path",
			dir:      "bob's folder",
			filename: "alice's notes.txt",
			content:  []byte("quotes content payload"),
		},
		{
			name:     "dollar signs and parenthesis",
			dir:      "special_$var_(dir)",
			filename: "my_$file-(1).txt",
			content:  []byte("dollar content payload"),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			mkdirCmd := fmt.Sprintf("mkdir -p -- '%s'", strings.ReplaceAll(tc.dir, "'", `'\''`))
			if res, err := exec.Exec(context.Background(), mkdirCmd, 1000); err != nil || res.ExitCode != 0 {
				t.Fatalf("mkdir failed: %v (exit %d)", err, res.ExitCode)
			}
			relPath := filepath.ToSlash(filepath.Join(tc.dir, tc.filename))
			err := uploadFile(context.Background(), exec, tc.content, relPath)
			if err != nil {
				t.Fatalf("uploadFile failed for path %q: %v", relPath, err)
			}
			readCmd := fmt.Sprintf("cat -- '%s'", strings.ReplaceAll(relPath, "'", `'\''`))
			res, err := exec.ExecRaw(context.Background(), readCmd, 10000)
			if err != nil || res.ExitCode != 0 {
				t.Fatalf("cat failed for %s: %v (exit %d)", relPath, err, res.ExitCode)
			}
			if !bytes.Equal([]byte(res.Stdout), tc.content) {
				t.Errorf("Content mismatch! Got %q, want %q", res.Stdout, string(tc.content))
			}
		})
	}

}

func TestUploadValidationFailures(t *testing.T) {
	tmpDir := t.TempDir()
	exec := newBashExecutor(t, tmpDir, nil)

	// 1. Content > 8 MiB
	tooLarge := make([]byte, 8*1024*1024+1)
	err := uploadFile(context.Background(), exec, tooLarge, "toolarge.txt")
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("Expected 8 MiB size limit error, got: %v", err)
	}

	// 2. Empty path
	err = uploadFile(context.Background(), exec, []byte("data"), "")
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("Expected empty path error, got: %v", err)
	}

	// 3. Trailing slash (directory target)
	err = uploadFile(context.Background(), exec, []byte("data"), "folder/")
	if err == nil || !strings.Contains(err.Error(), "directory") {
		t.Errorf("Expected trailing slash directory error, got: %v", err)
	}

	// 4. Target is an existing directory
	existingDir := filepath.Join(tmpDir, "existing_directory")
	if err := os.MkdirAll(existingDir, 0755); err != nil {
		t.Fatalf("Failed to create dir: %v", err)
	}
	err = uploadFile(context.Background(), exec, []byte("data"), "existing_directory")
	if err == nil || !strings.Contains(err.Error(), "directory") {
		t.Errorf("Expected existing directory error, got: %v", err)
	}
}

func TestDownloadThirtyKiBText(t *testing.T) {
	tmpDir := t.TempDir()
	exec := newBashExecutor(t, tmpDir, nil)

	// Create 30 KiB of text
	var sb strings.Builder
	for sb.Len() < 30*1024 {
		sb.WriteString("Line of text data for thirty kib download test. 1234567890\n")
	}
	content := sb.String()[:30*1024]
	filePath := filepath.Join(tmpDir, "thirty_kib.txt")
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		t.Fatalf("Failed to write fixture: %v", err)
	}

	maxChars := 15000
	result, err := downloadFile(context.Background(), exec, "thirty_kib.txt", maxChars)
	if err != nil {
		t.Fatalf("downloadFile failed: %v", err)
	}

	expectedIndicator := fmt.Sprintf("\n\n... [truncated to %d characters; file size %d bytes]", maxChars, len(content))
	if !strings.HasSuffix(result, expectedIndicator) {
		t.Fatalf("Expected result to have truncation indicator %q, got suffix %q", expectedIndicator, result[len(result)-len(expectedIndicator):])
	}

	trimmedText := strings.TrimSuffix(result, expectedIndicator)
	runes := []rune(trimmedText)
	if len(runes) != maxChars {
		t.Errorf("Expected %d runes before indicator, got %d", maxChars, len(runes))
	}

	expectedPrefix := string([]rune(content)[:maxChars])
	if trimmedText != expectedPrefix {
		t.Errorf("Truncated text does not match expected prefix")
	}
}

func TestDownloadInvalidBase64IsError(t *testing.T) {
	tmpDir := t.TempDir()
	exec := newBashExecutor(t, tmpDir, nil)

	// Subtest 1: Corrupted base64 payload
	corruptedExec := &interceptingExecutor{
		underlying: exec,
		onExec: func(ctx context.Context, cmd string) (*rtc.ExecResult, error) {
			if strings.Contains(cmd, "base64 -w 0") {
				return &rtc.ExecResult{
					ExitCode: 0,
					Stdout:   "25\nThisIsNotValidBase64!@#$%\n",
				}, nil
			}
			return nil, nil
		},
	}

	res, err := downloadFile(context.Background(), corruptedExec, "file.txt", 15000)
	if err == nil {
		t.Fatalf("Expected error for corrupted base64, got success: %q", res)
	}

	// Subtest 2: Incomplete / mid-stream truncated base64
	truncatedExec := &interceptingExecutor{
		underlying: exec,
		onExec: func(ctx context.Context, cmd string) (*rtc.ExecResult, error) {
			if strings.Contains(cmd, "base64 -w 0") {
				return &rtc.ExecResult{
					ExitCode: 0,
					Stdout:   "4\nYWJjZA",
				}, nil
			}
			return nil, nil
		},
	}

	res, err = downloadFile(context.Background(), truncatedExec, "file.txt", 15000)
	if err == nil {
		t.Fatalf("Expected error for incomplete base64 stream, got success: %q", res)
	}

	// Subtest 3: Response without the size line
	noSizeExec := &interceptingExecutor{
		underlying: exec,
		onExec: func(ctx context.Context, cmd string) (*rtc.ExecResult, error) {
			if strings.Contains(cmd, "base64 -w 0") {
				return &rtc.ExecResult{ExitCode: 0, Stdout: "YWJjZA=="}, nil
			}
			return nil, nil
		},
	}
	res, err = downloadFile(context.Background(), noSizeExec, "file.txt", 15000)
	if err == nil {
		t.Fatalf("Expected error for a response without a size line, got success: %q", res)
	}
}

func TestDownloadBinaryRoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	exec := newBashExecutor(t, tmpDir, nil)

	// Binary sequence with invalid UTF-8 bytes
	binaryContent := make([]byte, 2048)
	for i := range binaryContent {
		binaryContent[i] = byte(i % 256)
	}
	binaryContent[0] = 0xFF
	binaryContent[1] = 0xFE
	binaryContent[2] = 0x80
	binaryContent[3] = 0xC0

	filePath := filepath.Join(tmpDir, "binary.bin")
	if err := os.WriteFile(filePath, binaryContent, 0644); err != nil {
		t.Fatalf("Failed to write fixture: %v", err)
	}

	result, err := downloadFile(context.Background(), exec, "binary.bin", 15000)
	if err != nil {
		t.Fatalf("downloadFile failed: %v", err)
	}

	var jsonRes struct {
		RemotePath    string `json:"remote_path"`
		SizeBytes     int    `json:"size_bytes"`
		ContentBase64 string `json:"content_base64"`
	}
	if err := json.Unmarshal([]byte(result), &jsonRes); err != nil {
		t.Fatalf("Failed to unmarshal binary result JSON: %v, output: %q", err, result)
	}

	if jsonRes.RemotePath != "binary.bin" {
		t.Errorf("Expected remote_path 'binary.bin', got %q", jsonRes.RemotePath)
	}
	if jsonRes.SizeBytes != len(binaryContent) {
		t.Errorf("Expected size_bytes %d, got %d", len(binaryContent), jsonRes.SizeBytes)
	}

	decoded, err := base64.StdEncoding.DecodeString(jsonRes.ContentBase64)
	if err != nil {
		t.Fatalf("Failed to decode content_base64: %v", err)
	}

	if !bytes.Equal(decoded, binaryContent) {
		t.Fatalf("Decoded binary round-trip content mismatch!")
	}
}

func TestDownloadUnicodeRuneLimit(t *testing.T) {
	tmpDir := t.TempDir()
	exec := newBashExecutor(t, tmpDir, nil)

	// Multi-byte runes (Japanese 3-byte characters and 4-byte emojis)
	content := "こんにちは世界🌍🚀🎉 testing unicode rune truncation"
	filePath := filepath.Join(tmpDir, "unicode.txt")
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		t.Fatalf("Failed to write fixture: %v", err)
	}

	maxChars := 10
	result, err := downloadFile(context.Background(), exec, "unicode.txt", maxChars)
	if err != nil {
		t.Fatalf("downloadFile failed: %v", err)
	}

	indicator := fmt.Sprintf("\n\n... [truncated to %d characters; file size %d bytes]", maxChars, len(content))
	if !strings.HasSuffix(result, indicator) {
		t.Fatalf("Expected truncation indicator suffix %q, got: %q", indicator, result)
	}

	trimmed := strings.TrimSuffix(result, indicator)
	runes := []rune(trimmed)
	if len(runes) != maxChars {
		t.Fatalf("Expected %d runes, got %d", maxChars, len(runes))
	}

	expectedPrefix := string([]rune(content)[:maxChars])
	if trimmed != expectedPrefix {
		t.Errorf("Expected prefix %q, got %q", expectedPrefix, trimmed)
	}

	if !utf8.ValidString(trimmed) {
		t.Errorf("Trimmed text is not valid UTF-8: %x", trimmed)
	}
}

func TestDownloadEmptyFile(t *testing.T) {
	tmpDir := t.TempDir()
	exec := newBashExecutor(t, tmpDir, nil)

	filePath := filepath.Join(tmpDir, "empty.txt")
	if err := os.WriteFile(filePath, []byte{}, 0644); err != nil {
		t.Fatalf("Failed to write empty fixture: %v", err)
	}

	result, err := downloadFile(context.Background(), exec, "empty.txt", 15000)
	if err != nil {
		t.Fatalf("downloadFile failed for empty file: %v", err)
	}

	if result != "" {
		t.Errorf("Expected empty string result for empty file, got %q", result)
	}
}

func TestDownloadOversizeIsError(t *testing.T) {
	tmpDir := t.TempDir()
	exec := newBashExecutor(t, tmpDir, nil)

	// Case 1: The remote side sends more than the bytes that were requested
	const maxChars = 15000
	oversizeBytes := make([]byte, downloadFetchLimit(maxChars)+1)
	oversizeB64 := base64.StdEncoding.EncodeToString(oversizeBytes)

	oversizeExec := &interceptingExecutor{
		underlying: exec,
		onExec: func(ctx context.Context, cmd string) (*rtc.ExecResult, error) {
			if strings.Contains(cmd, "base64 -w 0") {
				return &rtc.ExecResult{
					ExitCode: 0,
					Stdout:   fmt.Sprintf("%d\n%s", len(oversizeBytes), oversizeB64),
				}, nil
			}
			return nil, nil
		},
	}

	res, err := downloadFile(context.Background(), oversizeExec, "large.bin", maxChars)
	if err == nil {
		t.Fatalf("Expected error for a response larger than requested, got success (len %d)", len(res))
	}
	if !strings.Contains(err.Error(), "more than") {
		t.Errorf("Expected error to say more data arrived than requested, got: %v", err)
	}

	// Case 2: Output exceeds maxEncodedBytes so ExecRaw returns ErrOutputLimit
	limitExec := &interceptingExecutor{
		underlying: exec,
		onExec: func(ctx context.Context, cmd string) (*rtc.ExecResult, error) {
			if strings.Contains(cmd, "base64 -w 0") {
				return nil, rtc.ErrOutputLimit
			}
			return nil, nil
		},
	}

	_, err = downloadFile(context.Background(), limitExec, "too_large.bin", 15000)
	if err == nil {
		t.Fatal("Expected error when ExecRaw returns ErrOutputLimit, got nil")
	}
}

func TestDownloadValidationFailures(t *testing.T) {
	tmpDir := t.TempDir()
	exec := newBashExecutor(t, tmpDir, nil)

	// 1. Empty path
	_, err := downloadFile(context.Background(), exec, "", 15000)
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("Expected empty path error, got: %v", err)
	}

	// 2. Trailing slash
	_, err = downloadFile(context.Background(), exec, "folder/", 15000)
	if err == nil || !strings.Contains(err.Error(), "directory") {
		t.Errorf("Expected directory error on trailing slash, got: %v", err)
	}

	// 3. Non-existent file
	_, err = downloadFile(context.Background(), exec, "nonexistent.txt", 15000)
	if err == nil {
		t.Errorf("Expected error for non-existent file, got nil")
	}
}

func TestUploadRemainsInOriginalDirectory(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	executor := newBashExecutor(t, first, nil)
	executor.onCommand = func(command string, res rtc.ExecResult) {
		if strings.Contains(command, "mktemp") {
			executor.dir = second
		}
	}
	if err := uploadFile(context.Background(), executor, []byte("stable directory"), "result.txt"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(first, "result.txt"))
	if err != nil || string(data) != "stable directory" {
		t.Fatalf("wrong destination: %v", err)
	}
	if _, err := os.Stat(filepath.Join(second, "result.txt")); !os.IsNotExist(err) {
		t.Fatal("upload redirected by concurrent cd")
	}
}

func TestUploadRejectsDestinationBecomingDirectory(t *testing.T) {
	dir := t.TempDir()
	executor := newBashExecutor(t, dir, nil)
	executor.onCommand = func(command string, res rtc.ExecResult) {
		if strings.HasPrefix(command, "sha256sum") {
			if err := os.Mkdir(filepath.Join(dir, "result.txt"), 0700); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := uploadFile(context.Background(), executor, []byte("data"), "result.txt"); err == nil {
		t.Fatal("reported success after moving staging inside a directory")
	}
	entries, err := os.ReadDir(filepath.Join(dir, "result.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatal("published inside directory")
	}
}

type recordingUploadExecutor struct {
	underlying UploadExecutor
	appends    []struct {
		StagingPath string
		Content     []byte
	}
	execCommands []string
}

func (r *recordingUploadExecutor) Exec(ctx context.Context, command string, maxChars int) (rtc.ExecResult, error) {
	r.execCommands = append(r.execCommands, command)
	return r.underlying.Exec(ctx, command, maxChars)
}

func (r *recordingUploadExecutor) ExecRaw(ctx context.Context, command string, maxBytes int) (rtc.ExecResult, error) {
	r.execCommands = append(r.execCommands, command)
	return r.underlying.ExecRaw(ctx, command, maxBytes)
}

func (r *recordingUploadExecutor) AppendFile(ctx context.Context, stagingPath string, content []byte) error {
	r.appends = append(r.appends, struct {
		StagingPath string
		Content     []byte
	}{
		StagingPath: stagingPath,
		Content:     append([]byte(nil), content...),
	})
	return r.underlying.AppendFile(ctx, stagingPath, content)
}

func TestUploadUsesSingleStreamingAppend(t *testing.T) {
	dir := t.TempDir()
	bashExec := newBashExecutor(t, dir, nil)

	testCases := []struct {
		name    string
		content []byte
		dest    string
	}{
		{
			name:    "NonEmptyContent10KiB",
			content: bytes.Repeat([]byte("0123456789"), 1024),
			dest:    "nonempty.bin",
		},
		{
			name:    "EmptyContent",
			content: []byte{},
			dest:    "empty.bin",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recordingUploadExecutor{underlying: bashExec}
			err := uploadFile(context.Background(), rec, tc.content, tc.dest)
			if err != nil {
				t.Fatalf("uploadFile failed: %v", err)
			}

			// A recording fake must observe exactly one append for each upload, including empty content
			if len(rec.appends) != 1 {
				t.Fatalf("expected exactly 1 AppendFile call, got %d", len(rec.appends))
			}
			if !bytes.Equal(rec.appends[0].Content, tc.content) {
				t.Errorf("appended content mismatch: expected %d bytes, got %d bytes", len(tc.content), len(rec.appends[0].Content))
			}

			// Check that no per-chunk Exec commands were dispatched
			for _, cmd := range rec.execCommands {
				if strings.Contains(cmd, "base64 -d >>") {
					t.Errorf("found legacy chunked exec command: %s", cmd)
				}
			}

			// Verify file on disk
			data, err := os.ReadFile(filepath.Join(dir, tc.dest))
			if err != nil {
				t.Fatalf("ReadFile dest failed: %v", err)
			}
			if !bytes.Equal(data, tc.content) {
				t.Errorf("file on disk mismatch")
			}
		})
	}
}

func TestUploadStreamingFailurePreservesDestination(t *testing.T) {
	dir := t.TempDir()
	bashExec := newBashExecutor(t, dir, nil)

	destFile := "preserved.txt"
	destFullPath := filepath.Join(dir, destFile)
	originalContent := []byte("original preserved content that must not be altered")
	if err := os.WriteFile(destFullPath, originalContent, 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	var checksumAttempted, publishAttempted bool
	intercept := &interceptingExecutor{
		underlying: bashExec,
		onAppend: func(ctx context.Context, stagingPath string, content []byte) error {
			return errors.New("simulated streaming failure in AppendFile")
		},
		onExec: func(ctx context.Context, cmd string) (*rtc.ExecResult, error) {
			if strings.HasPrefix(cmd, "sha256sum") {
				checksumAttempted = true
			}
			if strings.HasPrefix(cmd, "mv -fT") {
				publishAttempted = true
			}
			return nil, nil
		},
	}

	err := uploadFile(context.Background(), intercept, []byte("new replacement content"), destFile)
	if err == nil {
		t.Fatalf("expected uploadFile to fail on AppendFile error, got nil")
	}

	if checksumAttempted {
		t.Errorf("checksum was attempted despite append failure")
	}
	if publishAttempted {
		t.Errorf("publication was attempted despite append failure")
	}

	// Destination must remain unchanged
	data, err := os.ReadFile(destFullPath)
	if err != nil {
		t.Fatalf("ReadFile destFullPath failed: %v", err)
	}
	if !bytes.Equal(data, originalContent) {
		t.Errorf("destination content was corrupted! expected %q, got %q", string(originalContent), string(data))
	}
}

func TestUploadStreamingCleanupFailure(t *testing.T) {
	dir := t.TempDir()
	bashExec := newBashExecutor(t, dir, nil)

	intercept := &interceptingExecutor{
		underlying: bashExec,
		onAppend: func(ctx context.Context, stagingPath string, content []byte) error {
			return errors.New("simulated append stream error")
		},
		onExec: func(ctx context.Context, cmd string) (*rtc.ExecResult, error) {
			if strings.HasPrefix(cmd, "rm -f --") {
				res := rtc.ExecResult{ExitCode: 1, Stdout: "rm: cannot remove staging: permission denied"}
				return &res, nil
			}
			return nil, nil
		},
	}

	err := uploadFile(context.Background(), intercept, []byte("test data"), "cleanup_fail.txt")
	if err == nil {
		t.Fatalf("expected error on append and cleanup failure, got nil")
	}

	if !strings.Contains(err.Error(), "simulated append stream error") {
		t.Errorf("expected original error in output, got: %v", err)
	}
	if !strings.Contains(err.Error(), "residue may remain") {
		t.Errorf("expected staging residue warning in error, got: %v", err)
	}
}
