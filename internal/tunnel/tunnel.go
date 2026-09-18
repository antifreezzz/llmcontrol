package tunnel

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Status represents the current operational status of the tunnel client.
type Status struct {
	IsActive        bool       `json:"is_active"`
	State           string     `json:"state"` // "disconnected", "connecting", "connected", "error"
	TargetModelID   string     `json:"target_model_id,omitempty"`
	TargetProfile   string     `json:"target_profile,omitempty"`
	VPSHost         string     `json:"vps_host"`
	VPSTunnelPort   int        `json:"vps_tunnel_port"`
	VPSRemotePort   int        `json:"vps_remote_port"`
	ConnectedAt     *time.Time `json:"connected_at,omitempty"`
	BytesIn         int64      `json:"bytes_in"`
	BytesOut        int64      `json:"bytes_out"`
	ActiveStreams   int64      `json:"active_streams"`
	LastError       string     `json:"last_error,omitempty"`
}

// ClientManager controls the local tunnel client lifecycle.
type ClientManager struct {
	mu            sync.RWMutex
	status        Status
	vpsToken      string
	localEndpoint string // e.g. "127.0.0.1:8666"
	cancelFunc    context.CancelFunc
	onStateChange func(Status)
}

func NewClientManager(localEndpoint string, vpsHost string, vpsTunnelPort int, vpsRemotePort int, vpsToken string) *ClientManager {
	if localEndpoint == "" {
		localEndpoint = "127.0.0.1:8666"
	}
	if vpsTunnelPort <= 0 {
		vpsTunnelPort = 8443
	}
	if vpsRemotePort <= 0 {
		vpsRemotePort = 8666
	}

	return &ClientManager{
		localEndpoint: localEndpoint,
		vpsToken:      vpsToken,
		status: Status{
			IsActive:      false,
			State:         "disconnected",
			VPSHost:       vpsHost,
			VPSTunnelPort: vpsTunnelPort,
			VPSRemotePort: vpsRemotePort,
		},
	}
}

func (m *ClientManager) SetOnStateChange(cb func(Status)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onStateChange = cb
}

func (m *ClientManager) GetStatus() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.status
}

func (m *ClientManager) UpdateConfig(host string, tunnelPort, remotePort int, token string, targetModelID ...string) {
	m.mu.Lock()
	if host != "" {
		m.status.VPSHost = host
	}
	if tunnelPort > 0 {
		m.status.VPSTunnelPort = tunnelPort
	}
	if remotePort > 0 {
		m.status.VPSRemotePort = remotePort
	}
	if token != "" {
		m.vpsToken = token
	}
	if len(targetModelID) > 0 {
		m.status.TargetModelID = targetModelID[0]
	}
	if len(targetModelID) > 1 {
		m.status.TargetProfile = targetModelID[1]
	}
	isActive := m.status.IsActive
	targetModel := m.status.TargetModelID
	targetProfile := m.status.TargetProfile
	cb := m.onStateChange
	curr := m.status
	m.mu.Unlock()

	if cb != nil {
		cb(curr)
	}

	if isActive {
		_ = m.Start(targetModel, targetProfile)
	}
}

func (m *ClientManager) SetTargetModel(modelID, profile string) {
	m.mu.Lock()
	m.status.TargetModelID = modelID
	if profile != "" {
		m.status.TargetProfile = profile
	}
	isActive := m.status.IsActive
	targetModel := m.status.TargetModelID
	targetProfile := m.status.TargetProfile
	cb := m.onStateChange
	curr := m.status
	m.mu.Unlock()

	if cb != nil {
		cb(curr)
	}

	if isActive {
		_ = m.Start(targetModel, targetProfile)
	}
}

func (m *ClientManager) Start(modelID, profile string) error {
	m.mu.Lock()
	if m.status.IsActive && m.cancelFunc != nil {
		m.cancelFunc()
	}

	if m.status.VPSHost == "" {
		m.mu.Unlock()
		return errors.New("vps_host is not configured")
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.cancelFunc = cancel
	m.status.IsActive = true
	m.status.State = "connecting"
	m.status.TargetModelID = modelID
	m.status.TargetProfile = profile
	m.status.LastError = ""

	host := m.status.VPSHost
	tunnelPort := m.status.VPSTunnelPort
	token := m.vpsToken
	localEP := m.localEndpoint

	cb := m.onStateChange
	currentStatus := m.status
	m.mu.Unlock()

	if cb != nil {
		cb(currentStatus)
	}

	go m.runLoop(ctx, host, tunnelPort, token, localEP, modelID)
	return nil
}

func (m *ClientManager) Stop() {
	m.mu.Lock()
	if m.cancelFunc != nil {
		m.cancelFunc()
		m.cancelFunc = nil
	}
	m.status.IsActive = false
	m.status.State = "disconnected"
	m.status.ConnectedAt = nil
	cb := m.onStateChange
	curr := m.status
	m.mu.Unlock()

	if cb != nil {
		cb(curr)
	}
}

func (m *ClientManager) runLoop(ctx context.Context, vpsHost string, tunnelPort int, token string, localEP string, modelID string) {
	backoff := time.Second
	remoteAddr := fmt.Sprintf("%s:%d", vpsHost, tunnelPort)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		m.mu.Lock()
		m.status.State = "connecting"
		m.mu.Unlock()

		err := m.runClientSession(ctx, remoteAddr, token, localEP, modelID)
		if ctx.Err() != nil {
			return
		}

		m.mu.Lock()
		m.status.State = "error"
		if err != nil {
			m.status.LastError = err.Error()
		}
		m.status.ConnectedAt = nil
		cb := m.onStateChange
		curr := m.status
		m.mu.Unlock()

		if cb != nil {
			cb(curr)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
			if backoff < 15*time.Second {
				backoff *= 2
			}
		}
	}
}

func (m *ClientManager) runClientSession(ctx context.Context, remoteAddr, token, localEP, modelID string) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", remoteAddr)
	if err != nil {
		return fmt.Errorf("dial VPS control port: %w", err)
	}
	defer conn.Close()

	// Handshake
	hello := fmt.Sprintf("CONTROL %s %s\n", token, modelID)
	if _, err := conn.Write([]byte(hello)); err != nil {
		return fmt.Errorf("send control handshake: %w", err)
	}

	reader := bufio.NewReader(conn)
	resp, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read handshake response: %w", err)
	}
	resp = strings.TrimSpace(resp)
	if !strings.HasPrefix(resp, "OK") {
		return fmt.Errorf("control handshake rejected: %s", resp)
	}

	now := time.Now()
	m.mu.Lock()
	m.status.State = "connected"
	m.status.ConnectedAt = &now
	m.status.LastError = ""
	cb := m.onStateChange
	curr := m.status
	m.mu.Unlock()

	if cb != nil {
		cb(curr)
	}

	// Listen for incoming STREAM requests
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		line, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("control connection closed: %w", err)
		}
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "STREAM ") {
			streamID := strings.TrimPrefix(line, "STREAM ")
			go m.handleStream(ctx, remoteAddr, token, localEP, streamID)
		} else if strings.HasPrefix(line, "PING") {
			_, _ = conn.Write([]byte("PONG\n"))
		}
	}
}

func (m *ClientManager) handleStream(ctx context.Context, remoteAddr, token, localEP, streamID string) {
	atomic.AddInt64(&m.status.ActiveStreams, 1)
	defer atomic.AddInt64(&m.status.ActiveStreams, -1)

	// Dial VPS data port
	var d net.Dialer
	dataConn, err := d.DialContext(ctx, "tcp", remoteAddr)
	if err != nil {
		log.Printf("[tunnel] failed to dial VPS for stream %s: %v", streamID, err)
		return
	}
	defer dataConn.Close()

	// Handshake data stream
	handshake := fmt.Sprintf("DATA %s %s\n", token, streamID)
	if _, err := dataConn.Write([]byte(handshake)); err != nil {
		return
	}

	// Dial local service
	localConn, err := net.DialTimeout("tcp", localEP, 3*time.Second)
	if err != nil {
		log.Printf("[tunnel] failed to dial local endpoint %s: %v", localEP, err)
		return
	}
	defer localConn.Close()

	// Bidirectional copy
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		n, _ := io.Copy(localConn, dataConn)
		atomic.AddInt64(&m.status.BytesIn, n)
		if tc, ok := localConn.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()

	go func() {
		defer wg.Done()
		n, _ := io.Copy(dataConn, localConn)
		atomic.AddInt64(&m.status.BytesOut, n)
		if tc, ok := dataConn.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()

	wg.Wait()
}

// --------------------------------------------------------------------------
// Server Side (runs on VPS)
// --------------------------------------------------------------------------

type ServerConfig struct {
	ControlListen string // e.g. ":8443"
	BindListen    string // e.g. "127.0.0.1:8666"
	Token         string
}

type Server struct {
	cfg            ServerConfig
	mu             sync.RWMutex
	controlConn    net.Conn
	activeModel    string
	pendingStreams map[string]chan net.Conn
	stopChan       chan struct{}
}

func NewServer(cfg ServerConfig) *Server {
	if cfg.ControlListen == "" {
		cfg.ControlListen = ":8443"
	}
	if cfg.BindListen == "" {
		cfg.BindListen = "127.0.0.1:8666"
	}

	return &Server{
		cfg:            cfg,
		pendingStreams: make(map[string]chan net.Conn),
		stopChan:       make(chan struct{}),
	}
}

func (s *Server) Start() error {
	ctlListener, err := net.Listen("tcp", s.cfg.ControlListen)
	if err != nil {
		return fmt.Errorf("listen control on %s: %w", s.cfg.ControlListen, err)
	}
	defer ctlListener.Close()

	bindListener, err := net.Listen("tcp", s.cfg.BindListen)
	if err != nil {
		return fmt.Errorf("listen bind on %s: %w", s.cfg.BindListen, err)
	}
	defer bindListener.Close()

	log.Printf("[tunnel-server] listening control on %s, proxying on %s", s.cfg.ControlListen, s.cfg.BindListen)

	go s.acceptProxyConnections(bindListener)

	for {
		conn, err := ctlListener.Accept()
		if err != nil {
			select {
			case <-s.stopChan:
				return nil
			default:
				log.Printf("[tunnel-server] accept error: %v", err)
				continue
			}
		}

		go s.handleInboundControlOrData(conn)
	}
}

func (s *Server) Close() {
	close(s.stopChan)
	s.mu.Lock()
	if s.controlConn != nil {
		_ = s.controlConn.Close()
		s.controlConn = nil
	}
	s.mu.Unlock()
}

func (s *Server) handleInboundControlOrData(conn net.Conn) {
	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		conn.Close()
		return
	}
	line = strings.TrimSpace(line)
	parts := strings.Split(line, " ")
	if len(parts) < 2 {
		conn.Write([]byte("ERR invalid_command\n"))
		conn.Close()
		return
	}

	cmd := parts[0]
	token := parts[1]

	if s.cfg.Token != "" && token != s.cfg.Token {
		conn.Write([]byte("ERR unauthorized\n"))
		conn.Close()
		return
	}

	switch cmd {
	case "CONTROL":
		model := "default"
		if len(parts) >= 3 {
			model = parts[2]
		}
		s.mu.Lock()
		if s.controlConn != nil {
			_ = s.controlConn.Close()
		}
		s.controlConn = conn
		s.activeModel = model
		s.mu.Unlock()

		_, _ = conn.Write([]byte("OK\n"))
		log.Printf("[tunnel-server] client connected from %s (target model: %s)", conn.RemoteAddr(), model)

	case "DATA":
		if len(parts) < 3 {
			conn.Close()
			return
		}
		streamID := parts[2]
		s.mu.Lock()
		ch, ok := s.pendingStreams[streamID]
		if ok {
			delete(s.pendingStreams, streamID)
		}
		s.mu.Unlock()

		if ok && ch != nil {
			ch <- conn
		} else {
			conn.Close()
		}

	default:
		conn.Write([]byte("ERR unknown_command\n"))
		conn.Close()
	}
}

func (s *Server) acceptProxyConnections(listener net.Listener) {
	for {
		clientConn, err := listener.Accept()
		if err != nil {
			select {
			case <-s.stopChan:
				return
			default:
				log.Printf("[tunnel-server] proxy accept error: %v", err)
				continue
			}
		}

		go s.dispatchProxyConnection(clientConn)
	}
}

func (s *Server) dispatchProxyConnection(clientConn net.Conn) {
	defer clientConn.Close()

	s.mu.RLock()
	ctl := s.controlConn
	s.mu.RUnlock()

	if ctl == nil {
		log.Printf("[tunnel-server] rejected request on proxy: no client connected")
		return
	}

	streamID := newStreamID()
	dataChan := make(chan net.Conn, 1)

	s.mu.Lock()
	s.pendingStreams[streamID] = dataChan
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.pendingStreams, streamID)
		s.mu.Unlock()
	}()

	// Ask client for a data stream
	if _, err := fmt.Fprintf(ctl, "STREAM %s\n", streamID); err != nil {
		log.Printf("[tunnel-server] failed to write to control connection: %v", err)
		return
	}

	select {
	case dataConn := <-dataChan:
		defer dataConn.Close()
		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			defer wg.Done()
			_, _ = io.Copy(dataConn, clientConn)
			if tc, ok := dataConn.(*net.TCPConn); ok {
				_ = tc.CloseWrite()
			}
		}()

		go func() {
			defer wg.Done()
			_, _ = io.Copy(clientConn, dataConn)
			if tc, ok := clientConn.(*net.TCPConn); ok {
				_ = tc.CloseWrite()
			}
		}()

		wg.Wait()

	case <-time.After(10 * time.Second):
		log.Printf("[tunnel-server] timeout waiting for stream %s", streamID)
	case <-s.stopChan:
		return
	}
}

func newStreamID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
