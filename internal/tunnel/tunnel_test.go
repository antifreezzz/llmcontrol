package tunnel

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTunnelEndToEnd(t *testing.T) {
	// 1. Setup a mock local target HTTP server (e.g. llmcontrol API)
	localMux := http.NewServeMux()
	localMux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("pong-from-local"))
	})
	localServer := httptest.NewServer(localMux)
	defer localServer.Close()

	localHostPort := localServer.Listener.Addr().String()

	// 2. Pick dynamic ports for Tunnel Server
	ctlListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen ctl: %v", err)
	}
	ctlAddr := ctlListener.Addr().String()
	ctlListener.Close()

	bindListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen bind: %v", err)
	}
	bindAddr := bindListener.Addr().String()
	bindListener.Close()

	_, ctlPortStr, _ := net.SplitHostPort(ctlAddr)
	var ctlPort int
	fmt.Sscanf(ctlPortStr, "%d", &ctlPort)

	// 3. Start Tunnel Server
	srv := NewServer(ServerConfig{
		ControlListen: ctlAddr,
		BindListen:    bindAddr,
		Token:         "secret-test-token",
	})
	defer srv.Close()

	go func() {
		_ = srv.Start()
	}()
	time.Sleep(50 * time.Millisecond)

	// 4. Start Tunnel Client
	mgr := NewClientManager(localHostPort, "127.0.0.1", ctlPort, 0, "secret-test-token")
	err = mgr.Start("test-model", "default")
	if err != nil {
		t.Fatalf("failed to start tunnel client: %v", err)
	}
	defer mgr.Stop()

	// Wait for client to connect
	var connected bool
	for i := 0; i < 20; i++ {
		time.Sleep(50 * time.Millisecond)
		if mgr.GetStatus().State == "connected" {
			connected = true
			break
		}
	}
	if !connected {
		t.Fatalf("tunnel client failed to connect within timeout: %+v", mgr.GetStatus())
	}

	// 5. Send request through the VPS bind address
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://%s/ping", bindAddr))
	if err != nil {
		t.Fatalf("request through tunnel failed: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body failed: %v", err)
	}

	if string(body) != "pong-from-local" {
		t.Fatalf("unexpected response body: got %q, want %q", string(body), "pong-from-local")
	}

	// Check status telemetry
	st := mgr.GetStatus()
	if st.BytesIn == 0 || st.BytesOut == 0 {
		t.Logf("telemetry: in=%d, out=%d", st.BytesIn, st.BytesOut)
	}
}
