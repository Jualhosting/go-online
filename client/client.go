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
			KeepAlivePeriod: 10 * time.Second,
		})
		if err != nil {
			log.Printf("[Client] Connection failed: %v. Retrying in 5 seconds...", err)
			time.Sleep(5 * time.Second)
			continue
		}

		log.Println("[Client] Connected to server! Performing handshake...")
		if err := c.runHandshake(conn); err != nil {
			log.Printf("[Client] Handshake failed: %v. Reconnecting in 5 seconds...", err)
			conn.CloseWithError(1, "handshake failed")
			time.Sleep(5 * time.Second)
			continue
		}

		log.Println("[Client] Handshake successful! Tunnel is open and ready.")
		
		// Accept streams representing incoming visitor connections
		c.acceptStreams(conn)
		log.Println("[Client] Connection closed. Reconnecting in 5 seconds...")
		time.Sleep(5 * time.Second)
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

	if header.Protocol == "http" {
		c.handleHTTPStream(stream)
	} else {
		// Generic TCP tunneling
		c.handleTCPStream(stream)
	}
}

func (c *TunnelClient) handleHTTPStream(stream *quic.Stream) {
	log.Printf("[Client Stream %d] Reading HTTP request...", stream.StreamID())
	reader := bufio.NewReader(stream)

	// Read the HTTP request from the stream
	req, err := http.ReadRequest(reader)
	if err != nil {
		log.Printf("[Client Stream %d] Error reading HTTP request: %v", stream.StreamID(), err)
		return
	}
	log.Printf("[Client Stream %d] HTTP request read successfully: %s %s", stream.StreamID(), req.Method, req.URL)

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
	isUpgrade := strings.ToLower(req.Header.Get("Connection")) == "upgrade" ||
		strings.ToLower(req.Header.Get("Upgrade")) == "websocket"

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

		// Direct TCP bridge
		errChan := make(chan error, 2)
		go func() {
			_, err := io.Copy(localConn, reader) // copy from stream buffer
			errChan <- err
		}()
		go func() {
			_, err := io.Copy(stream, localConn) // copy back to stream
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

func (c *TunnelClient) handleTCPStream(stream *quic.Stream) {
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
