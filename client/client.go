package client

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"go-online/common"

	"github.com/quic-go/quic-go"
	"golang.org/x/net/websocket"
)

type TunnelClient struct {
	ServerAddr string
	Subdomain  string
	AuthToken  string
	TargetAddr string
	Inspector  *InspectorServer
}

func NewTunnelClient(serverAddr, subdomain, authToken, targetAddr string, inspector *InspectorServer) *TunnelClient {
	return &TunnelClient{
		ServerAddr: serverAddr,
		Subdomain:  subdomain,
		AuthToken:  authToken,
		TargetAddr: targetAddr,
		Inspector:  inspector,
	}
}

func (c *TunnelClient) Start() {
	tlsConf := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"mtm-protocol"},
	}

	for {
		log.Printf("[Client] Connecting to tunnel server at %s...", c.ServerAddr)
		conn, err := quic.DialAddr(context.Background(), c.ServerAddr, tlsConf, &quic.Config{
			KeepAlivePeriod: 5 * time.Second,
			MaxIdleTimeout:  30 * time.Second,
		})
		if err != nil {
			log.Printf("[Client] QUIC connection failed: %v. Falling back to WebSocket (TCP)...", err)
			c.startWebSocketTunnel()
			time.Sleep(5 * time.Second)
			continue
		}

		log.Println("[Client] Connected to server via QUIC! Performing handshake...")
		if err := c.runHandshake(conn); err != nil {
			log.Printf("[Client] QUIC handshake failed: %v. Falling back to WebSocket (TCP)...", err)
			conn.CloseWithError(1, "handshake failed")
			c.startWebSocketTunnel()
			time.Sleep(5 * time.Second)
			continue
		}

		log.Println("[Client] QUIC handshake successful! Tunnel is open and ready.")
		
		// Accept streams representing incoming visitor connections
		c.acceptStreams(conn)
		log.Println("[Client] QUIC connection closed. Falling back to WebSocket...")
		c.startWebSocketTunnel()
		time.Sleep(5 * time.Second)
	}
}

func (c *TunnelClient) startWebSocketTunnel() {
	host, port, err := net.SplitHostPort(c.ServerAddr)
	if err != nil {
		host = c.ServerAddr
		port = "443"
	}

	scheme := "wss"
	if host == "localhost" || host == "127.0.0.1" {
		scheme = "ws"
	}

	url := fmt.Sprintf("%s://%s:%s/api/tunnel/ws/control?subdomain=%s&token=%s", scheme, host, port, c.Subdomain, c.AuthToken)
	log.Printf("[Client WS] Connecting to WebSocket tunnel control at %s...", url)

	config, err := websocket.NewConfig(url, "http://"+host)
	if err != nil {
		log.Printf("[Client WS] Failed to create config: %v", err)
		return
	}
	config.TlsConfig = &tls.Config{
		InsecureSkipVerify: true,
	}

	ws, err := websocket.DialConfig(config)
	if err != nil {
		log.Printf("[Client WS] Connection failed: %v. Retrying in next cycle...", err)
		return
	}
	defer ws.Close()

	log.Println("[Client WS] Connected successfully! Waiting for control messages...")

	type WSControlMessage struct {
		Action   string `json:"action"`
		StreamID string `json:"stream_id"`
		Protocol string `json:"protocol"`
		Host     string `json:"host"`
	}

	for {
		var msg WSControlMessage
		err := websocket.JSON.Receive(ws, &msg)
		if err != nil {
			log.Printf("[Client WS] Control stream disconnected: %v", err)
			break
		}

		if msg.Action == "open_stream" {
			go c.handleWSStream(msg.StreamID, msg.Protocol, host, port, scheme)
		}
	}
}

func (c *TunnelClient) handleWSStream(streamID, protocol, host, port, scheme string) {
	logPrefix := fmt.Sprintf("WS-%s", streamID)
	log.Printf("[%s] Opening stream data connection...", logPrefix)

	dataURL := fmt.Sprintf("%s://%s:%s/api/tunnel/ws/data?stream_id=%s", scheme, host, port, streamID)
	config, err := websocket.NewConfig(dataURL, "http://"+host)
	if err != nil {
		log.Printf("[%s] Config error: %v", logPrefix, err)
		return
	}
	config.TlsConfig = &tls.Config{
		InsecureSkipVerify: true,
	}

	dataConn, err := websocket.DialConfig(config)
	if err != nil {
		log.Printf("[%s] Connection error: %v", logPrefix, err)
		return
	}
	defer dataConn.Close()

	if protocol == "http" {
		c.handleHTTPStream(dataConn, logPrefix)
	} else {
		c.handleTCPStream(dataConn, logPrefix)
	}
}

func (c *TunnelClient) runHandshake(conn *quic.Conn) error {
	stream, err := conn.OpenStreamSync(context.Background())
	if err != nil {
		return err
	}
	defer stream.Close()

	// Send handshake request
	req := common.HandshakeRequest{
		Token:     c.AuthToken,
		Subdomain: c.Subdomain,
	}
	if err := common.WriteJSON(stream, req); err != nil {
		return err
	}

	// Read handshake response
	var resp common.HandshakeResponse
	if err := common.ReadJSON(stream, &resp); err != nil {
		return err
	}

	if !resp.Success {
		return fmt.Errorf("server rejected handshake: %s", resp.Error)
	}

	if resp.Token != "" {
		log.Printf("[Client] Server assigned new session token: %s. Saving to ~/.goinstant/config.json", resp.Token)
		_ = common.SaveLocalToken(resp.Token)
	}

	return nil
}

func (c *TunnelClient) acceptStreams(conn *quic.Conn) {
	for {
		log.Println("[Client] Waiting for incoming stream from server...")
		stream, err := conn.AcceptStream(context.Background())
		if err != nil {
			log.Printf("[Client] Error accepting tunnel stream: %v", err)
			return
		}
		log.Printf("[Client] Accepted new stream from server (ID: %d)", stream.StreamID())
		go c.handleStream(stream)
	}
}

func (c *TunnelClient) handleStream(stream *quic.Stream) {
	defer func() {
		log.Printf("[Client Stream %d] Closing stream", stream.StreamID())
		stream.Close()
	}()

	log.Printf("[Client Stream %d] Reading stream header...", stream.StreamID())
	// Read header to determine protocol and routing
	var header common.StreamHeader
	if err := common.ReadJSON(stream, &header); err != nil {
		log.Printf("[Client Stream %d] Error reading stream header: %v", stream.StreamID(), err)
		return
	}
	log.Printf("[Client Stream %d] Read stream header successfully: %+v", stream.StreamID(), header)

	logPrefix := fmt.Sprintf("Stream %d", stream.StreamID())
	if header.Protocol == "http" {
		c.handleHTTPStream(stream, logPrefix)
	} else {
		// Generic TCP tunneling
		c.handleTCPStream(stream, logPrefix)
	}
}

func (c *TunnelClient) handleHTTPStream(stream io.ReadWriteCloser, logPrefix string) {
	log.Printf("[%s] Reading HTTP request...", logPrefix)
	reader := bufio.NewReader(stream)

	// Read the HTTP request from the stream
	req, err := http.ReadRequest(reader)
	if err != nil {
		log.Printf("[%s] Error reading HTTP request: %v", logPrefix, err)
		return
	}
	log.Printf("[%s] HTTP request read successfully: %s %s", logPrefix, req.Method, req.URL)

	// Capture request details for the inspector
	reqID := fmt.Sprintf("req-%d", time.Now().UnixNano())
	var reqBody []byte
	if req.Body != nil {
		reqBody, _ = io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewBuffer(reqBody))
	}

	reqLog := &RequestLog{
		ID:        reqID,
		Timestamp: time.Now(),
		Method:    req.Method,
		URL:       req.URL.String(),
		Headers:   req.Header,
		Body:      string(reqBody),
	}
	if c.Inspector != nil {
		c.Inspector.Log(reqLog)
	}

	// Check if this is a WebSocket request (upgrade)
	isUpgrade := strings.Contains(strings.ToLower(req.Header.Get("Connection")), "upgrade") ||
		strings.Contains(strings.ToLower(req.Header.Get("Upgrade")), "websocket")

	// Connect to local service
	localConn, err := net.Dial("tcp", c.TargetAddr)
	if err != nil {
		log.Printf("[Client] Failed to dial local service %s: %v", c.TargetAddr, err)
		// Send 502 Bad Gateway response back
		resp := &http.Response{
			StatusCode: http.StatusBadGateway,
			ProtoMajor: 1,
			ProtoMinor: 1,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("Failed to connect to local service")),
		}
		resp.Write(stream)
		
		// Update inspector
		reqLog.Response = &ResponseLog{
			Status:     resp.Status,
			StatusCode: resp.StatusCode,
			Body:       "Failed to connect to local service",
		}
		return
	}
	defer localConn.Close()

	if isUpgrade {
		log.Printf("[Client] Upgrading connection to WebSocket/TCP proxy for request %s", req.URL)
		// Write the raw initial request headers to the local service
		reqDump, err := httputil.DumpRequest(req, true)
		if err != nil {
			log.Printf("[Client] Failed to dump upgrade request: %v", err)
			return
		}
		localConn.Write(reqDump)

		// Direct TCP bridge with active teardown to prevent leaks
		errChan := make(chan error, 2)
		go func() {
			_, err := io.Copy(localConn, reader)
			localConn.Close()
			errChan <- err
		}()
		go func() {
			_, err := io.Copy(stream, localConn)
			if qs, ok := stream.(interface{ CancelRead(quic.StreamErrorCode) }); ok {
				qs.CancelRead(0)
			} else {
				stream.Close()
			}
			errChan <- err
		}()
		<-errChan
		return
	}

	// For standard HTTP: rewrite host and forward via local HTTP connection
	req.URL.Scheme = "http"
	req.URL.Host = c.TargetAddr
	req.RequestURI = "" // must be blank for client requests

	// Execute request to local application
	httpClient := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: 30 * time.Second,
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("[Client] HTTP request failed to local app: %v", err)
		respErr := &http.Response{
			StatusCode: http.StatusInternalServerError,
			ProtoMajor: 1,
			ProtoMinor: 1,
			Body:       io.NopCloser(strings.NewReader("Local server timeout or error")),
		}
		respErr.Write(stream)

		reqLog.Response = &ResponseLog{
			Status:     respErr.Status,
			StatusCode: respErr.StatusCode,
			Body:       "Local server timeout or error",
		}
		return
	}
	defer resp.Body.Close()

	// Read response body fully to log it
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body = io.NopCloser(bytes.NewBuffer(respBody))

	// Log response details
	reqLog.Response = &ResponseLog{
		Status:     resp.Status,
		StatusCode: resp.StatusCode,
		Headers:    resp.Header,
		Body:       string(respBody),
	}

	// Write response back to the tunnel stream
	err = resp.Write(stream)
	if err != nil {
		log.Printf("[Client] Failed to write response back to tunnel: %v", err)
	}
}

func (c *TunnelClient) handleTCPStream(stream io.ReadWriteCloser, logPrefix string) {
	localConn, err := net.Dial("tcp", c.TargetAddr)
	if err != nil {
		log.Printf("[Client] Failed to dial local service %s: %v", c.TargetAddr, err)
		return
	}
	defer localConn.Close()

	errChan := make(chan error, 2)
	go func() {
		_, err := io.Copy(localConn, stream)
		errChan <- err
	}()
	go func() {
		_, err := io.Copy(stream, localConn)
		errChan <- err
	}()
	<-errChan
}
