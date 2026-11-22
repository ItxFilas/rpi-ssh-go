package rtc

import (
	"fmt"

	"github.com/pion/ice/v4"
	"github.com/pion/stun/v4"
	"github.com/pion/webrtc/v4"
	"rpi-ssh-go/pkg/connect"
)

// NetworkMode controls the local connection to the ICE service.
type NetworkMode string

const (
	NetworkModeOutboundRelay NetworkMode = "outbound-relay"
	NetworkModeDirect        NetworkMode = "direct"
)

func peerConnectionSettings(cfg connect.ICEConfig, mode NetworkMode) (webrtc.Configuration, webrtc.SettingEngine, error) {
	var settings webrtc.SettingEngine
	var config webrtc.Configuration
	if mode == "" {
		mode = NetworkModeOutboundRelay
	}
	if mode != NetworkModeOutboundRelay && mode != NetworkModeDirect {
		return config, settings, fmt.Errorf("invalid RPI_NETWORK_MODE: use outbound-relay (default) or direct")
	}
	if mode == NetworkModeOutboundRelay {
		config.ICETransportPolicy = webrtc.ICETransportPolicyRelay
		// Relay-only ICE alone does not disable Pion's default mDNS listener.
		settings.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	}
	for _, server := range cfg.IceServers {
		urls := make([]string, 0, len(server.URLs))
		for _, raw := range server.URLs {
			if mode == NetworkModeOutboundRelay {
				uri, err := stun.ParseURI(raw)
				if err != nil || (uri.Scheme != stun.SchemeTypeTURN && uri.Scheme != stun.SchemeTypeTURNS) || uri.Proto != stun.ProtoTypeTCP {
					continue
				}
			}
			urls = append(urls, raw)
		}
		if len(urls) > 0 {
			config.ICEServers = append(config.ICEServers, webrtc.ICEServer{URLs: urls, Username: server.Username, Credential: server.Credential, CredentialType: webrtc.ICECredentialTypePassword})
		}
	}
	if mode == NetworkModeOutboundRelay && len(config.ICEServers) == 0 {
		return config, settings, fmt.Errorf("Raspberry Pi Connect did not provide a compatible TCP/TLS TURN relay; outbound-relay mode will not fall back to direct networking")
	}
	return config, settings, nil
}

func newPeerConnection(cfg connect.ICEConfig, mode NetworkMode) (*webrtc.PeerConnection, error) {
	config, settings, err := peerConnectionSettings(cfg, mode)
	if err != nil {
		return nil, err
	}
	return webrtc.NewAPI(webrtc.WithSettingEngine(settings)).NewPeerConnection(config)
}
