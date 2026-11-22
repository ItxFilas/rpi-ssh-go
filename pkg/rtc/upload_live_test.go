//go:build integration

package rtc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"rpi-ssh-go/pkg/connect"
)

// TestLiveAppendFileBinary tests arbitrary binary bytes (non-UTF8, 0x00..0xFF)
// through Session.AppendFile against a live Raspberry Pi Connect device.
// Excluded from ordinary tests by the integration build tag.
// Requires RPI_TEST_DEVICE environment variable.
func TestLiveAppendFileBinary(t *testing.T) {
	deviceName := os.Getenv("RPI_TEST_DEVICE")
	if deviceName == "" {
		t.Skip("skipping live test: RPI_TEST_DEVICE environment variable not set")
	}

	client, err := connect.NewClient()
	if err != nil {
		t.Fatalf("connect.NewClient failed: %v", err)
	}

	pool := NewPool(client)
	defer pool.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	sess, err := pool.GetOrOpen(ctx, deviceName)
	if err != nil {
		t.Fatalf("failed to connect to live device %q: %v", deviceName, err)
	}

	// 1. Create unique temporary remote directory
	mktempRes, err := sess.Exec(ctx, "mktemp -d /tmp/_mcp_binary_live.XXXXXXXXXX", 1000)
	if err != nil || mktempRes.ExitCode != 0 {
		t.Fatalf("failed to create temporary remote directory: %v (exit: %d, out: %s)", err, mktempRes.ExitCode, mktempRes.Stdout)
	}
	remoteDir := strings.TrimSpace(mktempRes.Stdout)
	if !strings.HasPrefix(remoteDir, "/tmp/_mcp_binary_live.") || strings.ContainsAny(strings.TrimPrefix(remoteDir, "/tmp/"), "/'\\\n\r") {
		t.Fatalf("unexpected temporary directory %q", remoteDir)
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_, _ = sess.Exec(cleanupCtx, fmt.Sprintf("rm -rf -- '%s'", remoteDir), 1000)
	}()

	// 2. Generate arbitrary binary data (all byte values 0..255, including NULs and invalid UTF-8)
	binaryData := make([]byte, 65536)
	for i := range binaryData {
		binaryData[i] = byte(i % 256)
	}
	localSum := sha256.Sum256(binaryData)
	localHash := hex.EncodeToString(localSum[:])

	remoteFilePath := fmt.Sprintf("%s/binary_payload.bin", remoteDir)

	// 3. AppendFile binary payload
	t0 := time.Now()
	if err := sess.AppendFile(ctx, remoteFilePath, binaryData); err != nil {
		t.Fatalf("AppendFile failed on arbitrary binary payload: %v", err)
	}
	elapsed := time.Since(t0)
	t.Logf("Uploaded 64 KiB arbitrary binary payload in %v", elapsed)

	// 4. Verify remote SHA-256
	hashRes, err := sess.Exec(ctx, fmt.Sprintf("sha256sum -- '%s'", remoteFilePath), 1000)
	if err != nil || hashRes.ExitCode != 0 {
		t.Fatalf("remote sha256sum failed: %v (exit: %d, out: %s)", err, hashRes.ExitCode, hashRes.Stdout)
	}

	fields := strings.Fields(hashRes.Stdout)
	if len(fields) == 0 {
		t.Fatalf("empty output from remote sha256sum: %q", hashRes.Stdout)
	}
	remoteHash := strings.ToLower(fields[0])
	if remoteHash != localHash {
		t.Fatalf("SHA-256 mismatch on arbitrary binary upload! Expected %s, got %s", localHash, remoteHash)
	}

	t.Logf("Live binary upload verified successfully: SHA-256 matched (%s)", remoteHash)
	// A directory cannot be opened as the decoder output. Verify immediate failure
	// and a usable fresh session, with no payload left for the shell to interpret.
	failureCtx, failureCancel := context.WithTimeout(ctx, 5*time.Second)
	err = sess.AppendFile(failureCtx, remoteDir, binaryData)
	failureCancel()
	if err == nil || !strings.Contains(err.Error(), "exit code 1") {
		t.Fatalf("expected receiver write failure, got %v", err)
	}
	if !sess.IsClosed() {
		t.Fatal("failed receiver session not closed")
	}
	reconnected, err := pool.GetOrOpen(ctx, deviceName)
	if err != nil {
		t.Fatalf("reconnect after receiver failure: %v", err)
	}
	sess = reconnected
	recovered, err := sess.Exec(ctx, "echo receiver-recovered", 1000)
	if err != nil || recovered.ExitCode != 0 || recovered.Stdout != "receiver-recovered" {
		t.Fatalf("recovery failed: %v %+v", err, recovered)
	}
	t.Log("Receiver write failure and fresh-session recovery passed")

}
