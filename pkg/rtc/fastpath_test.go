package rtc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"rpi-ssh-go/pkg/connect"
)

// rawFixture builds a raw stream with command echo, a repeated start marker and
// payload bytes that look like the beginning of markers without being one.
func rawFixture() (start, endPrefix, payload string, stream []byte) {
	start = "__MCP_BEGIN_cafe0123__"
	endPrefix = "__MCP_END_cafe0123_"
	payload = "line one\r\n__MCP_ partial " + endPrefix + "x false end " + endPrefix + "12_y also false\n" +
		"__MCP_BEGIN_cafe0123_ almost start\n" + strings.Repeat("QUJD", 40) + "\n  tail  "
	text := "prompt$ echo '" + start + "'\r\n" + start + "\r\n" + payload + endPrefix + "7__\r\nprompt$ "
	return start, endPrefix, payload, []byte(text)
}

func TestRawCollectorMatchesAtEverySplit(t *testing.T) {
	start, endPrefix, payload, stream := rawFixture()

	check := func(name string, chunks [][]byte) {
		c := newOutputCollector(start, endPrefix, 100000, true)
		done := false
		for _, chunk := range chunks {
			var err error
			if done, err = c.Feed(chunk); err != nil {
				t.Fatalf("%s: unexpected error: %v", name, err)
			}
		}
		if !done {
			t.Fatalf("%s: end marker not detected", name)
		}
		res := c.Result()
		if res.Stdout != payload {
			t.Fatalf("%s: stdout mismatch\n got %q\nwant %q", name, res.Stdout, payload)
		}
		if res.ExitCode != 7 {
			t.Fatalf("%s: exit code %d, want 7", name, res.ExitCode)
		}
	}

	for i := 0; i <= len(stream); i++ {
		check(fmt.Sprintf("split at %d", i), [][]byte{stream[:i], stream[i:]})
	}
	single := make([][]byte, len(stream))
	for i := range stream {
		single[i] = stream[i : i+1]
	}
	check("one byte at a time", single)
	for _, size := range []int{2, 3, 7, 19, 22, 23} {
		var chunks [][]byte
		for off := 0; off < len(stream); off += size {
			chunks = append(chunks, stream[off:min(off+size, len(stream))])
		}
		check(fmt.Sprintf("chunks of %d", size), chunks)
	}
}

func TestRawCollectorEnforcesLimitInBulk(t *testing.T) {
	start, endPrefix := "__MCP_BEGIN_lim__", "__MCP_END_lim_"

	// Exactly at the limit is accepted, whether or not the end marker shares a chunk.
	for _, split := range []bool{false, true} {
		c := newOutputCollector(start, endPrefix, 1000, true)
		body := start + "\n" + strings.Repeat("C", 1000)
		tail := endPrefix + "0__\n"
		var done bool
		var err error
		if split {
			if _, err = c.Feed([]byte(body)); err == nil {
				done, err = c.Feed([]byte(tail))
			}
		} else {
			done, err = c.Feed([]byte(body + tail))
		}
		if err != nil || !done {
			t.Fatalf("split=%v: output at the limit failed: done=%v err=%v", split, done, err)
		}
		if got := len(c.Result().Stdout); got != 1000 {
			t.Fatalf("split=%v: captured %d bytes, want 1000", split, got)
		}
	}

	// One byte over the limit is refused, with and without a visible end marker.
	c := newOutputCollector(start, endPrefix, 1000, true)
	if _, err := c.Feed([]byte(start + "\n" + strings.Repeat("C", 1001) + endPrefix + "0__\n")); !errors.Is(err, ErrOutputLimit) {
		t.Fatalf("expected ErrOutputLimit for 1001 bytes, got %v", err)
	}
	c = newOutputCollector(start, endPrefix, 1000, true)
	if _, err := c.Feed([]byte(start + "\n" + strings.Repeat("C", 4000))); !errors.Is(err, ErrOutputLimit) {
		t.Fatalf("expected ErrOutputLimit for unterminated oversize output, got %v", err)
	}
}

// countingClient counts device list lookups.
type countingClient struct {
	mockConnectClient
	listCalls atomic.Int32
}

func (c *countingClient) ListDevices(ctx context.Context, forceRefresh bool) ([]connect.DeviceInfo, error) {
	c.listCalls.Add(1)
	return c.mockConnectClient.ListDevices(ctx, forceRefresh)
}

func TestWarmSessionSkipsDeviceListLookup(t *testing.T) {
	client := &countingClient{mockConnectClient: mockConnectClient{
		devices: []connect.DeviceInfo{{ID: "dev-1", Name: "Pi-One", Online: true}},
	}}
	pool := NewPool(nil)
	pool.SetClient(client)

	pc := newTestPeerConnection(t)
	defer pc.Close()
	sess := NewSession("Pi-One", "dev-1", pc, nil, make(chan string, 10))
	pool.globalMu.Lock()
	pool.sessions["dev-1"] = sess
	pool.globalMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, name := range []string{"pi-one", "Pi-One", "dev-1"} {
		got, err := pool.GetOrOpen(ctx, name)
		if err != nil {
			t.Fatalf("GetOrOpen(%q) failed: %v", name, err)
		}
		if got != sess {
			t.Fatalf("GetOrOpen(%q) did not return the live session", name)
		}
	}
	if calls := client.listCalls.Load(); calls != 0 {
		t.Fatalf("expected no device list lookup for a live session, got %d", calls)
	}

	// Once the session is gone the device list is consulted again.
	sess.Close()
	if _, err := pool.resolveDevice(ctx, "pi-one"); err != nil {
		t.Fatalf("resolveDevice after close failed: %v", err)
	}
	if calls := client.listCalls.Load(); calls != 1 {
		t.Fatalf("expected one device list lookup after the session closed, got %d", calls)
	}
}

func BenchmarkRawStreaming(b *testing.B) {
	start, endPrefix := "__MCP_BEGIN_bench__", "__MCP_END_bench_"
	for _, size := range []int{64 * 1024, 1024 * 1024, 8 * 1024 * 1024} {
		b.Run(fmt.Sprintf("%dKiB", size/1024), func(b *testing.B) {
			stream := []byte(start + "\n" + strings.Repeat("QUJD", size/4) + endPrefix + "0__\n")
			b.SetBytes(int64(size))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				c := newOutputCollector(start, endPrefix, 2*size, true)
				for off := 0; off < len(stream); off += 16 * 1024 {
					if _, err := c.Feed(stream[off:min(off+16*1024, len(stream))]); err != nil {
						b.Fatal(err)
					}
				}
				if len(c.Result().Stdout) != size {
					b.Fatal("size mismatch")
				}
			}
		})
	}
}
