package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"rpi-ssh-go/pkg/rtc"
)

// The staging file comes from mktemp and is therefore 0600. Its mode has to
// be set before it replaces the destination.
func TestUploadSetsModeBeforePublish(t *testing.T) {
	dir := t.TempDir()
	rec := &recordingUploadExecutor{underlying: newBashExecutor(t, dir, nil)}

	if err := uploadFile(context.Background(), rec, []byte("#!/bin/sh\necho hi\n"), "deploy.sh"); err != nil {
		t.Fatalf("uploadFile failed: %v", err)
	}

	hashAt, modeAt, publishAt := -1, -1, -1
	for i, cmd := range rec.execCommands {
		switch {
		case strings.HasPrefix(cmd, "sha256sum"):
			hashAt = i
		case strings.Contains(cmd, "chmod"):
			modeAt = i
			if !strings.Contains(cmd, "--reference=") || !strings.Contains(cmd, "umask") {
				t.Errorf("mode step must copy the replaced file's mode or apply the umask: %s", cmd)
			}
			if !strings.Contains(cmd, ".staging.") {
				t.Errorf("mode step does not target the staging file: %s", cmd)
			}
		case strings.HasPrefix(cmd, "mv -fT"):
			publishAt = i
		}
	}
	if modeAt < 0 {
		t.Fatalf("no step sets the staging file's mode; uploads would always be 0600. Commands: %q", rec.execCommands)
	}
	if !(hashAt < modeAt && modeAt < publishAt) {
		t.Fatalf("mode must be set after verification and before publish (hash=%d mode=%d publish=%d)", hashAt, modeAt, publishAt)
	}
}

// Real permission bits cannot be observed through Git Bash on Windows.
func TestUploadKeepsModeOfReplacedFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not observable on Windows")
	}
	dir := t.TempDir()
	executor := newBashExecutor(t, dir, nil)

	existing := filepath.Join(dir, "deploy.sh")
	if err := os.WriteFile(existing, []byte("old"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(existing, 0755); err != nil {
		t.Fatal(err)
	}
	if err := uploadFile(context.Background(), executor, []byte("new"), "deploy.sh"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(existing)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0755 {
		t.Errorf("replaced file mode = %o, want 755", info.Mode().Perm())
	}

	if err := uploadFile(context.Background(), executor, []byte("page"), "index.html"); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(filepath.Join(dir, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0044 == 0 {
		t.Errorf("new file mode = %o, want it readable by group and others under a normal umask", info.Mode().Perm())
	}
}

func TestTransfersExpandTildePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// The shell starts somewhere else, so only ~ can lead to the home directory.
	executor := newBashExecutor(t, t.TempDir(), nil)

	if err := os.Mkdir(filepath.Join(home, "app"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := uploadFile(context.Background(), executor, []byte("tilde upload"), "~/app/config.json"); err != nil {
		t.Fatalf("upload to ~/ path failed: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(home, "app", "config.json"))
	if err != nil || string(data) != "tilde upload" {
		t.Fatalf("file was not written below the home directory: %q, %v", data, err)
	}

	got, err := downloadFile(context.Background(), executor, "~/app/config.json", 1000)
	if err != nil {
		t.Fatalf("download from ~/ path failed: %v", err)
	}
	if got != "tilde upload" {
		t.Fatalf("downloaded %q, want %q", got, "tilde upload")
	}
}

// sessionLostExecutor behaves like a session that is closed once the
// streaming transfer fails: every later command is rejected.
type sessionLostExecutor struct {
	UploadExecutor
	mu   sync.Mutex
	lost bool
}

func (s *sessionLostExecutor) isLost() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lost
}

func (s *sessionLostExecutor) Exec(ctx context.Context, command string, maxChars int) (rtc.ExecResult, error) {
	if s.isLost() {
		return rtc.ExecResult{}, errors.New("session is closed")
	}
	return s.UploadExecutor.Exec(ctx, command, maxChars)
}

func (s *sessionLostExecutor) AppendFile(ctx context.Context, stagingPath string, content []byte) error {
	s.mu.Lock()
	s.lost = true
	s.mu.Unlock()
	return errors.New("upload cancelled or timed out: context deadline exceeded")
}

func stagingFiles(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".staging.*"))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

// A transfer that fails usually takes its session down with it. Cleanup then
// has to run on a new session, or the partial file stays next to the target.
func TestUploadCleansStagingOnFreshSessionAfterSessionLoss(t *testing.T) {
	dir := t.TempDir()
	bashExec := newBashExecutor(t, dir, nil)
	lost := &sessionLostExecutor{UploadExecutor: bashExec}

	reopened := 0
	reopen := func(ctx context.Context) (CommandExecutor, error) {
		reopened++
		return bashExec, nil
	}

	err := uploadFileWithReopen(context.Background(), lost, reopen, []byte("partial data"), "target.bin")
	if err == nil {
		t.Fatal("expected the upload to fail")
	}
	if left := stagingFiles(t, dir); len(left) != 0 {
		t.Fatalf("staging files left behind: %v (error: %v)", left, err)
	}
	if reopened != 1 {
		t.Errorf("expected cleanup to open exactly one fresh session, got %d", reopened)
	}
	if strings.Contains(err.Error(), "residue") {
		t.Errorf("error claims residue although cleanup succeeded: %v", err)
	}
}

func TestUploadReportsResidueWhenNoFreshSessionIsAvailable(t *testing.T) {
	dir := t.TempDir()
	lost := &sessionLostExecutor{UploadExecutor: newBashExecutor(t, dir, nil)}

	reopen := func(ctx context.Context) (CommandExecutor, error) {
		return nil, errors.New("device is currently offline")
	}

	err := uploadFileWithReopen(context.Background(), lost, reopen, []byte("partial data"), "target.bin")
	if err == nil || !strings.Contains(err.Error(), "residue may remain") {
		t.Fatalf("expected a residue warning, got: %v", err)
	}
}

// Only what max_chars can use is transferred, however large the file is.
func TestDownloadTransfersOnlyWhatIsNeeded(t *testing.T) {
	dir := t.TempDir()
	rec := &recordingUploadExecutor{underlying: newBashExecutor(t, dir, nil)}

	big := bytes.Repeat([]byte("0123456789abcdef"), 64*1024) // 1 MiB
	if err := os.WriteFile(filepath.Join(dir, "big.log"), big, 0644); err != nil {
		t.Fatal(err)
	}

	var transferred int
	inner := rec.underlying
	rec.underlying = &interceptingExecutor{
		underlying: inner,
		afterExec: func(ctx context.Context, cmd string, res rtc.ExecResult, err error) {
			transferred += len(res.Stdout)
		},
	}

	const maxChars = 100
	result, err := downloadFile(context.Background(), rec, "big.log", maxChars)
	if err != nil {
		t.Fatalf("downloadFile failed: %v", err)
	}
	if !strings.HasPrefix(result, string(big[:maxChars])+"\n\n... [truncated to 100 characters") {
		t.Fatalf("unexpected result: %q", result)
	}
	if !strings.Contains(result, fmt.Sprintf("%d bytes", len(big))) {
		t.Errorf("truncation note should state the full file size, got %q", result[maxChars:])
	}
	if transferred > 4096 {
		t.Errorf("%d bytes were transferred to return %d characters", transferred, maxChars)
	}
}

// A file larger than any transfer cap is still readable up to max_chars
// instead of failing and taking the session down.
func TestDownloadOfHugeFileDoesNotFail(t *testing.T) {
	dir := t.TempDir()
	executor := newBashExecutor(t, dir, nil)

	f, err := os.Create(filepath.Join(dir, "huge.log"))
	if err != nil {
		t.Fatal(err)
	}
	line := []byte("a line of log output that repeats many times\n")
	if _, err := f.Write(bytes.Repeat(line, (9*1024*1024)/len(line))); err != nil {
		t.Fatal(err)
	}
	f.Close()

	result, err := downloadFile(context.Background(), executor, "huge.log", 15000)
	if err != nil {
		t.Fatalf("downloadFile failed for a 9 MiB file: %v", err)
	}
	if !strings.Contains(result, "... [truncated to 15000 characters") {
		t.Fatalf("expected a truncated prefix, got %d characters", len(result))
	}
}

func TestDownloadLimitsBinaryPayload(t *testing.T) {
	dir := t.TempDir()
	executor := newBashExecutor(t, dir, nil)

	binary := make([]byte, 100*1024)
	for i := range binary {
		binary[i] = byte(i*7 + 3)
	}
	binary[0], binary[1] = 0xFF, 0xFE
	if err := os.WriteFile(filepath.Join(dir, "image.bin"), binary, 0644); err != nil {
		t.Fatal(err)
	}

	const maxChars = 1000
	result, err := downloadFile(context.Background(), executor, "image.bin", maxChars)
	if err != nil {
		t.Fatalf("downloadFile failed: %v", err)
	}

	var res struct {
		RemotePath    string `json:"remote_path"`
		SizeBytes     int    `json:"size_bytes"`
		ReturnedBytes int    `json:"returned_bytes"`
		Truncated     bool   `json:"truncated"`
		ContentBase64 string `json:"content_base64"`
	}
	if err := json.Unmarshal([]byte(result), &res); err != nil {
		t.Fatalf("binary result is not JSON: %v (%d characters)", err, len(result))
	}
	if len(res.ContentBase64) > maxChars {
		t.Fatalf("content_base64 has %d characters, limit is %d", len(res.ContentBase64), maxChars)
	}
	if res.SizeBytes != len(binary) {
		t.Errorf("size_bytes = %d, want the full file size %d", res.SizeBytes, len(binary))
	}
	if !res.Truncated {
		t.Errorf("truncated should be true")
	}
	decoded, err := base64.StdEncoding.DecodeString(res.ContentBase64)
	if err != nil {
		t.Fatalf("content_base64 is not valid base64: %v", err)
	}
	if len(decoded) == 0 || !bytes.Equal(decoded, binary[:len(decoded)]) {
		t.Errorf("content_base64 is not a prefix of the file")
	}
	if res.ReturnedBytes != len(decoded) {
		t.Errorf("returned_bytes = %d, want %d", res.ReturnedBytes, len(decoded))
	}
}

// A multi-byte character cut in half by the transfer limit must not turn a
// text file into a binary result.
func TestDownloadPrefixEndingInsideCharacterStaysText(t *testing.T) {
	dir := t.TempDir()
	executor := newBashExecutor(t, dir, nil)

	content := strings.Repeat("€", 5000) // 3 bytes each
	if err := os.WriteFile(filepath.Join(dir, "euro.txt"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	for _, maxChars := range []int{1, 2, 3, 7, 100} {
		result, err := downloadFile(context.Background(), executor, "euro.txt", maxChars)
		if err != nil {
			t.Fatalf("max_chars=%d: %v", maxChars, err)
		}
		want := strings.Repeat("€", maxChars)
		if !strings.HasPrefix(result, want+"\n\n... [truncated to") {
			t.Fatalf("max_chars=%d: unexpected result %q", maxChars, result[:min(len(result), 80)])
		}
	}
}

func TestDownloadDirectoryIsClearError(t *testing.T) {
	dir := t.TempDir()
	executor := newBashExecutor(t, dir, nil)
	if err := os.Mkdir(filepath.Join(dir, "folder"), 0755); err != nil {
		t.Fatal(err)
	}
	_, err := downloadFile(context.Background(), executor, "folder", 1000)
	if err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("expected a directory error, got: %v", err)
	}
}

// Uploading to a symbolic link replaces the file it points to and leaves the
// link in place. Git Bash on Windows copies instead of linking, so the test
// needs a real POSIX file system. (Checked against a Raspberry Pi as well.)
func TestUploadWritesThroughSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links are not observable through Git Bash on Windows")
	}
	dir := t.TempDir()
	executor := newBashExecutor(t, dir, nil)

	if err := os.Mkdir(filepath.Join(dir, "real"), 0755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "real", "target.txt")
	if err := os.WriteFile(target, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(filepath.Join("real", "target.txt"), link); err != nil {
		t.Fatal(err)
	}

	if err := uploadFile(context.Background(), executor, []byte("new through link"), "link.txt"); err != nil {
		t.Fatalf("uploadFile failed: %v", err)
	}

	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link was replaced by a regular file (mode %v, err %v)", info.Mode(), err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "new through link" {
		t.Fatalf("the link's target was not updated: %q, %v", data, err)
	}
}
