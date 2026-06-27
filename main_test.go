package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"go-online/client"
	"go-online/server"
)

func TestMTMTunnelEndToEnd(t *testing.T) {
	// 1. Start a mock user local app (target app)
	localAppHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Custom-Header", "MTM-Test")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Hello from Local Application via MTM Tunnel!"))
	})
	
	localAppListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start local app listener: %v", err)
	}
	defer localAppListener.Close()
	
	localAppPort := localAppListener.Addr().(*net.TCPAddr).Port
	localAppAddr := fmt.Sprintf("127.0.0.1:%d", localAppPort)
	
	go http.Serve(localAppListener, localAppHandler)
	t.Logf("Mock Local Application running at %s", localAppAddr)

	// 2. Start MTM Server on random ports
	// We listen on random UDP port for QUIC and random TCP ports for HTTP
	quicListener, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start UDP listener: %v", err)
	}
	quicAddrStr := quicListener.LocalAddr().String()
	quicListener.Close() // Close it immediately so QUIC can bind to it

	// Create random TCP listeners for HTTP traffic routing
	httpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start HTTP listener: %v", err)
	}
	httpPort := fmt.Sprintf("%d", httpListener.Addr().(*net.TCPAddr).Port)
	httpListener.Close() // Close immediately so server can bind

	httpsListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start HTTPS listener: %v", err)
	}
	httpsPort := fmt.Sprintf("%d", httpsListener.Addr().(*net.TCPAddr).Port)
	httpsListener.Close() // Close immediately so server can bind

	authToken := "test_token_12345"
	domain := "localhost"

	srv := server.NewTunnelServer(quicAddrStr, domain, authToken, "test@localhost", httpPort, httpsPort)
	
	// Start server in background
	go func() {
		if err := srv.Start(); err != nil {
			t.Logf("server exit: %v", err)
		}
	}()
	time.Sleep(500 * time.Millisecond) // Wait for server setup

	// 3. Start local dashboard inspector and client
	inspectorPortStr := "4041" // use different port from default 4040 to avoid conflict
	inspector := client.NewInspectorServer(inspectorPortStr, localAppAddr)
	inspector.Start()

	cli := client.NewTunnelClient(quicAddrStr, "testapp", authToken, localAppAddr, inspector)
	go cli.Start()
	time.Sleep(1 * time.Second) // Wait for tunnel handshake to complete

	// 4. Perform a mock visitor HTTP request to MTM Server
	// The visitor sends request to server's HTTP port with Host: testapp.localhost
	visitorClient := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%s/test-path?param=abc", httpPort), nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	req.Host = "testapp.localhost" // set custom Host header to route through tunnel

	resp, err := visitorClient.Do(req)
	if err != nil {
		t.Fatalf("visitor HTTP request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status OK, got %v", resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}

	expectedBody := "Hello from Local Application via MTM Tunnel!"
	if string(body) != expectedBody {
		t.Errorf("expected response body %q, got %q", expectedBody, string(body))
	}

	if val := resp.Header.Get("X-Custom-Header"); val != "MTM-Test" {
		t.Errorf("expected header X-Custom-Header 'MTM-Test', got %q", val)
	}

	t.Logf("End-to-End tunnel test PASSED! Response body: %s", string(body))

	// 5. Verify the inspector has logged the request
	inspectorClient := &http.Client{Timeout: 2 * time.Second}
	inspectResp, err := inspectorClient.Get(fmt.Sprintf("http://127.0.0.1:%s/api/requests", inspectorPortStr))
	if err != nil {
		t.Fatalf("failed to query inspector API: %v", err)
	}
	defer inspectResp.Body.Close()

	var logs []map[string]interface{}
	if err := json.NewDecoder(inspectResp.Body).Decode(&logs); err != nil {
		t.Fatalf("failed to decode inspector logs: %v", err)
	}

	if len(logs) == 0 {
		t.Error("expected inspector to have logged at least one request, but got 0 logs")
	} else {
		loggedReq := logs[0]
		if loggedReq["method"] != "GET" || loggedReq["url"] != "/test-path?param=abc" {
			t.Errorf("unexpected logged request details: %+v", loggedReq)
		}
		t.Logf("Inspector verified! Logged request URL: %v", loggedReq["url"])
	}
}
