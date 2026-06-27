package server

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go-online/common"

	"github.com/caddyserver/certmagic"
	"github.com/quic-go/quic-go"
)

// ClientSession represents an active client tunnel connection.
type ClientSession struct {
	Connection *quic.Conn
	Subdomain  string
	Token      string
}

type RouteInfo struct {
	SubdomainID int64
	UserID      string
	Subdomain   string
	RoutingType string
	IsActive    bool
}

type TunnelServer struct {
	Addr      string
	Domain    string
	AuthToken string
	Email     string
	HTTPPort  string
	HTTPSPort string
	DBPath    string
	DeployDir string

	clientsMu sync.RWMutex
	clients   map[string]*ClientSession

	// Dynamic TCP listeners for custom non-HTTP ports
	tcpListenersMu sync.Mutex
	tcpListeners   map[string]net.Listener

	// Database and Cache
	db         *DBManager
	routeCache sync.Map
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
	// Initialize SQLite Database
	dbPath := s.DBPath
	if dbPath == "" {
		dbPath = "./config.db"
	}
	dbMgr, err := NewDBManager(dbPath)
	if err != nil {
		return fmt.Errorf("failed to initialize SQLite database: %w", err)
	}
	s.db = dbMgr

	// Load all domains from SQLite into routeCache
	if err := s.loadRoutesToCache(); err != nil {
		return fmt.Errorf("failed to populate route cache: %w", err)
	}

	// Setup global CertMagic domain validator
	DomainValidator = func(domainName string) bool {
		return s.IsValidDomainForTLS(domainName)
	}

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

	// Look up subdomain in cache
	val, found := s.routeCache.Load(subdomain)
	if !found {
		log.Printf("[Server] Handshake failed: Subdomain %s is not registered in the database.", subdomain)
		common.WriteJSON(stream, common.HandshakeResponse{
			Success: false,
			Error:   "Subdomain is not registered",
		})
		conn.CloseWithError(4, "subdomain not registered")
		return
	}
	info := val.(RouteInfo)
	if !info.IsActive {
		log.Printf("[Server] Handshake failed: Subdomain %s is inactive.", subdomain)
		common.WriteJSON(stream, common.HandshakeResponse{
			Success: false,
			Error:   "Subdomain is inactive",
		})
		conn.CloseWithError(5, "subdomain inactive")
		return
	}
	if info.RoutingType != "tunnel" {
		log.Printf("[Server] Handshake failed: Subdomain %s is configured for %s, not tunnel.", subdomain, info.RoutingType)
		common.WriteJSON(stream, common.HandshakeResponse{
			Success: false,
			Error:   "Subdomain is configured for static deployment",
		})
		conn.CloseWithError(6, "invalid routing type")
		return
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

		// Serve pre-compiled client binaries
		if strings.HasPrefix(r.URL.Path, "/downloads/") {
			_ = os.MkdirAll("./downloads", 0755)
			fs := http.StripPrefix("/downloads/", http.FileServer(http.Dir("./downloads")))
			fs.ServeHTTP(w, r)
			return
		}

		// Handle static deployment endpoint
		if r.URL.Path == "/api/deploy" && r.Method == http.MethodPost {
			s.handleDeploy(w, r)
			return
		}

		host := r.Host
		subdomain := s.extractSubdomain(host)
		lookupKey := host
		if subdomain != "" {
			lookupKey = subdomain
		}
		lookupKey = strings.ToLower(lookupKey)

		// Check routeCache
		val, found := s.routeCache.Load(lookupKey)
		if !found {
			// Fallback: if it's a subdomain and not in cache, return 404
			if subdomain != "" {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprintf(w, "Subdomain %s is not registered.\n", subdomain)
				return
			}
			// Root domain landing
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(LandingHTML))
			return
		}

		info := val.(RouteInfo)
		if !info.IsActive {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprintf(w, "Site %s is inactive.\n", lookupKey)
			return
		}

		subdomainPrefix := info.Subdomain

		if info.RoutingType == "tunnel" {
			session := s.getClient(subdomainPrefix)
			if session == nil {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprintf(w, "Tunnel %s.%s is not online.\n", subdomainPrefix, s.Domain)
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
			return
		} else if info.RoutingType == "static" {
			// Serve static files from local disk fallback
			deployDir := filepath.Join(s.getDeployDir(), subdomainPrefix)
			if info, err := os.Stat(deployDir); err == nil && info.IsDir() {
				fs := http.FileServer(http.Dir(deployDir))
				fs.ServeHTTP(w, r)
				return
			}

			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, "Static site %s has no active deployments.\n", subdomainPrefix)
			return
		}
	})

	// Start Port 80 (HTTP) redirect or proxy server
	go func() {
		addr := ":" + s.HTTPPort
		log.Printf("[Server] Traffic listener on port HTTP %s (Redirect to HTTPS)", addr)
		
		redirectHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host := r.Host
			if h, _, err := net.SplitHostPort(host); err == nil {
				host = h
			}
			
			// Bypass HTTPS redirect for local development/testing to avoid DNS lookup issues
			if host == "localhost" || host == "127.0.0.1" || strings.HasSuffix(host, ".localhost") {
				handler.ServeHTTP(w, r)
				return
			}

			target := "https://" + host + r.URL.Path
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			log.Printf("[Server HTTP] Redirecting http://%s%s to %s", r.Host, r.URL.Path, target)
			http.Redirect(w, r, target, http.StatusMovedPermanently)
		})

		server := &http.Server{
			Addr:    addr,
			Handler: certmagic.DefaultACME.HTTPChallengeHandler(redirectHandler),
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

func (s *TunnelServer) handleDeploy(w http.ResponseWriter, r *http.Request) {
	// Authenticate
	token := r.Header.Get("Authorization")
	expectedPrefix := "Bearer "
	if !strings.HasPrefix(token, expectedPrefix) || token[len(expectedPrefix):] != s.AuthToken {
		log.Printf("[Server Deploy] Unauthorized deploy attempt")
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	subdomain := r.Header.Get("X-Subdomain")
	if subdomain == "" {
		http.Error(w, "Subdomain header missing", http.StatusBadRequest)
		return
	}

	log.Printf("[Server Deploy] Receiving deployment for subdomain: %s", subdomain)

	// Read zip file from body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusInternalServerError)
		return
	}

	deployDir := filepath.Join(s.getDeployDir(), subdomain)
	// Clear old deployment
	os.RemoveAll(deployDir)
	if err := os.MkdirAll(deployDir, 0755); err != nil {
		http.Error(w, "Failed to create deploy directory", http.StatusInternalServerError)
		return
	}

	// Unzip the body
	zipReader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		http.Error(w, "Invalid zip archive", http.StatusBadRequest)
		return
	}

	for _, file := range zipReader.File {
		path := filepath.Join(deployDir, file.Name)
		// Prevent path traversal
		cleanDeployDir := filepath.Clean(deployDir)
		cleanPath := filepath.Clean(path)
		if !strings.HasPrefix(cleanPath, cleanDeployDir+string(filepath.Separator)) && cleanPath != cleanDeployDir {
			continue
		}

		if file.FileInfo().IsDir() {
			os.MkdirAll(path, 0755)
			continue
		}

		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			http.Error(w, "Failed to create directory structure", http.StatusInternalServerError)
			return
		}

		dstFile, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, file.Mode())
		if err != nil {
			http.Error(w, "Failed to open destination file", http.StatusInternalServerError)
			return
		}
		
		srcFile, err := file.Open()
		if err != nil {
			dstFile.Close()
			http.Error(w, "Failed to open zip file member", http.StatusInternalServerError)
			return
		}

		if _, err := io.Copy(dstFile, srcFile); err != nil {
			dstFile.Close()
			srcFile.Close()
			http.Error(w, "Failed to extract file contents", http.StatusInternalServerError)
			return
		}
		dstFile.Close()
		srcFile.Close()
	}

	log.Printf("[Server Deploy] Successfully deployed %s to %s", subdomain, deployDir)

	// Register/update subdomain in DB and cache
	if s.db != nil {
		subdomainID, err := s.db.GetSubdomainID(subdomain)
		if err != nil {
			// Register new static subdomain
			id, err := s.db.RegisterSubdomain("user_syafri", subdomain, "static", "")
			if err != nil {
				log.Printf("[Server Deploy] Failed to register subdomain: %v", err)
			}
			subdomainID = id
		} else {
			// Update to static routing
			_, err = s.db.RegisterSubdomain("user_syafri", subdomain, "static", "")
			if err != nil {
				log.Printf("[Server Deploy] Failed to update subdomain to static: %v", err)
			}
		}

		// Save deployment metadata
		versionStr := fmt.Sprintf("deploy-%d", time.Now().UnixNano())
		err = s.db.AddStaticDeployment(subdomainID, subdomain, versionStr)
		if err != nil {
			log.Printf("[Server Deploy] Failed to save deployment metadata: %v", err)
		}

		// Reload cache
		if err := s.loadRoutesToCache(); err != nil {
			log.Printf("[Server Deploy] Failed to reload routes to cache: %v", err)
		}
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Deployment successful!"))
}

func (s *TunnelServer) Close() {
	if s.db != nil {
		s.db.Close()
	}
}

func (s *TunnelServer) loadRoutesToCache() error {
	records, err := s.db.LoadAllSubdomains()
	if err != nil {
		return err
	}

	// Reset route cache
	s.routeCache.Range(func(key, value interface{}) bool {
		s.routeCache.Delete(key)
		return true
	})

	for _, rec := range records {
		info := RouteInfo{
			SubdomainID: rec.ID,
			UserID:      rec.UserID,
			Subdomain:   rec.Subdomain,
			RoutingType:  rec.RoutingType,
			IsActive:    rec.IsActive,
		}
		// Cache by subdomain
		s.routeCache.Store(strings.ToLower(rec.Subdomain), info)
		// Cache by custom domain if present
		if rec.CustomDomain.Valid && rec.CustomDomain.String != "" {
			s.routeCache.Store(strings.ToLower(rec.CustomDomain.String), info)
		}
	}
	log.Printf("[Server] Loaded %d subdomain configurations into cache.", len(records))
	return nil
}

func (s *TunnelServer) IsValidDomainForTLS(domainName string) bool {
	domainName = strings.ToLower(strings.TrimSpace(domainName))
	
	if domainName == strings.ToLower(s.Domain) {
		return true
	}

	subdomain := s.extractSubdomain(domainName)
	lookupKey := domainName
	if subdomain != "" {
		lookupKey = subdomain
	}

	val, found := s.routeCache.Load(lookupKey)
	if !found {
		return false
	}
	info := val.(RouteInfo)
	return info.IsActive
}

func (s *TunnelServer) getDeployDir() string {
	if s.DeployDir != "" {
		return s.DeployDir
	}
	return "./deployed"
}
