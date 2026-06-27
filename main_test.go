package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
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
	srv.DBPath = "./config_test_e2e.db"
	_ = os.Remove(srv.DBPath)
	defer os.Remove(srv.DBPath)
	defer srv.Close()
	
	// Start server in background
	go func() {
		if err := srv.Start(); err != nil {
			t.Logf("server exit: %v", err)
		}
	}()
	time.Sleep(2000 * time.Millisecond) // Wait for server setup

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

func TestMTMTunnelStaticDeployment(t *testing.T) {
	// Create a temp dir with a mock index.html
	tempDir := t.TempDir()
	htmlContent := "Hello from Deploy Static HTML File!"
	err := os.WriteFile(filepath.Join(tempDir, "index.html"), []byte(htmlContent), 0644)
	if err != nil {
		t.Fatalf("failed to write temp html file: %v", err)
	}

	// 2. Start MTM Server on random ports
	quicListener, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start UDP listener: %v", err)
	}
	quicAddrStr := quicListener.LocalAddr().String()
	quicListener.Close()

	httpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start HTTP listener: %v", err)
	}
	httpPort := fmt.Sprintf("%d", httpListener.Addr().(*net.TCPAddr).Port)
	httpListener.Close()

	httpsListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start HTTPS listener: %v", err)
	}
	httpsPort := fmt.Sprintf("%d", httpsListener.Addr().(*net.TCPAddr).Port)
	httpsListener.Close()

	authToken := "test_token_12345"
	domain := "localhost"

	srv := server.NewTunnelServer(quicAddrStr, domain, authToken, "test@localhost", httpPort, httpsPort)
	srv.DBPath = "./config_test_static.db"
	_ = os.Remove(srv.DBPath)
	defer os.Remove(srv.DBPath)
	defer srv.Close()
	
	go func() {
		if err := srv.Start(); err != nil {
			log.Printf("[Test Server] start error: %v", err)
		}
	}()
	time.Sleep(2000 * time.Millisecond)

	// Deploy using runDeploy
	clientArgs := []string{
		"-server", quicAddrStr,
		"-subdomain", "staticapp",
		"-token", authToken,
		"-dir", tempDir,
	}

	os.Setenv("MTM_HTTP_PORT", httpPort)
	defer os.Unsetenv("MTM_HTTP_PORT")
	runDeploy(clientArgs)

	// Query the server to get the static content directly (since tunnel is offline, it should serve static file)
	visitorClient := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%s/", httpPort), nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	req.Host = "staticapp.localhost"

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

	if string(body) != htmlContent {
		t.Errorf("expected response body %q, got %q", htmlContent, string(body))
	}
	t.Logf("Static deploy test PASSED! Content: %s", string(body))

	// Clean up deployed folder from filesystem
	os.RemoveAll(filepath.Join(".", "deployed", "staticapp"))
}
