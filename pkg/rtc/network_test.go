package rtc

import (
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/transport/v5"
	"github.com/pion/transport/v5/stdnet"
	"github.com/pion/webrtc/v4"
	"rpi-ssh-go/pkg/connect"
)

func TestOutboundRelayRejectsMissingCompatibleTURN(t *testing.T) {
	for _, urls := range [][]string{nil, {"stun:localhost:3478"}, {"turn:localhost:3478"}, {"turn:localhost:3478?transport=udp"}, {"turns:localhost:5349?transport=udp"}, {"not-a-url-secret"}} {
		cfg := connect.ICEConfig{IceServers: []connect.ICEServerConfig{{URLs: urls, Username: "user", Credential: "secret"}}}
		pc, err := newPeerConnection(cfg, NetworkMode(""))
		if pc != nil {
			pc.Close()
			t.Fatal("must not create peer without compatible TCP TURN")
		}
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("expected safe compatibility error, got %v", err)
		}
	}
}

func TestNetworkModeDoesNotFallbackToDirect(t *testing.T) {
	cfg := connect.ICEConfig{IceServers: []connect.ICEServerConfig{{URLs: []string{"stun:localhost:3478"}}}}
	for _, mode := range []NetworkMode{"", NetworkModeOutboundRelay, "typo"} {
		pc, err := newPeerConnection(cfg, mode)
		if pc != nil {
			pc.Close()
		}
		if err == nil {
			t.Fatalf("mode %q silently accepted incompatible configuration", mode)
		}
	}
	// Inspect the opt-in configuration without creating OS-backed direct sockets.
	config, _, err := peerConnectionSettings(cfg, NetworkModeDirect)
	if err != nil {
		t.Fatal(err)
	}
	if config.ICETransportPolicy == webrtc.ICETransportPolicyRelay {
		t.Fatal("explicit direct mode unavailable")
	}
}

func TestOutboundRelayFiltersURLs(t *testing.T) {
	cfg := connect.ICEConfig{IceServers: []connect.ICEServerConfig{{URLs: []string{"stun:localhost:3478", "turn:localhost:3478?transport=udp", "turn:localhost:3478?transport=tcp", "turns:localhost:5349"}, Username: "user", Credential: "secret"}}}
	pc, err := newPeerConnection(cfg, NetworkModeOutboundRelay)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	got := pc.GetConfiguration()
	if got.ICETransportPolicy != webrtc.ICETransportPolicyRelay {
		t.Fatal("host candidates permitted")
	}
	if len(got.ICEServers) != 1 || len(got.ICEServers[0].URLs) != 2 {
		t.Fatalf("wrong filtered server count: %d", len(got.ICEServers))
	}
	if got.ICEServers[0].Username != "user" || got.ICEServers[0].Credential != "secret" {
		t.Fatal("relay credentials not preserved")
	}
	if len(cfg.IceServers[0].URLs) != 4 {
		t.Fatal("mutated input configuration")
	}
}

// Deny socket listeners before they reach Windows, including unexpected mDNS.
type listenerGuard struct {
	transport.Net
	listeners atomic.Int32
	dials     atomic.Int32
}

func (n *listenerGuard) ListenPacket(string, string) (net.PacketConn, error) {
	n.listeners.Add(1)
	return nil, errors.New("listener forbidden")
}
func (n *listenerGuard) ListenUDP(string, *net.UDPAddr) (transport.UDPConn, error) {
	n.listeners.Add(1)
	return nil, errors.New("listener forbidden")
}
func (n *listenerGuard) ListenTCP(string, *net.TCPAddr) (transport.TCPListener, error) {
	n.listeners.Add(1)
	return nil, errors.New("listener forbidden")
}
func (n *listenerGuard) DialTCP(string, *net.TCPAddr, *net.TCPAddr) (transport.TCPConn, error) {
	n.dials.Add(1)
	return nil, errors.New("simulated unavailable relay")
}

func TestOutboundRelayExcludesHostCandidates(t *testing.T) {
	cfg := connect.ICEConfig{IceServers: []connect.ICEServerConfig{{URLs: []string{"stun:127.0.0.1:9", "turn:127.0.0.1:9?transport=udp", "turn:127.0.0.1:9?transport=tcp"}, Username: "user", Credential: "secret"}}}
	config, settings, err := peerConnectionSettings(cfg, NetworkModeOutboundRelay)
	if err != nil {
		t.Fatal(err)
	}
	native, err := stdnet.NewNet()
	if err != nil {
		t.Fatal(err)
	}
	guard := &listenerGuard{Net: native}
	settings.SetNet(guard)
	pc, err := webrtc.NewAPI(webrtc.WithSettingEngine(settings)).NewPeerConnection(config)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	var candidates atomic.Int32
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			candidates.Add(1)
		}
	})
	if _, err = pc.CreateDataChannel("shell", nil); err != nil {
		t.Fatal(err)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	done := webrtc.GatheringCompletePromise(pc)
	if err = pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("gathering did not finish")
	}
	if guard.listeners.Load() != 0 {
		t.Fatalf("attempted %d listener sockets", guard.listeners.Load())
	}
	if guard.dials.Load() == 0 {
		t.Fatal("did not try outbound TCP relay")
	}
	if candidates.Load() != 0 {
		t.Fatal("unavailable relay fell back to non-relay candidates")
	}
}
