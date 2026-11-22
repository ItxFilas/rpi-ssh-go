package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"rpi-ssh-go/pkg/rtc"
	"rpi-ssh-go/pkg/terminal"
)

const (
	MaxUploadBytes  = 8 * 1024 * 1024 // 8 MiB
	UploadChunkSize = 1024            // 1 KiB conservative chunk size
	UploadTimeout   = 120 * time.Second
	CleanupTimeout  = 5 * time.Second
	// ReconnectCleanupTimeout bounds opening a new session for cleanup after
	// the transfer took the old one down.
	ReconnectCleanupTimeout = 40 * time.Second
	MaxDownloadBytes        = 8 * 1024 * 1024 // 8 MiB
	DownloadTimeout         = 120 * time.Second

	// downloadIsDirectoryExit is the exit code the download command uses to
	// report that the path is a directory.
	downloadIsDirectoryExit = 21
)

type CommandExecutor interface {
	Exec(ctx context.Context, command string, maxChars int) (rtc.ExecResult, error)
	ExecRaw(ctx context.Context, command string, maxBytes int) (rtc.ExecResult, error)
}

type UploadExecutor interface {
	CommandExecutor
	AppendFile(ctx context.Context, stagingPath string, content []byte) error
}

// ExecutorOpener returns an executor on a usable session. Uploads call it to
// clean up after the session they were using has been closed.
type ExecutorOpener func(ctx context.Context) (CommandExecutor, error)

// shellPathWord returns path as one shell word. A leading "~" or "~/" becomes
// $HOME, as it would when typed into a shell; everything else is quoted
// literally.
func shellPathWord(path string) string {
	if path == "~" {
		return `"$HOME"`
	}
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		return `"$HOME"/` + terminal.ShellQuote(rest)
	}
	return terminal.ShellQuote(path)
}

func uploadFile(ctx context.Context, executor UploadExecutor, content []byte, remotePath string) error {
	return uploadFileWithReopen(ctx, executor, nil, content, remotePath)
}

// uploadFileWithReopen uploads content to remotePath through a staging file
// that is verified and then renamed over the destination. reopen may be nil;
// when set it provides a fresh session for removing the staging file after
// the transfer has closed the one it ran on.
func uploadFileWithReopen(ctx context.Context, executor UploadExecutor, reopen ExecutorOpener, content []byte, remotePath string) error {
	// 1. Enforce 8 MiB content cap
	if len(content) > MaxUploadBytes {
		return fmt.Errorf("upload content size (%d bytes) exceeds maximum limit of %d bytes (8 MiB)", len(content), MaxUploadBytes)
	}

	// 2. Validate remotePath: reject empty or trailing slash / directory target
	trimmedPath := strings.TrimSpace(remotePath)
	if trimmedPath == "" {
		return fmt.Errorf("remote path cannot be empty")
	}
	if strings.HasSuffix(trimmedPath, "/") {
		return fmt.Errorf("remote path '%s' cannot be a directory (trailing slash)", remotePath)
	}

	// 3. Enforce 120s overall deadline (capped by parent context)
	uploadCtx, cancel := context.WithTimeout(ctx, UploadTimeout)
	defer cancel()

	if uploadCtx.Err() != nil {
		return uploadCtx.Err()
	}

	// Capture an absolute destination before other requests can change shell cwd.
	// A symlink is followed so the file it points to is replaced, not the link.
	// Base64 preserves whitespace in the path across the terminal envelope.
	resolveCmd := fmt.Sprintf(`( p=%s; case "$p" in /*) ;; *) p="$(pwd -P)/$p" ;; esac; if [ -L "$p" ]; then p=$(readlink -f -- "$p") || exit 1; fi; printf '%%s' "$p" | base64 -w 0 )`, shellPathWord(trimmedPath))
	resolved, err := executor.ExecRaw(uploadCtx, resolveCmd, 65536)
	if err != nil {
		return fmt.Errorf("resolve upload destination: %w", err)
	}
	if resolved.ExitCode != 0 {
		return fmt.Errorf("resolve upload destination: exit code %d", resolved.ExitCode)
	}
	absolutePath, err := base64.StdEncoding.DecodeString(strings.TrimSpace(resolved.Stdout))
	if err != nil || len(absolutePath) == 0 || absolutePath[0] != '/' {
		return fmt.Errorf("invalid absolute upload destination")
	}
	trimmedPath = string(absolutePath)
	quotedDest := terminal.ShellQuote(trimmedPath)

	// 4. Verify target is not an existing directory
	checkPath := trimmedPath
	if strings.HasPrefix(checkPath, "-") {
		checkPath = "./" + checkPath
	}
	checkDirCmd := fmt.Sprintf("test -d %s", terminal.ShellQuote(checkPath))
	res, err := executor.Exec(uploadCtx, checkDirCmd, 1000)
	if err != nil {
		return fmt.Errorf("failed to check destination path: %w", err)
	}
	if res.ExitCode == 0 {
		return fmt.Errorf("destination '%s' is an existing directory", trimmedPath)
	}

	if uploadCtx.Err() != nil {
		return uploadCtx.Err()
	}

	// 5. Create unique staging file in target directory using mktemp
	mktempCmd := fmt.Sprintf("d=$(dirname -- %s 2>/dev/null || echo .); [ -z \"$d\" ] && d=.; mktemp -p \"$d\" -- .staging.XXXXXXXXXX", quotedDest)
	res, err = executor.Exec(uploadCtx, mktempCmd, 1000)
	if err != nil {
		return fmt.Errorf("failed to create staging file: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("failed to create staging file: exit code %d: %s", res.ExitCode, strings.TrimSpace(res.Stdout))
	}

	stagingPath := strings.TrimSpace(res.Stdout)
	if stagingPath == "" {
		return fmt.Errorf("failed to create staging file: empty path returned by mktemp")
	}

	quotedStaging := terminal.ShellQuote(stagingPath)
	removeCmd := fmt.Sprintf("rm -f -- %s", quotedStaging)

	// removeStaging reports ran=false when the command could not be run at
	// all, which is what a closed session looks like.
	removeStaging := func(target CommandExecutor) (ran bool, err error) {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), CleanupTimeout)
		defer cleanupCancel()
		cleanRes, cleanErr := target.Exec(cleanupCtx, removeCmd, 1000)
		if cleanErr != nil {
			return false, cleanErr
		}
		if cleanRes.ExitCode != 0 {
			return true, fmt.Errorf("exit code %d: %s", cleanRes.ExitCode, strings.TrimSpace(cleanRes.Stdout))
		}
		return true, nil
	}

	// Best-effort cleanup. A failed transfer usually closes its session, so
	// when the command cannot be run there, try once on a fresh session.
	cleanupStaging := func() error {
		ran, cleanErr := removeStaging(executor)
		if cleanErr == nil {
			return nil
		}
		if !ran && reopen != nil {
			openCtx, openCancel := context.WithTimeout(context.Background(), ReconnectCleanupTimeout)
			fresh, openErr := reopen(openCtx)
			openCancel()
			if openErr != nil {
				cleanErr = fmt.Errorf("%v; reconnecting to clean up failed: %v", cleanErr, openErr)
			} else if _, cleanErr = removeStaging(fresh); cleanErr == nil {
				return nil
			}
		}
		return fmt.Errorf("staging file '%s' residue may remain: %v", stagingPath, cleanErr)
	}

	failWithCleanup := func(origErr error) error {
		if cleanErr := cleanupStaging(); cleanErr != nil {
			return fmt.Errorf("%w (cleanup failed: %v)", origErr, cleanErr)
		}
		return origErr
	}

	// 6. Transfer content using single streaming append operation
	if uploadCtx.Err() != nil {
		return failWithCleanup(uploadCtx.Err())
	}
	if err := executor.AppendFile(uploadCtx, stagingPath, content); err != nil {
		return failWithCleanup(fmt.Errorf("failed to stream content to staging file: %w", err))
	}

	// 7. Calculate local SHA-256 and verify remote SHA-256
	localSum := sha256.Sum256(content)
	localHash := hex.EncodeToString(localSum[:])

	if uploadCtx.Err() != nil {
		return failWithCleanup(uploadCtx.Err())
	}

	hashCmd := fmt.Sprintf("sha256sum -- %s", quotedStaging)
	res, err = executor.Exec(uploadCtx, hashCmd, 1000)
	if err != nil {
		return failWithCleanup(fmt.Errorf("failed to calculate remote sha256: %w", err))
	}
	if res.ExitCode != 0 {
		return failWithCleanup(fmt.Errorf("failed to calculate remote sha256: exit code %d: %s", res.ExitCode, strings.TrimSpace(res.Stdout)))
	}

	fields := strings.Fields(res.Stdout)
	if len(fields) == 0 {
		return failWithCleanup(fmt.Errorf("failed to parse remote sha256: empty output from sha256sum"))
	}
	remoteHash := strings.ToLower(fields[0])
	if len(remoteHash) != 64 {
		return failWithCleanup(fmt.Errorf("invalid remote sha256 digest length %d: %s", len(remoteHash), remoteHash))
	}
	if remoteHash != localHash {
		return failWithCleanup(fmt.Errorf("sha256 checksum mismatch: expected %s, got %s", localHash, remoteHash))
	}

	// 8. mktemp creates the staging file 0600. Give it the mode of the file
	// it replaces, or what a newly created file would get under the umask.
	if uploadCtx.Err() != nil {
		return failWithCleanup(uploadCtx.Err())
	}
	modeCmd := fmt.Sprintf(`if [ -e %[2]s ]; then chmod --reference=%[2]s -- %[1]s; else chmod "$(printf '%%o' $((0666 & ~$(umask))))" -- %[1]s; fi`, quotedStaging, quotedDest)
	res, err = executor.Exec(uploadCtx, modeCmd, 1000)
	if err != nil {
		return failWithCleanup(fmt.Errorf("failed to set file mode: %w", err))
	}
	if res.ExitCode != 0 {
		return failWithCleanup(fmt.Errorf("failed to set file mode: exit code %d: %s", res.ExitCode, strings.TrimSpace(res.Stdout)))
	}

	// 9. Commit boundary: Dispatch atomic publish mv -f -- "$staging" "$dest"
	// If context cancelled or connection fails BEFORE dispatching mv, remove staging and return cancellation error.
	if uploadCtx.Err() != nil {
		return failWithCleanup(uploadCtx.Err())
	}

	publishCmd := fmt.Sprintf("mv -fT -- %s %s", quotedStaging, quotedDest)
	res, err = executor.Exec(uploadCtx, publishCmd, 1000)
	if err != nil {
		// Error or timeout occurs AFTER dispatching mv: return explicit error stating outcome is unknown.
		// Never automatically retry, and do NOT remove staging as destination may or may not be updated.
		return fmt.Errorf("upload publish outcome unknown: command was dispatched but acknowledgement was lost; destination may or may not be updated: %w", err)
	}
	if res.ExitCode != 0 {
		return failWithCleanup(fmt.Errorf("failed to publish file: exit code %d: %s", res.ExitCode, strings.TrimSpace(res.Stdout)))
	}

	return nil
}

// downloadFetchLimit is how many bytes of the file have to be transferred to
// return maxChars characters: a character takes at most four bytes, and a few
// more show whether the file continues.
func downloadFetchLimit(maxChars int) int {
	return min(maxChars*utf8.UTFMax+utf8.UTFMax, MaxDownloadBytes)
}

type binaryDownloadResult struct {
	RemotePath    string `json:"remote_path"`
	SizeBytes     int64  `json:"size_bytes"`
	ReturnedBytes int    `json:"returned_bytes"`
	Truncated     bool   `json:"truncated"`
	ContentBase64 string `json:"content_base64"`
}

// downloadFile returns up to maxChars characters of a remote file. Text comes
// back as text; anything else as JSON carrying a base64 prefix of at most
// maxChars characters. Only the bytes needed for that are transferred, so the
// size of the file does not matter.
func downloadFile(ctx context.Context, executor CommandExecutor, remotePath string, maxChars int) (string, error) {
	// 1. Validate remotePath non-empty and clean
	trimmedPath := strings.TrimSpace(remotePath)
	if trimmedPath == "" {
		return "", fmt.Errorf("remote path cannot be empty")
	}
	if strings.HasSuffix(trimmedPath, "/") {
		return "", fmt.Errorf("remote path '%s' cannot be a directory (trailing slash)", remotePath)
	}
	if maxChars <= 0 {
		maxChars = DefaultDownloadChars
	}

	// 2. Enforce 120s total deadline
	dlCtx, cancel := context.WithTimeout(ctx, DownloadTimeout)
	defer cancel()

	if dlCtx.Err() != nil {
		return "", dlCtx.Err()
	}

	// 3. One command prints the file size on the first line and the base64
	// of the first fetchLimit bytes on the second. It runs in a subshell so
	// it can bail out without touching the persistent shell.
	fetchLimit := downloadFetchLimit(maxChars)
	maxEncodedBytes := 4*((fetchLimit+2)/3) + 4096
	cmd := fmt.Sprintf(`( set -o pipefail; f=%s; if [ -d "$f" ]; then echo "is a directory"; exit %d; fi; stat -L -c %%s -- "$f" || exit 1; head -c %d -- "$f" | base64 -w 0 )`,
		shellPathWord(trimmedPath), downloadIsDirectoryExit, fetchLimit)

	res, err := executor.ExecRaw(dlCtx, cmd, maxEncodedBytes)
	if err != nil {
		return "", fmt.Errorf("failed to execute download command: %w", err)
	}
	if res.ExitCode == downloadIsDirectoryExit {
		return "", fmt.Errorf("remote path '%s' is a directory", remotePath)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("download command failed with exit code %d: %s", res.ExitCode, strings.TrimSpace(res.Stdout))
	}

	// 4. Split the size line from the payload.
	sizeLine, payload, _ := strings.Cut(strings.TrimLeft(res.Stdout, "\r\n"), "\n")
	fileSize, sizeErr := strconv.ParseInt(strings.TrimSpace(sizeLine), 10, 64)
	if sizeErr != nil || fileSize < 0 {
		return "", fmt.Errorf("unexpected download response: missing file size")
	}

	// 5. Clean and decode base64 bytes. `base64 -w 0` emits a single line, so
	// stripping line breaks is normally unnecessary.
	cleanB64 := strings.TrimSpace(payload)
	if strings.ContainsAny(cleanB64, "\r\n") {
		cleanB64 = strings.NewReplacer("\n", "", "\r", "").Replace(cleanB64)
	}
	decoded, decErr := base64.StdEncoding.DecodeString(cleanB64)
	if decErr != nil {
		return "", fmt.Errorf("failed to decode base64 content: %w", decErr)
	}
	if len(decoded) > fetchLimit {
		return "", fmt.Errorf("download returned %d bytes, more than the %d requested", len(decoded), fetchLimit)
	}

	// Files such as /proc entries report size 0 but have content.
	if int64(len(decoded)) > fileSize {
		fileSize = int64(len(decoded))
	}
	partial := fileSize > int64(len(decoded)) || len(decoded) == fetchLimit

	// 6. Valid UTF-8 is returned as text, limited to maxChars characters. A
	// partial read may end inside a character; that alone does not make the
	// file binary.
	text, isText := decoded, utf8.Valid(decoded)
	if !isText && partial {
		for cut := 1; cut < utf8.UTFMax && cut <= len(decoded); cut++ {
			if candidate := decoded[:len(decoded)-cut]; utf8.Valid(candidate) {
				text, isText = candidate, true
				break
			}
		}
	}
	if isText {
		// Find the byte offset of the maxChars-th character without
		// materialising the whole prefix as runes.
		offset := 0
		for chars := 0; chars < maxChars && offset < len(text); chars++ {
			_, size := utf8.DecodeRune(text[offset:])
			offset += size
		}
		if offset < len(text) || partial {
			return string(text[:offset]) + fmt.Sprintf("\n\n... [truncated to %d characters; file size %d bytes]", maxChars, fileSize), nil
		}
		return string(text), nil
	}

	// 7. Binary: JSON with a base64 prefix of at most maxChars characters.
	returned := decoded
	if limit := maxChars / 4 * 3; len(returned) > limit {
		returned = returned[:limit]
	}
	return marshalResult(binaryDownloadResult{
		RemotePath:    remotePath,
		SizeBytes:     fileSize,
		ReturnedBytes: len(returned),
		Truncated:     int64(len(returned)) < fileSize,
		ContentBase64: base64.StdEncoding.EncodeToString(returned),
	}), nil
}
