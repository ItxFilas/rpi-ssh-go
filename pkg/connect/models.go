package connect

type ICEServerConfig struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

type ICEConfig struct {
	IceCandidatePoolSize int               `json:"iceCandidatePoolSize"`
	IceServers           []ICEServerConfig `json:"iceServers"`
}

type DeviceInfo struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Online  bool   `json:"online"`
	Shell   bool   `json:"shell"`
	Version string `json:"version,omitempty"`
}

type ConnectionResponse struct {
	ID     string `json:"id"`
	Answer *struct {
		Type string `json:"type"`
		SDP  string `json:"sdp"`
	} `json:"answer,omitempty"`
}
