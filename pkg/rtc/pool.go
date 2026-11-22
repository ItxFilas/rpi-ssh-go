package rtc

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"rpi-ssh-go/pkg/connect"

	"github.com/pion/webrtc/v4"
)

type deviceLockEntry struct {
	sem      chan struct{}
	refCount int
}

type DeviceResolverFunc func(nameOrID string) (*connect.DeviceInfo, error)
type ConnectorFunc func(target *connect.DeviceInfo) (*Session, error)
type PeerConnectionFactory func(cfg connect.ICEConfig, mode NetworkMode) (*webrtc.PeerConnection, error)

type ConnectClient interface {
	ListDevices(ctx context.Context, forceRefresh bool) ([]connect.DeviceInfo, error)
	GetShellSessionInfo(ctx context.Context, deviceID string) (connect.ICEConfig, string, error)
	PostOffer(ctx context.Context, deviceID, sdp, csrfToken, referer string) (string, error)
	PollAnswer(ctx context.Context, deviceID, connID, referer string, maxDuration time.Duration) (string, error)
}

type Pool struct {
	client                ConnectClient
	sessions              map[string]*Session         // Keyed by canonical device ID
	deviceLocks           map[string]*deviceLockEntry // Keyed by canonical device ID, ref-counted
	deviceEpoch           map[string]uint64           // Generation counter per canonical device ID
	globalMu              sync.Mutex
	closed                bool
	deviceResolver        DeviceResolverFunc
	connector             ConnectorFunc
	peerConnectionFactory PeerConnectionFactory
	setupTimeout          time.Duration
}

// DefaultSetupTimeout bounds resolving a device and opening its session.
const DefaultSetupTimeout = 30 * time.Second

func NewPool(client *connect.Client) *Pool {
	p := &Pool{
		sessions:     make(map[string]*Session),
		deviceLocks:  make(map[string]*deviceLockEntry),
		deviceEpoch:  make(map[string]uint64),
		setupTimeout: DefaultSetupTimeout,
	}
	if client != nil {
		p.client = client
	}
	return p
}

func (p *Pool) SetClient(client ConnectClient) {
	p.globalMu.Lock()
	defer p.globalMu.Unlock()
	p.client = client
}

// SetSetupTimeout changes how long GetOrOpen may spend resolving a device and
// opening its session. Callers budget the connection separately from the
// command that follows it.
func (p *Pool) SetSetupTimeout(d time.Duration) {
	p.globalMu.Lock()
	defer p.globalMu.Unlock()
	if d > 0 {
		p.setupTimeout = d
	}
}

func (p *Pool) SetDeviceResolver(fn DeviceResolverFunc) {
	p.globalMu.Lock()
	defer p.globalMu.Unlock()
	p.deviceResolver = fn
}

func (p *Pool) SetConnector(fn ConnectorFunc) {
	p.globalMu.Lock()
	defer p.globalMu.Unlock()
	p.connector = fn
}

func (p *Pool) SetPeerConnectionFactory(fn PeerConnectionFactory) {
	p.globalMu.Lock()
	defer p.globalMu.Unlock()
	p.peerConnectionFactory = fn
}

func (p *Pool) acquireDeviceLock(ctx context.Context, canonicalID string) (*deviceLockEntry, error) {
	p.globalMu.Lock()
	if p.closed {
		p.globalMu.Unlock()
		return nil, fmt.Errorf("session pool is closed")
	}

	entry, ok := p.deviceLocks[canonicalID]
	if !ok {
		entry = &deviceLockEntry{
			sem: make(chan struct{}, 1),
		}
		entry.sem <- struct{}{}
		p.deviceLocks[canonicalID] = entry
	}
	entry.refCount++
	p.globalMu.Unlock()

	select {
	case <-entry.sem:
		if ctx.Err() != nil {
			p.releaseDeviceLock(canonicalID, entry)
			return nil, ctx.Err()
		}
		return entry, nil
	case <-ctx.Done():
		p.globalMu.Lock()
		entry.refCount--
		if entry.refCount <= 0 {
			delete(p.deviceLocks, canonicalID)
		}
		p.globalMu.Unlock()
		return nil, ctx.Err()
	}
}

func (p *Pool) releaseDeviceLock(canonicalID string, entry *deviceLockEntry) {
	entry.sem <- struct{}{}

	p.globalMu.Lock()
	entry.refCount--
	if entry.refCount <= 0 {
		delete(p.deviceLocks, canonicalID)
	}
	p.globalMu.Unlock()
}

func (p *Pool) resolveDevice(ctx context.Context, nameOrID string) (*connect.DeviceInfo, error) {
	p.globalMu.Lock()
	resolver := p.deviceResolver
	client := p.client
	p.globalMu.Unlock()

	if resolver != nil {
		return resolver(nameOrID)
	}

	// Warm path: a live session already identifies the device, so no HTTP
	// round trip to the device list is needed before using it.
	if target := p.liveSessionDevice(nameOrID); target != nil {
		return target, nil
	}

	var listErr error
	if client != nil {
		devices, err := client.ListDevices(ctx, false)
		if err == nil {
			for _, d := range devices {
				if strings.EqualFold(d.Name, nameOrID) || d.ID == nameOrID {
					return &d, nil
				}
			}
			return nil, fmt.Errorf("device '%s' not found", nameOrID)
		}
		listErr = err
	}

	// Fallback check against existing sessions in pool
	p.globalMu.Lock()
	for id, sess := range p.sessions {
		if sess != nil && (strings.EqualFold(sess.DeviceName, nameOrID) || sess.DeviceID == nameOrID || id == nameOrID) {
			target := &connect.DeviceInfo{
				ID:     sess.DeviceID,
				Name:   sess.DeviceName,
				Online: true,
			}
			p.globalMu.Unlock()
			return target, nil
		}
	}
	p.globalMu.Unlock()

	// Synthetic fallback: assume nameOrID is both Name and ID only when client == nil
	if client == nil {
		return &connect.DeviceInfo{
			ID:     nameOrID,
			Name:   nameOrID,
			Online: true,
		}, nil
	}

	// The list could not be fetched, so whether the device exists is
	// unknown. Report the real failure (expired login, network) instead.
	if listErr != nil {
		return nil, fmt.Errorf("could not look up device '%s': %w", nameOrID, listErr)
	}
	return nil, fmt.Errorf("device '%s' not found", nameOrID)
}

// liveSessionDevice returns the device of an open session matching nameOrID,
// preferring an exact ID match over a name match, or nil if there is none.
func (p *Pool) liveSessionDevice(nameOrID string) *connect.DeviceInfo {
	p.globalMu.Lock()
	var match *Session
	canonicalID := nameOrID
	if sess := p.sessions[nameOrID]; sess != nil {
		match = sess
	} else {
		for id, sess := range p.sessions {
			if sess != nil && (sess.DeviceID == nameOrID || strings.EqualFold(sess.DeviceName, nameOrID)) {
				match, canonicalID = sess, id
				if sess.DeviceID == nameOrID {
					break
				}
			}
		}
	}
	p.globalMu.Unlock()

	if match == nil || match.IsClosed() {
		return nil
	}
	// The pool key is the canonical device ID the session was opened under.
	return &connect.DeviceInfo{ID: canonicalID, Name: match.DeviceName, Online: true}
}

func (p *Pool) GetOrOpen(ctx context.Context, deviceName string) (*Session, error) {
	p.globalMu.Lock()
	if p.closed {
		p.globalMu.Unlock()
		return nil, fmt.Errorf("session pool is closed")
	}
	setupTimeout := p.setupTimeout
	p.globalMu.Unlock()

	// Setup has its own budget (capped by the parent context).
	setupCtx, cancel := context.WithTimeout(ctx, setupTimeout)
	defer cancel()

	target, err := p.resolveDevice(setupCtx, deviceName)
	if err != nil {
		return nil, err
	}
	canonicalID := target.ID

	// Serialize per canonical device without holding global map lock across network I/O
	devLock, err := p.acquireDeviceLock(setupCtx, canonicalID)
	if err != nil {
		return nil, err
	}
	defer p.releaseDeviceLock(canonicalID, devLock)

	p.globalMu.Lock()
	if p.closed {
		p.globalMu.Unlock()
		return nil, fmt.Errorf("session pool is closed")
	}
	epoch := p.deviceEpoch[canonicalID]
	existing, ok := p.sessions[canonicalID]
	p.globalMu.Unlock()

	if ok && existing != nil && !existing.IsClosed() {
		return existing, nil
	}

	// Session is either non-existent or dead, clean up old one if exists
	if ok && existing != nil {
		existing.Close()
		p.globalMu.Lock()
		delete(p.sessions, canonicalID)
		p.globalMu.Unlock()
	}

	sess, err := p.connect(setupCtx, target)
	if err != nil {
		return nil, err
	}

	p.globalMu.Lock()
	if p.closed || p.deviceEpoch[canonicalID] != epoch {
		p.globalMu.Unlock()
		sess.Close()
		return nil, fmt.Errorf("session aborted: device disconnected or pool closed during connection")
	}
	p.sessions[canonicalID] = sess
	p.globalMu.Unlock()

	return sess, nil
}

func (p *Pool) Disconnect(ctx context.Context, deviceName string) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}

	target, err := p.resolveDevice(ctx, deviceName)
	canonicalID := deviceName
	if err == nil && target != nil && target.ID != "" {
		canonicalID = target.ID
	}

	p.globalMu.Lock()
	p.deviceEpoch[canonicalID]++
	sess, ok := p.sessions[canonicalID]
	if !ok {
		// Fallback check by name or id if map was keyed differently
		for id, s := range p.sessions {
			if s != nil && (strings.EqualFold(s.DeviceName, deviceName) || s.DeviceID == deviceName) {
				sess = s
				canonicalID = id
				ok = true
				break
			}
		}
	}
	if ok {
		delete(p.sessions, canonicalID)
	}
	p.globalMu.Unlock()

	if ok && sess != nil {
		sess.Close()
		displayName := sess.DeviceName
		if displayName == "" {
			displayName = deviceName
		}
		return fmt.Sprintf("Disconnected persistent session for '%s'.", displayName), nil
	}
	return fmt.Sprintf("No active session found for '%s'.", deviceName), nil
}

func (p *Pool) ActiveSessionNames() []string {
	p.globalMu.Lock()
	defer p.globalMu.Unlock()

	var names []string
	for _, sess := range p.sessions {
		if sess != nil && !sess.IsClosed() {
			if sess.DeviceName != "" {
				names = append(names, sess.DeviceName)
			} else {
				names = append(names, sess.DeviceID)
			}
		}
	}
	return names
}

func (p *Pool) Close() {
	p.globalMu.Lock()
	if p.closed {
		p.globalMu.Unlock()
		return
	}
	p.closed = true

	toClose := make([]*Session, 0, len(p.sessions))
	for id, sess := range p.sessions {
		if sess != nil {
			toClose = append(toClose, sess)
		}
		delete(p.sessions, id)
	}
	p.globalMu.Unlock()

	for _, sess := range toClose {
		sess.Close()
	}
}

func (p *Pool) connect(ctx context.Context, target *connect.DeviceInfo) (*Session, error) {
	p.globalMu.Lock()
	connector := p.connector
	pcFactory := p.peerConnectionFactory
	p.globalMu.Unlock()

	if connector != nil {
		sess, err := connector(target)
		if err != nil {
			return nil, err
		}
		sess.StartKeepalive(DefaultKeepaliveInterval)
		return sess, nil
	}

	if p.client == nil {
		return nil, fmt.Errorf("connect client is not configured")
	}

	if !target.Online {
		return nil, fmt.Errorf("device '%s' is currently offline", target.Name)
	}

	iceCfg, csrfToken, err := p.client.GetShellSessionInfo(ctx, target.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to get shell session info: %w", err)
	}

	mode := NetworkMode(os.Getenv("RPI_NETWORK_MODE"))
	var peerConnection *webrtc.PeerConnection
	if pcFactory != nil {
		peerConnection, err = pcFactory(iceCfg, mode)
	} else {
		peerConnection, err = newPeerConnection(iceCfg, mode)
	}
	if err != nil {
		return nil, fmt.Errorf("new peer connection error: %w", err)
	}

	var session *Session
	var success bool
	defer func() {
		if !success {
			if session != nil {
				session.Close()
			} else if peerConnection != nil {
				_ = peerConnection.Close()
			}
		}
	}()

	dc, err := peerConnection.CreateDataChannel("shell", nil)
	if err != nil {
		return nil, fmt.Errorf("create data channel error: %w", err)
	}

	openChan := make(chan bool, 1)

	session = AttachSession(target.Name, target.ID, peerConnection, dc)

	dc.OnOpen(func() {
		select {
		case openChan <- true:
		default:
		}
	})

	// "Disconnected" is transient: ICE either recovers or reports "failed".
	peerConnection.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if s == webrtc.PeerConnectionStateFailed || s == webrtc.PeerConnectionStateClosed {
			session.MarkClosed()
		}
	})

	peerConnection.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) {
		if s == webrtc.ICEConnectionStateFailed || s == webrtc.ICEConnectionStateClosed {
			session.MarkClosed()
		}
	})

	offer, err := peerConnection.CreateOffer(nil)
	if err != nil {
		return nil, fmt.Errorf("create offer error: %w", err)
	}

	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)
	if err = peerConnection.SetLocalDescription(offer); err != nil {
		return nil, fmt.Errorf("set local description error: %w", err)
	}

	select {
	case <-gatherComplete:
	case <-ctx.Done():
		return nil, fmt.Errorf("context cancelled during ICE gathering: %w", ctx.Err())
	}
	finalOffer := peerConnection.LocalDescription()

	shellURL := fmt.Sprintf("https://connect.raspberrypi.com/devices/%s/remote-shell-session", target.ID)
	connID, err := p.client.PostOffer(ctx, target.ID, finalOffer.SDP, csrfToken, shellURL)
	if err != nil {
		return nil, fmt.Errorf("post offer error: %w", err)
	}

	answerSDP, err := p.client.PollAnswer(ctx, target.ID, connID, shellURL, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("poll answer error: %w", err)
	}

	if err := peerConnection.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  answerSDP,
	}); err != nil {
		return nil, fmt.Errorf("set remote description error: %w", err)
	}

	select {
	case <-openChan:
	case <-session.closeChan:
		return nil, fmt.Errorf("WebRTC connection failed before the DataChannel opened")
	case <-ctx.Done():
		return nil, fmt.Errorf("context cancelled waiting for WebRTC DataChannel to open: %w", ctx.Err())
	case <-time.After(12 * time.Second):
		return nil, fmt.Errorf("timed out waiting for WebRTC DataChannel to open")
	}

	// Initialize virtual PTY dimensions (160 cols x 40 rows) to prevent tabular wrapping
	_ = session.InitPTY()

	session.StartKeepalive(DefaultKeepaliveInterval)

	success = true
	return session, nil
}
