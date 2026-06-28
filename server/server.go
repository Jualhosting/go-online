package server

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go-online/common"

	"github.com/caddyserver/certmagic"
	"github.com/quic-go/quic-go"
)

// ClientTunnel represents a unified stream/tunnel driver.
type ClientTunnel interface {
	OpenStream(ctx context.Context, protocol, host string) (net.Conn, error)
	Close() error
}

type QuicTunnel struct {
	Connection *quic.Conn
}

func (t *QuicTunnel) OpenStream(ctx context.Context, protocol, host string) (net.Conn, error) {
	stream, err := t.Connection.OpenStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	header := common.StreamHeader{
		Protocol: protocol,
		Host:     host,
	}
	if err := common.WriteJSON(stream, header); err != nil {
		stream.Close()
		return nil, err
	}
	return &quicConnWrap{Stream: stream, conn: t.Connection}, nil
}

func (t *QuicTunnel) Close() error {
	t.Connection.CloseWithError(0, "closed")
	return nil
}

// ClientSession represents an active client tunnel connection.
type ClientSession struct {
	Tunnel    ClientTunnel
	Subdomain string
	Token     string
	Type      string // "quic", "websocket", "ssh"
}

type RouteInfo struct {
	SubdomainID int64
	UserID      string
	Subdomain   string
	RoutingType string
	IsActive    bool
}

type chanListener struct {
	addr net.Addr
	ch   chan net.Conn
	done chan struct{}
}

func newChanListener(addr net.Addr) *chanListener {
	return &chanListener{
		addr: addr,
		ch:   make(chan net.Conn, 100),
		done: make(chan struct{}),
	}
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.ch:
		return conn, nil
	case <-l.done:
		return nil, io.EOF
	}
}

func (l *chanListener) Close() error {
	close(l.done)
	return nil
}

func (l *chanListener) Addr() net.Addr {
	return l.addr
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

	// Database, Cache, and R2 Storage
	db         *DBManager
	routeCache sync.Map
	r2         *R2Manager

	// Multiplexing & SSH listeners
	httpsListener    *chanListener
	sshListener      *chanListener
	debuggerListener *chanListener
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

	// Initialize Cloudflare R2 Manager
	r2Mgr, err := NewR2Manager()
	if err != nil {
		return fmt.Errorf("failed to initialize Cloudflare R2 manager: %w", err)
	}
	s.r2 = r2Mgr

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
		KeepAlivePeriod: 5 * time.Second,
		MaxIdleTimeout:  30 * time.Second,
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
	rawToken := req.Token
	var userID string
	var assignedToken string
	if s.db != nil {
		var err error
		userID, err = s.db.ValidateUserToken(rawToken)
		if err != nil {
			if rawToken == s.AuthToken {
				userID = "user_syafri"
			} else {
				// Create new anonymous user
				assignedToken = "tok_" + fmt.Sprintf("%d", time.Now().UnixNano())
				anonUserID := "usr_" + fmt.Sprintf("%d", time.Now().UnixNano())
				_, errUser := s.db.db.Exec("INSERT INTO users (id, email, plan_type, token, is_anonymous) VALUES (?, ?, ?, ?, 1)", anonUserID, anonUserID+"@anonymous.goinstant.my.id", "free", assignedToken)
				if errUser == nil {
					userID = anonUserID
					rawToken = assignedToken
				} else {
					log.Printf("[Server] Failed to create anonymous user: %v", errUser)
					conn.CloseWithError(2, "failed to create user")
					return
				}
			}
		}
	} else {
		if rawToken != s.AuthToken {
			log.Printf("[Server] Unauthorized handshake attempt from %s", conn.RemoteAddr())
			common.WriteJSON(stream, common.HandshakeResponse{
				Success: false,
				Error:   "Invalid token",
			})
			conn.CloseWithError(2, "invalid token")
			return
		}
		userID = "user_syafri"
	}

	// Validate Subdomain
	subdomain := strings.ToLower(strings.TrimSpace(req.Subdomain))
	if subdomain == "" {
		subdomain = fmt.Sprintf("client-%d", time.Now().UnixNano()%10000)
	}

	// Look up subdomain in cache
	val, found := s.routeCache.Load(subdomain)
	if !found {
		if s.db != nil {
			id, err := s.db.RegisterSubdomain(userID, subdomain, "tunnel", "")
			if err != nil {
				log.Printf("[Server] Failed to auto-register subdomain: %v", err)
				common.WriteJSON(stream, common.HandshakeResponse{
					Success: false,
					Error:   "Failed to register subdomain",
				})
				conn.CloseWithError(4, "registration failed")
				return
			}
			log.Printf("[Server] Auto-registered subdomain: %s for user %s", subdomain, userID)
			info := RouteInfo{
				SubdomainID: id,
				Subdomain:   subdomain,
				RoutingType: "tunnel",
				IsActive:    true,
			}
			s.routeCache.Store(subdomain, info)
			val = info
		} else {
			log.Printf("[Server] Handshake failed: Subdomain %s is not registered in the database.", subdomain)
			common.WriteJSON(stream, common.HandshakeResponse{
				Success: false,
				Error:   "Subdomain is not registered",
			})
			conn.CloseWithError(4, "subdomain not registered")
			return
		}
	} else {
		// Verify ownership of the subdomain
		if s.db != nil {
			var ownerID string
			err = s.db.db.QueryRow("SELECT user_id FROM subdomains WHERE subdomain = ?", subdomain).Scan(&ownerID)
			if err == nil && ownerID != userID {
				log.Printf("[Server] Handshake failed: Subdomain %s is owned by another user (%s)", subdomain, ownerID)
				common.WriteJSON(stream, common.HandshakeResponse{
					Success: false,
					Error:   "Subdomain is already taken by another user",
				})
				conn.CloseWithError(7, "subdomain owned by another user")
				return
			}
		}
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
		oldSession.Tunnel.Close()
	}

	session := &ClientSession{
		Tunnel:    &QuicTunnel{Connection: conn},
		Subdomain: subdomain,
		Token:     req.Token,
		Type:      "quic",
	}
	s.clients[subdomain] = session
	s.clientsMu.Unlock()

	log.Printf("[Server] Client successfully authenticated! Registered subdomain: %s.%s (User: %s)", subdomain, s.Domain, userID)

	if s.db != nil {
		_ = s.db.LogAuditEvent(userID, "expose", fmt.Sprintf("Opened control plane tunnel for subdomain %s", subdomain))
	}

	// Send success handshake response
	err = common.WriteJSON(stream, common.HandshakeResponse{
		Success: true,
		Token:   assignedToken,
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
	tlsConfig.NextProtos = []string{"h2", "http/1.1", "acme-tls/1"}

	mux := http.NewServeMux()
	s.RegisterDashboardRoutes(mux)
	
	// Host-based router: ensure subdomains bypass console routes (like /login or /settings)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		subdomain := s.extractSubdomain(host)
		lookupKey := host
		if subdomain != "" {
			lookupKey = subdomain
		}
		lookupKey = strings.ToLower(lookupKey)

		// If it's a client subdomain, bypass all dashboard routes and forward directly to tunnel traffic
		if lookupKey != strings.ToLower(s.Domain) && lookupKey != "localhost" && lookupKey != "127.0.0.1" {
			s.handleClientTraffic(w, r)
			return
		}

		// Otherwise, serve standard developer console routes
		mux.ServeHTTP(w, r)
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

	// Start Port 443 (HTTPS) server with TCP multiplexing
	addr := ":" + s.HTTPSPort
	log.Printf("[Server] Traffic listener on port HTTPS %s (Multiplexed)", addr)

	rawLis, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("[Server] Failed to listen on TCP %s: %v", addr, err)
	}

	// Initialize multiplexed listeners
	s.httpsListener = newChanListener(rawLis.Addr())
	s.sshListener = newChanListener(rawLis.Addr())
	s.debuggerListener = newChanListener(rawLis.Addr())

	// Start SSH Server
	go s.StartSSHServer(s.sshListener)

	// Start Web Debugger Server
	dbgr := NewDebuggerServer(s, s.debuggerListener)
	go dbgr.Start()

	// Start HTTPS server
	server := &http.Server{
		Handler:   handler,
		TLSConfig: tlsConfig,
	}
	tlsListener := tls.NewListener(s.httpsListener, tlsConfig)
	go func() {
		if err := server.Serve(tlsListener); err != nil && err != http.ErrServerClosed {
			log.Printf("[Server] HTTPS listener error: %v", err)
		}
	}()

	// Multiplex incoming TCP connections
	for {
		conn, err := rawLis.Accept()
		if err != nil {
			log.Printf("[Server] Raw accept error: %v", err)
			break
		}
		go s.multiplexConn(conn)
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

func (q *quicConnWrap) Close() error {
	q.Stream.CancelRead(0)
	return q.Stream.Close()
}

func (s *TunnelServer) handleDeploy(w http.ResponseWriter, r *http.Request) {
	// Authenticate
	token := r.Header.Get("Authorization")
	expectedPrefix := "Bearer "
	rawToken := ""
	if strings.HasPrefix(token, expectedPrefix) {
		rawToken = token[len(expectedPrefix):]
	}

	var userID string
	var assignedToken string
	if s.db != nil {
		var err error
		userID, err = s.db.ValidateUserToken(rawToken)
		if err != nil {
			// Backwards compatibility fallback for tests
			if rawToken == s.AuthToken {
				userID = "user_syafri"
			} else {
				// Create new anonymous user
				assignedToken = "tok_" + fmt.Sprintf("%d", time.Now().UnixNano())
				anonUserID := "usr_" + fmt.Sprintf("%d", time.Now().UnixNano())
				_, errUser := s.db.db.Exec("INSERT INTO users (id, email, plan_type, token, is_anonymous) VALUES (?, ?, ?, ?, 1)", anonUserID, anonUserID+"@anonymous.goinstant.my.id", "free", assignedToken)
				if errUser == nil {
					userID = anonUserID
					rawToken = assignedToken
					w.Header().Set("X-GoInstant-Token", assignedToken)
				} else {
					log.Printf("[Server Deploy] Failed to create anonymous user: %v", errUser)
					http.Error(w, "Unauthorized", http.StatusUnauthorized)
					return
				}
			}
		}
	} else {
		if rawToken != s.AuthToken {
			log.Printf("[Server Deploy] Unauthorized deploy attempt")
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		userID = "user_syafri"
	}

	subdomain := r.Header.Get("X-Subdomain")
	if subdomain == "" {
		http.Error(w, "Subdomain header missing", http.StatusBadRequest)
		return
	}

	log.Printf("[Server Deploy] Receiving deployment for subdomain: %s (User: %s)", subdomain, userID)

	// Read zip file from body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusInternalServerError)
		return
	}

	// 1. Generate version ID and prefix folder name
	versionStr := fmt.Sprintf("deploy-%d", time.Now().UnixNano())
	r2Prefix := subdomain + "/" + versionStr

	var subdomainID int64
	if s.db != nil {
		id, err := s.db.GetSubdomainID(subdomain)
		if err != nil {
			// Subdomain is free, register it to this user
			id, err = s.db.RegisterSubdomain(userID, subdomain, "static", "")
			if err != nil {
				log.Printf("[Server Deploy] Failed to register subdomain: %v", err)
			}
			subdomainID = id
		} else {
			// Verify ownership
			var ownerID string
			err = s.db.db.QueryRow("SELECT user_id FROM subdomains WHERE subdomain = ?", subdomain).Scan(&ownerID)
			if err != nil || ownerID != userID {
				log.Printf("[Server Deploy] Subdomain already taken by another user")
				http.Error(w, "Subdomain already taken by another user", http.StatusForbidden)
				return
			}
			// Update to static routing
			_, err = s.db.RegisterSubdomain(userID, subdomain, "static", "")
			if err != nil {
				log.Printf("[Server Deploy] Failed to update subdomain to static: %v", err)
			}
			subdomainID = id
		}

		_ = s.db.LogAuditEvent(userID, "deploy", fmt.Sprintf("Uploaded static website deployment version %s for subdomain %s", versionStr, subdomain))
	}

	// Unzip the body
	zipReader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		http.Error(w, "Invalid zip archive", http.StatusBadRequest)
		return
	}

	// 2. Upload to Cloudflare R2 if enabled
	if s.r2 != nil && s.r2.IsEnabled() {
		log.Printf("[Server Deploy R2] Uploading zip contents to R2 prefix: %s...", r2Prefix)
		
		var wg sync.WaitGroup
		errChan := make(chan error, len(zipReader.File))
		sem := make(chan struct{}, 15) // Run up to 15 uploads concurrently

		for _, file := range zipReader.File {
			if file.FileInfo().IsDir() {
				continue
			}

			// Clean path name
			fileName := filepath.Clean(file.Name)
			if strings.HasPrefix(fileName, "..") || strings.Contains(fileName, "../") {
				continue
			}

			r2Key := r2Prefix + "/" + strings.ReplaceAll(file.Name, "\\", "/")
			
			wg.Add(1)
			go func(f *zip.File, k string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				srcFile, err := f.Open()
				if err != nil {
					errChan <- fmt.Errorf("failed to open %s: %w", f.Name, err)
					return
				}
				data, err := io.ReadAll(srcFile)
				srcFile.Close()
				if err != nil {
					errChan <- fmt.Errorf("failed to read %s: %w", f.Name, err)
					return
				}

				ext := filepath.Ext(f.Name)
				contentType := mime.TypeByExtension(ext)
				if contentType == "" {
					contentType = "application/octet-stream"
				}

				err = s.r2.UploadFile(r.Context(), k, data, contentType)
				if err != nil {
					errChan <- fmt.Errorf("failed to upload %s: %w", f.Name, err)
					return
				}
			}(file, r2Key)
		}

		wg.Wait()
		close(errChan)

		if len(errChan) > 0 {
			firstErr := <-errChan
			log.Printf("[Server Deploy R2] Upload failed: %v", firstErr)
			http.Error(w, fmt.Sprintf("Failed to upload files to R2: %v", firstErr), http.StatusInternalServerError)
			return
		}

		log.Printf("[Server Deploy R2] Successfully uploaded to R2 prefix: %s", r2Prefix)

	} else {
		// 3. Fallback: Save to local disk
		deployDir := filepath.Join(s.getDeployDir(), subdomain)
		os.RemoveAll(deployDir)
		if err := os.MkdirAll(deployDir, 0755); err != nil {
			http.Error(w, "Failed to create deploy directory", http.StatusInternalServerError)
			return
		}

		for _, file := range zipReader.File {
			path := filepath.Join(deployDir, file.Name)
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
		// For local fallback, we save the subdomain folder name as the R2 bucket folder key
		r2Prefix = subdomain
	}

	// 4. Save metadata to SQLite
	if s.db != nil {
		err = s.db.AddStaticDeployment(subdomainID, r2Prefix, versionStr)
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

type peekConn struct {
	net.Conn
	r io.Reader
}

func (c *peekConn) Read(b []byte) (int, error) {
	return c.r.Read(b)
}

func (s *TunnelServer) multiplexConn(conn net.Conn) {
	br := bufio.NewReader(conn)
	// Use 2 seconds read deadline to safely receive ClientHello over WAN latency
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	peek, err := br.Peek(1)
	conn.SetReadDeadline(time.Time{})

	isTLS := false
	if err == nil && len(peek) > 0 && peek[0] == 0x16 {
		isTLS = true
	}

	wrapped := &peekConn{
		Conn: conn,
		r:    br,
	}

	if isTLS {
		select {
		case s.httpsListener.ch <- wrapped:
		default:
			wrapped.Close()
		}
	} else {
		log.Printf("[Mux] Routing connection from %s to SSH (peek err: %v, peek bytes: %x)", conn.RemoteAddr(), err, peek)
		select {
		case s.sshListener.ch <- wrapped:
		default:
			wrapped.Close()
		}
	}
}
