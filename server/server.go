package server

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"time"

	"go-online/common"

	"github.com/quic-go/quic-go"
)

// ClientSession represents an active client tunnel connection.
type ClientSession struct {
	Connection *quic.Conn
	Subdomain  string
	Token      string
}

type TunnelServer struct {
	Addr      string
	Domain    string
	AuthToken string
	Email     string
	HTTPPort  string
	HTTPSPort string

	clientsMu sync.RWMutex
	clients   map[string]*ClientSession

	// Dynamic TCP listeners for custom non-HTTP ports
	tcpListenersMu sync.Mutex
	tcpListeners   map[string]net.Listener
}

func NewTunnelServer(addr, domain, authToken, email, httpPort, httpsPort string) *TunnelServer {
	return &TunnelServer{
		Addr:         addr,
		Domain:       domain,
		AuthToken:    authToken,
		Email:        email,
		HTTPPort:     httpPort,
		HTTPSPort:    httpsPort,
		clients:      make(map[string]*ClientSession),
		tcpListeners: make(map[string]net.Listener),
	}
}

// Start runs the server's control plane (QUIC) and traffic listeners (HTTP/HTTPS).
func (s *TunnelServer) Start() error {
	// 1. Generate TLS Config for QUIC control plane
	quicTLSConfig, err := GenerateSelfSignedConfig()
	if err != nil {
		return fmt.Errorf("failed to generate QUIC TLS config: %w", err)
	}
	quicTLSConfig.NextProtos = []string{"mtm-protocol"}

	// 2. Start QUIC Listener
	listener, err := quic.ListenAddr(s.Addr, quicTLSConfig, &quic.Config{
		KeepAlivePeriod: 10 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("failed to listen on QUIC: %w", err)
	}
	log.Printf("[Server] Control plane listening on UDP %s (QUIC)", s.Addr)

	// 3. Start HTTP/HTTPS servers for traffic routing
	go s.startHTTPListeners()

	// 4. Handle incoming QUIC connections from clients
	for {
		conn, err := listener.Accept(context.Background())
		if err != nil {
			log.Printf("[Server] Error accepting QUIC connection: %v", err)
			continue
		}
		go s.handleClientConnection(conn)
	}
}

func (s *TunnelServer) handleClientConnection(conn *quic.Conn) {
	log.Printf("[Server] New incoming tunnel connection from %s", conn.RemoteAddr())

	// Accept the first stream as the control stream
	stream, err := conn.AcceptStream(context.Background())
	if err != nil {
		log.Printf("[Server] Failed to accept control stream: %v", err)
		conn.CloseWithError(1, "failed to accept control stream")
		return
	}
	defer stream.Close()

	// Read handshake
	var req common.HandshakeRequest
	if err := common.ReadJSON(stream, &req); err != nil {
		log.Printf("[Server] Handshake read failed: %v", err)
		return
	}

	// Validate Auth Token
	if req.Token != s.AuthToken {
		log.Printf("[Server] Unauthorized handshake attempt from %s", conn.RemoteAddr())
		common.WriteJSON(stream, common.HandshakeResponse{
			Success: false,
			Error:   "Invalid token",
		})
		conn.CloseWithError(2, "invalid token")
		return
	}

	// Validate Subdomain
	subdomain := strings.ToLower(strings.TrimSpace(req.Subdomain))
	if subdomain == "" {
		subdomain = fmt.Sprintf("client-%d", time.Now().UnixNano()%10000)
	}

	s.clientsMu.Lock()
	if oldSession, exists := s.clients[subdomain]; exists {
		log.Printf("[Server] Subdomain %s already registered. Disconnecting old session.", subdomain)
		oldSession.Connection.CloseWithError(3, "replaced by new session")
	}

	session := &ClientSession{
		Connection: conn,
		Subdomain:  subdomain,
		Token:      req.Token,
	}
	s.clients[subdomain] = session
	s.clientsMu.Unlock()

	log.Printf("[Server] Client successfully authenticated! Registered subdomain: %s.%s", subdomain, s.Domain)

	// Send success handshake response
	err = common.WriteJSON(stream, common.HandshakeResponse{
		Success: true,
	})
	if err != nil {
		log.Printf("[Server] Failed to send handshake response: %v", err)
		s.removeClient(subdomain)
		return
	}

	// Keep the session active by waiting on the connection context to close
	<-conn.Context().Done()
	log.Printf("[Server] Tunnel session for subdomain %s disconnected", subdomain)
	s.removeClient(subdomain)
}

func (s *TunnelServer) removeClient(subdomain string) {
	s.clientsMu.Lock()
	defer s.clientsMu.Unlock()
	delete(s.clients, subdomain)
}

func (s *TunnelServer) getClient(subdomain string) *ClientSession {
	s.clientsMu.RLock()
	defer s.clientsMu.RUnlock()
	return s.clients[subdomain]
}

// startHTTPListeners runs the port 80 and port 443 listeners.
func (s *TunnelServer) startHTTPListeners() {
	// Initialize CertMagic/TLS config
	tlsConfig, err := GetTLSConfig(s.Domain, s.Email)
	if err != nil {
		log.Fatalf("[Server] Failed to get TLS config: %v", err)
	}

	// Create reverse proxy logic
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("[Server HTTP] Received request: %s %s (Host: %s)", r.Method, r.URL.Path, r.Host)
		host := r.Host
		subdomain := s.extractSubdomain(host)

		if subdomain == "" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintln(w, "Bad Request: Missing subdomain")
			return
		}

		session := s.getClient(subdomain)
		if session == nil {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, "Tunnel %s.%s is not online.\n", subdomain, s.Domain)
			return
		}

		// Use a reverse proxy to forward the HTTP request over QUIC
		proxy := &httputil.ReverseProxy{
			Director: func(req *http.Request) {
				req.URL.Scheme = "http"
				req.URL.Host = r.Host
			},
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					log.Printf("[Server Proxy] DialContext called, opening QUIC stream to client...")
					// Open a new QUIC stream for this HTTP request
					stream, err := session.Connection.OpenStreamSync(ctx)
					if err != nil {
						log.Printf("[Server Proxy] Failed to open QUIC stream: %v", err)
						return nil, err
					}
					log.Printf("[Server Proxy] QUIC stream opened (ID: %d)", stream.StreamID())

					// Send header indicating HTTP protocol routing
					header := common.StreamHeader{
						Protocol: "http",
						Host:     r.Host,
					}
					log.Printf("[Server Proxy] Writing stream header to stream %d: %+v", stream.StreamID(), header)
					if err := common.WriteJSON(stream, header); err != nil {
						log.Printf("[Server Proxy] Failed to write stream header: %v", err)
						stream.Close()
						return nil, err
					}
					log.Printf("[Server Proxy] Stream header written successfully to stream %d", stream.StreamID())

					// Wrap the QUIC stream to satisfy net.Conn
					return &quicConnWrap{Stream: stream, conn: session.Connection}, nil
				},
			},
		}

		proxy.ServeHTTP(w, r)
	})

	// Start Port 80 (HTTP) redirect or proxy server
	go func() {
		addr := ":" + s.HTTPPort
		log.Printf("[Server] Traffic listener on port HTTP %s", addr)
		server := &http.Server{
			Addr:    addr,
			Handler: handler,
		}
		if err := server.ListenAndServe(); err != nil {
			log.Printf("[Server] HTTP listener error: %v", err)
		}
	}()

	// Start Port 443 (HTTPS) server
	addr := ":" + s.HTTPSPort
	log.Printf("[Server] Traffic listener on port HTTPS %s", addr)
	server := &http.Server{
		Addr:      addr,
		Handler:   handler,
		TLSConfig: tlsConfig,
	}
	if err := server.ListenAndServeTLS("", ""); err != nil {
		log.Fatalf("[Server] HTTPS listener error: %v", err)
	}
}

// extractSubdomain parses the subdomain from the hostname.
func (s *TunnelServer) extractSubdomain(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	host = strings.ToLower(host)
	domain := strings.ToLower(s.Domain)

	if host == domain || host == "localhost" || host == "127.0.0.1" {
		return ""
	}

	if strings.HasSuffix(host, "."+domain) {
		return strings.TrimSuffix(host, "."+domain)
	}

	// For localhost testing, accept any sub-part (e.g. app.localhost -> app)
	if s.Domain == "localhost" && strings.HasSuffix(host, ".localhost") {
		return strings.TrimSuffix(host, ".localhost")
	}

	return ""
}

// quicConnWrap wraps a quic.Stream to look like a net.Conn.
type quicConnWrap struct {
	*quic.Stream
	conn *quic.Conn
}

func (q *quicConnWrap) LocalAddr() net.Addr {
	return q.conn.LocalAddr()
}

func (q *quicConnWrap) RemoteAddr() net.Addr {
	return q.conn.RemoteAddr()
}

func (q *quicConnWrap) SetDeadline(t time.Time) error {
	return q.Stream.SetDeadline(t)
}

func (q *quicConnWrap) SetReadDeadline(t time.Time) error {
	return q.Stream.SetReadDeadline(t)
}

func (q *quicConnWrap) SetWriteDeadline(t time.Time) error {
	return q.Stream.SetWriteDeadline(t)
}
