// Package rtctest provides WebRTC fixtures for tests: peers joined through
// Pion's in-process network (never OS listeners) and a real bash behind a
// data channel that stands in for the remote shell on a Raspberry Pi.
package rtctest

import (
	"bytes"
	"io"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/logging"
	"github.com/pion/transport/v5/vnet"
	"github.com/pion/webrtc/v4"
)

// NewRouter starts an in-memory network for test peers.
func NewRouter(t testing.TB) *vnet.Router {
	t.Helper()
	router, err := vnet.NewRouter(&vnet.RouterConfig{CIDR: "10.77.0.0/24", LoggerFactory: logging.NewDefaultLoggerFactory()})
	if err != nil {
		t.Fatal(err)
	}
	if err = router.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = router.Stop() })
	return router
}

// NewPeer creates a peer connection attached to router with the given address.
func NewPeer(t testing.TB, router *vnet.Router, ip string, configs ...webrtc.Configuration) *webrtc.PeerConnection {
	t.Helper()
	network, err := vnet.NewNet(&vnet.NetConfig{StaticIPs: []string{ip}})
	if err != nil {
		t.Fatal(err)
	}
	if err = router.AddNet(network); err != nil {
		t.Fatal(err)
	}
	settings := webrtc.SettingEngine{}
	settings.SetNet(network)
	settings.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	config := webrtc.Configuration{}
	if len(configs) > 0 {
		config = configs[0]
	}
	pc, err := webrtc.NewAPI(webrtc.WithSettingEngine(settings)).NewPeerConnection(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	return pc
}

// LoopbackPair connects two real WebRTC peers through an in-memory router and
// returns them with the two open ends of a "shell" data channel.
func LoopbackPair(t testing.TB) (*webrtc.PeerConnection, *webrtc.PeerConnection, *webrtc.DataChannel, *webrtc.DataChannel) {
	t.Helper()
	router := NewRouter(t)
	pc1 := NewPeer(t, router, "10.77.0.1")
	pc2 := NewPeer(t, router, "10.77.0.2")

	fail := func(format string, args ...any) {
		pc1.Close()
		pc2.Close()
		t.Fatalf(format, args...)
	}

	dc1, err := pc1.CreateDataChannel("shell", nil)
	if err != nil {
		fail("failed to create dc1: %v", err)
	}

	dc2Chan := make(chan *webrtc.DataChannel, 1)
	pc2.OnDataChannel(func(d *webrtc.DataChannel) {
		dc2Chan <- d
	})

	pc1.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			_ = pc2.AddICECandidate(c.ToJSON())
		}
	})
	pc2.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			_ = pc1.AddICECandidate(c.ToJSON())
		}
	})

	offer, err := pc1.CreateOffer(nil)
	if err != nil {
		fail("create offer error: %v", err)
	}
	if err := pc1.SetLocalDescription(offer); err != nil {
		fail("set local description error: %v", err)
	}
	if err := pc2.SetRemoteDescription(offer); err != nil {
		fail("set remote description error: %v", err)
	}

	answer, err := pc2.CreateAnswer(nil)
	if err != nil {
		fail("create answer error: %v", err)
	}
	if err := pc2.SetLocalDescription(answer); err != nil {
		fail("set local description error: %v", err)
	}
	if err := pc1.SetRemoteDescription(answer); err != nil {
		fail("set remote description error: %v", err)
	}

	var dc2 *webrtc.DataChannel
	select {
	case dc2 = <-dc2Chan:
	case <-time.After(2 * time.Second):
		fail("timed out waiting for data channel on pc2")
	}

	open1 := make(chan struct{})
	open2 := make(chan struct{})
	dc1.OnOpen(func() { close(open1) })
	dc2.OnOpen(func() { close(open2) })

	select {
	case <-open1:
	case <-time.After(2 * time.Second):
		fail("timed out waiting for dc1 to open")
	}
	select {
	case <-open2:
	case <-time.After(2 * time.Second):
		fail("timed out waiting for dc2 to open")
	}

	return pc1, pc2, dc1, dc2
}

// StartBashShell runs a real bash in dir behind dc: everything received on
// the channel is typed into the shell and everything the shell prints is sent
// back, as the remote shell of a Raspberry Pi does. The test is skipped when
// bash is not installed.
//
// Unlike a real terminal there is no echo and no prompt, which the command
// framing does not depend on.
func StartBashShell(t testing.TB, dc *webrtc.DataChannel, dir string) {
	t.Helper()
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash fixture requires bash in PATH")
	}

	cmd := exec.Command(bashPath, "--noprofile", "--norc")
	cmd.Dir = dir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	// Children of the shell (a sleep that outlives the test) keep the
	// output pipe open; do not wait for them after the shell is killed.
	cmd.WaitDelay = 500 * time.Millisecond
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	var writeMu sync.Mutex
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		// Keepalive payloads are NUL bytes, which a terminal discards.
		data := bytes.ReplaceAll(msg.Data, []byte{0}, nil)
		if len(data) == 0 {
			return
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		_, _ = stdin.Write(data)
	})

	go func() {
		buf := make([]byte, 16*1024)
		for {
			n, err := pr.Read(buf)
			if n > 0 {
				_ = dc.Send(append([]byte(nil), buf[:n]...))
			}
			if err != nil {
				return
			}
		}
	}()

	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = pw.Close()
	})
}
