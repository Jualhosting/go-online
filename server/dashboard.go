package server

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go-online/common"
)

//go:embed templates/*
var templatesFS embed.FS

func (s *TunnelServer) RegisterDashboardRoutes(mux *http.ServeMux) {
	// Web Console HTML template endpoints
	mux.HandleFunc("/", s.handleLandingRoute)
	mux.HandleFunc("/login", s.serveTemplate("login.html"))
	mux.HandleFunc("/dashboard", s.serveTemplate("dashboard.html"))
	mux.HandleFunc("/domains", s.serveTemplate("domains.html"))
	mux.HandleFunc("/webhooks", s.serveTemplate("webhooks.html"))
	mux.HandleFunc("/files", s.serveTemplate("files.html"))
	mux.HandleFunc("/settings", s.serveTemplate("settings.html"))
	mux.HandleFunc("/audit", s.serveTemplate("audit.html"))
	mux.HandleFunc("/analytics", s.serveTemplate("analytics.html"))
	mux.HandleFunc("/status", s.serveTemplate("status.html"))
	mux.HandleFunc("/docs", s.serveTemplate("docs.html"))

	// Authentication API
	mux.HandleFunc("/api/auth/login", s.handleAPILogin)
	mux.HandleFunc("/api/auth/logout", s.handleAPILogout)

	// Developer Console REST APIs
	mux.HandleFunc("/api/dashboard", s.authAPI(s.handleAPIDashboard))
	mux.HandleFunc("/api/domains", s.authAPI(s.handleAPIDomains))
	mux.HandleFunc("/api/domains/add", s.authAPI(s.handleAPIDomainsAdd))
	mux.HandleFunc("/api/webhooks", s.authAPI(s.handleAPIWebhooks))
	mux.HandleFunc("/api/webhooks/replay", s.authAPI(s.handleAPIWebhooksReplay))
	mux.HandleFunc("/api/files", s.authAPI(s.handleAPIFiles))
	mux.HandleFunc("/api/audit", s.authAPI(s.handleAPIAudit))
	mux.HandleFunc("/api/analytics", s.authAPI(s.handleAPIAnalytics))
}

func (s *TunnelServer) handleLandingRoute(w http.ResponseWriter, r *http.Request) {
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

	// Route public site visitors
	host := r.Host
	subdomain := s.extractSubdomain(host)
	lookupKey := host
	if subdomain != "" {
		lookupKey = subdomain
	}
	lookupKey = strings.ToLower(lookupKey)

	// If visitor requested root domain, serve landing page
	if lookupKey == strings.ToLower(s.Domain) || lookupKey == "localhost" || lookupKey == "127.0.0.1" {
		if r.URL.Path != "/" {
			// Serve templates or 404
			http.NotFound(w, r)
			return
		}
		s.serveTemplate("landing.html")(w, r)
		return
	}

	// Forward client site traffic (from HTTP proxy logic)
	s.handleClientTraffic(w, r)
}

func (s *TunnelServer) serveTemplate(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Protected console routes redirect to login
		if name != "landing.html" && name != "login.html" && name != "status.html" && name != "docs.html" {
			if !s.isAuthenticated(r) {
				http.Redirect(w, r, "/login", http.StatusFound)
				return
			}
		}

		data, err := templatesFS.ReadFile("templates/" + name)
		if err != nil {
			http.Error(w, "Template not found: "+name, http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
	}
}

// Authentication Helpers
func (s *TunnelServer) isAuthenticated(r *http.Request) bool {
	cookie, err := r.Cookie("goinstant_session")
	if err != nil {
		return false
	}

	if s.db == nil {
		return cookie.Value == s.AuthToken
	}

	userID, err := s.db.ValidateUserToken(cookie.Value)
	return err == nil && userID != ""
}

func (s *TunnelServer) authAPI(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.isAuthenticated(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "Unauthorized"})
			return
		}
		next(w, r)
	}
}

func (s *TunnelServer) getUserFromSession(r *http.Request) string {
	cookie, err := r.Cookie("goinstant_session")
	if err != nil {
		return "user_syafri"
	}
	if s.db == nil {
		return "user_syafri"
	}
	userID, err := s.db.ValidateUserToken(cookie.Value)
	if err != nil {
		return "user_syafri"
	}
	return userID
}

// Authentication API handlers
func (s *TunnelServer) handleAPILogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "Invalid request body"})
		return
	}

	token := strings.TrimSpace(req.Token)
	if token == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "Token is required"})
		return
	}

	valid := false
	if s.db == nil {
		valid = (token == s.AuthToken)
	} else {
		userID, err := s.db.ValidateUserToken(token)
		valid = (err == nil && userID != "")
	}

	if !valid {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "Invalid token"})
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "goinstant_session",
		Value:    token,
		Path:     "/",
		Expires:  time.Now().Add(24 * time.Hour),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	if s.db != nil {
		userID, _ := s.db.ValidateUserToken(token)
		_ = s.db.LogAuditEvent(userID, "login", "User successfully authenticated web console session.")
	}

	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

func (s *TunnelServer) handleAPILogout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	http.SetCookie(w, &http.Cookie{
		Name:     "goinstant_session",
		Value:    "",
		Path:     "/",
		Expires:  time.Now().Add(-1 * time.Hour),
		HttpOnly: true,
	})
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// Console API Implementation
func (s *TunnelServer) handleAPIDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	userID := s.getUserFromSession(r)

	// Fetch summaries from SQLite
	totalTunnels := 0
	totalDeploys := 0
	var bytesTransferred int64 = 0

	if s.db != nil {
		_ = s.db.db.QueryRow("SELECT COUNT(*) FROM subdomains WHERE user_id = ? AND routing_type = 'tunnel'", userID).Scan(&totalTunnels)
		_ = s.db.db.QueryRow("SELECT COUNT(*) FROM subdomains WHERE user_id = ? AND routing_type = 'static'", userID).Scan(&totalDeploys)
		_ = s.db.db.QueryRow("SELECT COALESCE(SUM(bytes_sent), 0) FROM traffic_stats").Scan(&bytesTransferred)
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":           true,
		"email":             userID + "@goinstant.my.id",
		"plan":              "pro",
		"total_tunnels":     totalTunnels,
		"total_deploys":     totalDeploys,
		"bytes_transferred": bytesTransferred,
	})
}

func (s *TunnelServer) handleAPIDomains(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	userID := s.getUserFromSession(r)

	type DomainItem struct {
		Subdomain    string `json:"subdomain"`
		CustomDomain string `json:"custom_domain"`
		RoutingType  string `json:"routing_type"`
		IsActive     bool   `json:"is_active"`
	}

	var items []DomainItem
	if s.db != nil {
		rows, err := s.db.db.Query("SELECT subdomain, COALESCE(custom_domain, ''), routing_type, is_active FROM subdomains WHERE user_id = ?", userID)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var item DomainItem
				var active int
				if err := rows.Scan(&item.Subdomain, &item.CustomDomain, &item.RoutingType, &active); err == nil {
					item.IsActive = (active == 1)
					items = append(items, item)
				}
			}
		}
	}

	json.NewEncoder(w).Encode(items)
}

func (s *TunnelServer) handleAPIDomainsAdd(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Subdomain    string `json:"subdomain"`
		CustomDomain string `json:"custom_domain"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	userID := s.getUserFromSession(r)
	if s.db != nil {
		// Verify ownership
		var ownerID string
		err := s.db.db.QueryRow("SELECT user_id FROM subdomains WHERE subdomain = ?", req.Subdomain).Scan(&ownerID)
		if err != nil || ownerID != userID {
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "Subdomain is not owned by you"})
			return
		}

		// Bind custom domain
		_, err = s.db.db.Exec("UPDATE subdomains SET custom_domain = ? WHERE subdomain = ?", req.CustomDomain, req.Subdomain)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": err.Error()})
			return
		}

		_ = s.db.LogAuditEvent(userID, "domain_update", fmt.Sprintf("Bound custom domain %s to subdomain %s", req.CustomDomain, req.Subdomain))
		_ = s.loadRoutesToCache()
	}

	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

func (s *TunnelServer) handleAPIWebhooks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	type WebhookLog struct {
		ID         int    `json:"id"`
		Subdomain  string `json:"subdomain"`
		Method     string `json:"method"`
		URL        string `json:"url"`
		Headers    string `json:"headers"`
		Body       string `json:"body"`
		ReceivedAt string `json:"received_at"`
	}

	var logs []WebhookLog
	if s.db != nil {
		rows, err := s.db.db.Query("SELECT id, subdomain, method, url, headers, body, received_at FROM webhook_logs ORDER BY id DESC LIMIT 50")
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var wl WebhookLog
				if err := rows.Scan(&wl.ID, &wl.Subdomain, &wl.Method, &wl.URL, &wl.Headers, &wl.Body, &wl.ReceivedAt); err == nil {
					logs = append(logs, wl)
				}
			}
		}
	}

	json.NewEncoder(w).Encode(logs)
}

func (s *TunnelServer) handleAPIWebhooksReplay(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ID int `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if s.db == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "Database not initialized"})
		return
	}

	var subdomain, method, uri, headersStr, bodyStr string
	err := s.db.db.QueryRow("SELECT subdomain, method, url, headers, body FROM webhook_logs WHERE id = ?", req.ID).Scan(&subdomain, &method, &uri, &headersStr, &bodyStr)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "Log not found"})
		return
	}

	// Route to active tunnel session
	session := s.getClient(subdomain)
	if session == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "Tunnel client is currently offline"})
		return
	}

	// Replay request payload over QUIC
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		stream, err := session.Connection.OpenStreamSync(ctx)
		if err != nil {
			log.Printf("[Webhook Replay] Failed to open stream: %v", err)
			return
		}
		defer stream.Close()

		// Write protocol header
		header := common.StreamHeader{
			Protocol: "http",
			Host:     subdomain + "." + s.Domain,
		}
		_ = common.WriteJSON(stream, header)

		// Create mock HTTP request
		mockReq, err := http.NewRequest(method, uri, strings.NewReader(bodyStr))
		if err != nil {
			return
		}

		// Re-inflate headers
		var headers map[string][]string
		if err := json.Unmarshal([]byte(headersStr), &headers); err == nil {
			for k, v := range headers {
				for _, val := range v {
					mockReq.Header.Add(k, val)
				}
			}
		}

		// Write the request to stream
		_ = mockReq.Write(stream)
	}()

	userID := s.getUserFromSession(r)
	_ = s.db.LogAuditEvent(userID, "webhook_replay", fmt.Sprintf("Replayed webhook payload ID %d to tunnel client %s", req.ID, subdomain))

	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

func (s *TunnelServer) handleAPIFiles(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	userID := s.getUserFromSession(r)

	type FileDeployItem struct {
		ID         int    `json:"id"`
		Subdomain  string `json:"subdomain"`
		Folder     string `json:"r2_bucket_folder"`
		Version    string `json:"version_output"`
		DeployedAt string `json:"deployed_at"`
	}

	var deploys []FileDeployItem
	if s.db != nil {
		rows, err := s.db.db.Query("SELECT sd.id, s.subdomain, sd.r2_bucket_folder, sd.version_output, sd.deployed_at FROM static_deploys sd JOIN subdomains s ON sd.subdomain_id = s.id WHERE s.user_id = ? ORDER BY sd.id DESC", userID)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var item FileDeployItem
				if err := rows.Scan(&item.ID, &item.Subdomain, &item.Folder, &item.Version, &item.DeployedAt); err == nil {
					deploys = append(deploys, item)
				}
			}
		}
	}

	json.NewEncoder(w).Encode(deploys)
}

func (s *TunnelServer) handleAPIAudit(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	userID := s.getUserFromSession(r)

	type AuditLog struct {
		ID       int    `json:"id"`
		Type     string `json:"event_type"`
		Details  string `json:"details"`
		LoggedAt string `json:"logged_at"`
	}

	var logs []AuditLog
	if s.db != nil {
		rows, err := s.db.db.Query("SELECT id, event_type, details, logged_at FROM audit_events WHERE user_id = ? ORDER BY id DESC LIMIT 50", userID)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var logItem AuditLog
				if err := rows.Scan(&logItem.ID, &logItem.Type, &logItem.Details, &logItem.LoggedAt); err == nil {
					logs = append(logs, logItem)
				}
			}
		}
	}

	json.NewEncoder(w).Encode(logs)
}

func (s *TunnelServer) handleAPIAnalytics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	type AnalyticsDataPoint struct {
		Time      string `json:"time"`
		Bandwidth int64  `json:"bandwidth"`
		Requests  int    `json:"requests"`
	}

	// Mock graph data for charts rendering matching traffic_latency_analytics
	var points []AnalyticsDataPoint
	now := time.Now()
	for i := 6; i >= 0; i-- {
		t := now.Add(-time.Duration(i) * time.Hour)
		points = append(points, AnalyticsDataPoint{
			Time:      t.Format("15:04"),
			Bandwidth: int64(1024*1024*12*(7-i) + 512*1024),
			Requests:  12*(7-i) + 24,
		})
	}

	json.NewEncoder(w).Encode(points)
}

func (s *TunnelServer) handleClientTraffic(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	subdomain := s.extractSubdomain(host)
	lookupKey := host
	if subdomain != "" {
		lookupKey = subdomain
	}
	lookupKey = strings.ToLower(lookupKey)

	// Fetch route from cache
	val, found := s.routeCache.Load(lookupKey)
	if !found {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	info := val.(RouteInfo)
	if !info.IsActive {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	// Intercept/Proxy static or tunnel
	if info.RoutingType == "tunnel" {
		session := s.getClient(info.Subdomain)
		if session == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		// Proxy HTTP request and record payload for Webhook Inspector
		bodyBytes, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))

		// Log request headers & payload to DB
		if s.db != nil {
			headersJSON, _ := json.Marshal(r.Header)
			_ = s.db.LogWebhookRequest(info.Subdomain, r.Method, r.URL.Path, string(headersJSON), string(bodyBytes))
			_ = s.db.LogTraffic(info.Subdomain, int64(len(bodyBytes)))
		}

		proxy := &httputil.ReverseProxy{
			Director: func(req *http.Request) {
				req.URL.Scheme = "http"
				req.URL.Host = r.Host
			},
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					stream, err := session.Connection.OpenStreamSync(ctx)
					if err != nil {
						return nil, err
					}
					header := common.StreamHeader{
						Protocol: "http",
						Host:     r.Host,
					}
					if err := common.WriteJSON(stream, header); err != nil {
						stream.Close()
						return nil, err
					}
					return &quicConnWrap{Stream: stream, conn: session.Connection}, nil
				},
			},
		}
		proxy.ServeHTTP(w, r)
	} else if info.RoutingType == "static" {
		if s.r2 != nil && s.r2.IsEnabled() {
			folder, err := s.db.GetLatestDeploymentFolder(info.SubdomainID)
			if err != nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			cleanPath := filepath.Clean(r.URL.Path)
			if strings.HasSuffix(r.URL.Path, "/") || cleanPath == "." || cleanPath == "/" {
				cleanPath = "index.html"
			}
			cleanPath = strings.TrimPrefix(cleanPath, "/")

			r2Key := folder + "/" + cleanPath
			body, contentType, err := s.r2.DownloadFile(r.Context(), r2Key)
			if err != nil {
				fallbackKey := folder + "/index.html"
				body, contentType, err = s.r2.DownloadFile(r.Context(), fallbackKey)
				if err != nil {
					w.WriteHeader(http.StatusNotFound)
					return
				}
			}
			defer body.Close()

			if contentType != "" {
				w.Header().Set("Content-Type", contentType)
			}
			w.WriteHeader(http.StatusOK)
			written, _ := io.Copy(w, body)

			if s.db != nil {
				_ = s.db.LogTraffic(info.Subdomain, written)
			}
		} else {
			deployDir := filepath.Join(s.getDeployDir(), info.Subdomain)
			if infoStat, err := os.Stat(deployDir); err == nil && infoStat.IsDir() {
				fs := http.FileServer(http.Dir(deployDir))
				fs.ServeHTTP(w, r)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}
	}
}
