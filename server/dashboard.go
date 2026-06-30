package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"embed"
	"encoding/hex"
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
	"sync"
	"time"

	"golang.org/x/net/websocket"
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
	mux.HandleFunc("/api/billing/upgrade", s.authAPI(s.handleAPIBillingUpgrade))
	mux.HandleFunc("/webhook/payment", s.handleWebhookPayment)
	mux.HandleFunc("/in.ps1", s.handleScriptPS1)
	mux.HandleFunc("/in.sh", s.handleScriptSH)
	mux.HandleFunc("/ex.ps1", s.handleScriptExposePS1)
	mux.HandleFunc("/ex.sh", s.handleScriptExposeSH)

	// WebSocket & SSH tunnel routing
	mux.Handle("/api/tunnel/ws/control", websocket.Handler(s.handleWSControl))
	mux.Handle("/api/tunnel/ws/data", websocket.Handler(s.handleWSData))
	mux.HandleFunc("/qr/", s.handleQRRedirect)
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

	// Replay request payload over Tunnel
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		conn, err := session.Tunnel.OpenStream(ctx, "http", subdomain+"."+s.Domain)
		if err != nil {
			log.Printf("[Webhook Replay] Failed to open stream: %v", err)
			return
		}
		defer conn.Close()

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

		// Write the request to conn
		_ = mockReq.Write(conn)
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

}

func isScannerPath(path string) bool {
	p := strings.ToLower(path)
	// Block common vulnerability scanners and exploit attempts at the Edge
	blockedPrefixes := []string{
		"/.env", "/.git", "/.svn", "/.hg", "/.vscode", "/.idea",
		"/wp-admin", "/wp-content", "/wp-includes", "/wp-json",
		"/ecp/", "/owa/", "/autodiscover",
		"/debug/default", "/trace.axd",
	}
	blockedSuffixes := []string{
		".env", ".git/config", "sftp.json", "wp-login.php", "xmlrpc.php",
		"config.php", "web.config", "database.yml", "secrets.yml",
	}

	for _, pref := range blockedPrefixes {
		if strings.HasPrefix(p, pref) || strings.Contains(p, pref+"/") {
			return true
		}
	}
	for _, suff := range blockedSuffixes {
		if strings.HasSuffix(p, suff) || strings.Contains(p, "/"+suff) {
			return true
		}
	}
	return false
}

func (s *TunnelServer) handleClientTraffic(w http.ResponseWriter, r *http.Request) {
	// Block vulnerability scanner bots at the Edge to protect client machine TCP socket limits
	if isScannerPath(r.URL.Path) {
		w.WriteHeader(http.StatusForbidden)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("Forbidden: Threat blocked by GoInstant Edge WAF"))
		return
	}

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
					return session.Tunnel.OpenStream(ctx, "http", r.Host)
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

func (s *TunnelServer) handleAPIBillingUpgrade(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID := s.getUserFromSession(r)
	if userID == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var req struct {
		ChannelCode string `json:"channel_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	apiKey := os.Getenv("PAYMENKU_API_KEY")
	if apiKey == "" {
		http.Error(w, "Paymenku API key is not configured in environment variables", http.StatusInternalServerError)
		return
	}

	referenceID := fmt.Sprintf("INV-%d", time.Now().UnixNano())
	amount := 50000.0 // Rp 50,000 for PRO plan

	paymenkuReq := map[string]interface{}{
		"channel_code":   req.ChannelCode,
		"amount":         amount,
		"reference_id":   referenceID,
		"customer_name":  userID,
		"customer_email": fmt.Sprintf("%s@goinstant.my.id", userID),
		"return_url":     "https://goinstant.my.id/dashboard",
	}

	payloadBytes, err := json.Marshal(paymenkuReq)
	if err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	httpReq, err := http.NewRequest("POST", "https://paymenku.com/api/v1/transaction/create", bytes.NewBuffer(payloadBytes))
	if err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		http.Error(w, "Failed to connect to payment gateway: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, "Failed to read gateway response", http.StatusInternalServerError)
		return
	}

	if resp.StatusCode != http.StatusOK {
		http.Error(w, fmt.Sprintf("Payment gateway error (status %d): %s", resp.StatusCode, string(respBytes)), http.StatusBadGateway)
		return
	}

	var paymenkuResp struct {
		Status string `json:"status"`
		Data   struct {
			TrxID       string      `json:"trx_id"`
			ReferenceID string      `json:"reference_id"`
			Amount      string      `json:"amount"`
			Status      string      `json:"status"`
			PayURL      string      `json:"pay_url"`
			PaymentInfo interface{} `json:"payment_info"`
		} `json:"data"`
	}

	if err := json.Unmarshal(respBytes, &paymenkuResp); err != nil {
		http.Error(w, "Failed to parse gateway response", http.StatusInternalServerError)
		return
	}

	paymentInfoBytes, _ := json.Marshal(paymenkuResp.Data.PaymentInfo)

	err = s.db.CreateBillingTransaction(
		userID,
		referenceID,
		paymenkuResp.Data.TrxID,
		amount,
		req.ChannelCode,
		"pending",
		string(paymentInfoBytes),
	)
	if err != nil {
		http.Error(w, "Failed to save transaction to database: "+err.Error(), http.StatusInternalServerError)
		return
	}

	_ = s.db.LogAuditEvent(userID, "billing.upgrade_started", fmt.Sprintf("Upgrade to PRO via %s started, Reference: %s", req.ChannelCode, referenceID))

	w.Header().Set("Content-Type", "application/json")
	w.Write(respBytes)
}

func (s *TunnelServer) handleWebhookPayment(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	signature := r.Header.Get("X-PaymenKu-Signature")
	timestamp := r.Header.Get("X-PaymenKu-Timestamp")

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	webhookSecret := os.Getenv("PAYMENKU_WEBHOOK_SECRET")
	if webhookSecret == "" {
		webhookSecret = "814f8477c0d0b615b06ea9879cfb0d99ef9f26b22a7038b65d71e1948843e8da"
	}

	mac := hmac.New(sha256.New, []byte(webhookSecret))
	mac.Write([]byte(timestamp + "." + string(bodyBytes)))
	expectedSignature := hex.EncodeToString(mac.Sum(nil))

	if signature != expectedSignature {
		log.Printf("[Webhook Payment] Signature mismatch. Got: %s, Expected: %s", signature, expectedSignature)
		http.Error(w, "Invalid signature", http.StatusUnauthorized)
		return
	}

	var payload struct {
		Event       string `json:"event"`
		TrxID       string `json:"trx_id"`
		ReferenceID string `json:"reference_id"`
		Status      string `json:"status"`
		Amount      string `json:"amount"`
	}

	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		http.Error(w, "Failed to parse body", http.StatusBadRequest)
		return
	}

	log.Printf("[Webhook Payment] Received event: %s for Trx: %s Reference: %s Status: %s", payload.Event, payload.TrxID, payload.ReferenceID, payload.Status)

	if payload.Status == "paid" {
		userID, err := s.db.UpdateBillingTransactionStatus(payload.ReferenceID, "paid")
		if err != nil {
			log.Printf("[Webhook Payment] Failed to update transaction status in DB: %v", err)
			http.Error(w, "Transaction not found", http.StatusNotFound)
			return
		}

		err = s.db.UpdateUserPlan(userID, "pro")
		if err != nil {
			log.Printf("[Webhook Payment] Failed to update user plan to PRO in DB: %v", err)
			http.Error(w, "Failed to upgrade user", http.StatusInternalServerError)
			return
		}

		_ = s.db.LogAuditEvent(userID, "billing.upgrade_completed", fmt.Sprintf("Upgrade to PRO via Paymenku completed. Trx: %s", payload.TrxID))
		log.Printf("[Webhook Payment] User %s successfully upgraded to PRO!", userID)
	} else {
		_, _ = s.db.UpdateBillingTransactionStatus(payload.ReferenceID, payload.Status)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"success"}`))
}

func (s *TunnelServer) handleScriptPS1(w http.ResponseWriter, r *http.Request) {
	sub := r.URL.Query().Get("sub")
	if sub == "" {
		sub = "my-site"
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	script := fmt.Sprintf(`$ProgressPreference = 'SilentlyContinue';
Write-Host "📥 Downloading GoInstant CLI client..." -ForegroundColor Cyan;
curl.exe -L https://raw.githubusercontent.com/Jualhosting/go-online/main/downloads/goinstant-windows.exe -o "$env:TEMP\goinstant.exe";
Write-Host "📦 Commencing static site deployment..." -ForegroundColor Green;
&$env:TEMP\goinstant.exe deploy -subdomain %s .`, sub)
	w.Write([]byte(script))
}

func (s *TunnelServer) handleScriptSH(w http.ResponseWriter, r *http.Request) {
	sub := r.URL.Query().Get("sub")
	if sub == "" {
		sub = "my-site"
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	script := fmt.Sprintf(`#!/bin/bash
OS_TYPE=$(uname -s | tr '[:upper:]' '[:lower:]')
if [ "$OS_TYPE" = "darwin" ]; then
    BINARY="goinstant-darwin"
else
    BINARY="goinstant-linux"
fi
echo "📥 Downloading GoInstant CLI client..."
curl -L "https://raw.githubusercontent.com/Jualhosting/go-online/main/downloads/$BINARY" -o /tmp/goinstant
chmod +x /tmp/goinstant
echo "📦 Commencing static site deployment..."
/tmp/goinstant deploy -subdomain %s .`, sub)
	w.Write([]byte(script))
}

func (s *TunnelServer) handleScriptExposePS1(w http.ResponseWriter, r *http.Request) {
	sub := r.URL.Query().Get("sub")
	if sub == "" {
		sub = "my-site"
	}
	port := r.URL.Query().Get("port")
	if port == "" {
		port = "8080"
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	script := fmt.Sprintf(`$ProgressPreference = 'SilentlyContinue';
Write-Host "📥 Downloading GoInstant CLI client..." -ForegroundColor Cyan;
curl.exe -L https://raw.githubusercontent.com/Jualhosting/go-online/main/downloads/goinstant-windows.exe -o "$env:TEMP\goinstant.exe";
Write-Host "🔌 Starting tunnel for port %s on subdomain %s..." -ForegroundColor Green;
&$env:TEMP\goinstant.exe expose -subdomain %s %s`, port, sub, sub, port)
	w.Write([]byte(script))
}

func (s *TunnelServer) handleScriptExposeSH(w http.ResponseWriter, r *http.Request) {
	sub := r.URL.Query().Get("sub")
	if sub == "" {
		sub = "my-site"
	}
	port := r.URL.Query().Get("port")
	if port == "" {
		port = "8080"
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	script := fmt.Sprintf(`#!/bin/bash
OS_TYPE=$(uname -s | tr '[:upper:]' '[:lower:]')
if [ "$OS_TYPE" = "darwin" ]; then
    BINARY="goinstant-darwin"
else
    BINARY="goinstant-linux"
fi
echo "📥 Downloading GoInstant CLI client..."
curl -L "https://raw.githubusercontent.com/Jualhosting/go-online/main/downloads/$BINARY" -o /tmp/goinstant
chmod +x /tmp/goinstant
echo "🔌 Starting tunnel for port %s on subdomain %s..."
/tmp/goinstant expose -subdomain %s %s`, port, sub, sub, port)
	w.Write([]byte(script))
}

type WSControlMessage struct {
	Action   string `json:"action"`
	StreamID string `json:"stream_id"`
	Protocol string `json:"protocol"`
	Host     string `json:"host"`
}

type WsTunnel struct {
	Subdomain string
	WriteMu   sync.Mutex
	Conn      *websocket.Conn
	Streams   sync.Map
}

func (w *WsTunnel) OpenStream(ctx context.Context, protocol, host string) (net.Conn, error) {
	streamID := fmt.Sprintf("str_%d", time.Now().UnixNano())
	ch := make(chan net.Conn, 1)
	w.Streams.Store(streamID, ch)
	defer w.Streams.Delete(streamID)

	msg := WSControlMessage{
		Action:   "open_stream",
		StreamID: streamID,
		Protocol: protocol,
		Host:     host,
	}

	w.WriteMu.Lock()
	err := websocket.JSON.Send(w.Conn, msg)
	w.WriteMu.Unlock()
	if err != nil {
		return nil, err
	}

	select {
	case conn := <-ch:
		return conn, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(5 * time.Second):
		return nil, fmt.Errorf("timeout waiting for data connection on stream %s", streamID)
	}
}

func (w *WsTunnel) Close() error {
	return w.Conn.Close()
}

type wsConnWrap struct {
	*websocket.Conn
	done chan struct{}
	once sync.Once
}

func (w *wsConnWrap) Close() error {
	var err error
	w.once.Do(func() {
		err = w.Conn.Close()
		close(w.done)
	})
	return err
}

func (s *TunnelServer) handleWSControl(ws *websocket.Conn) {
	r := ws.Request()
	subdomain := r.URL.Query().Get("subdomain")
	token := r.URL.Query().Get("token")

	log.Printf("[Server WS] Incoming control connection request for subdomain %s", subdomain)

	var userID string
	var assignedToken string
	if s.db != nil {
		var err error
		userID, err = s.db.ValidateUserToken(token)
		if err != nil {
			if token == s.AuthToken {
				userID = "user_syafri"
			} else {
				assignedToken = "tok_" + fmt.Sprintf("%d", time.Now().UnixNano())
				anonUserID := "usr_" + fmt.Sprintf("%d", time.Now().UnixNano())
				_, errUser := s.db.db.Exec("INSERT INTO users (id, email, plan_type, token, is_anonymous) VALUES (?, ?, ?, ?, 1)", anonUserID, anonUserID+"@anonymous.goinstant.my.id", "free", assignedToken)
				if errUser == nil {
					userID = anonUserID
					token = assignedToken
				} else {
					log.Printf("[Server WS] Failed to create anonymous user: %v", errUser)
					ws.Close()
					return
				}
			}
		}
	} else {
		if token != s.AuthToken {
			log.Printf("[Server WS] Unauthorized control handshake from %s", ws.RemoteAddr())
			ws.Close()
			return
		}
		userID = "user_syafri"
	}

	subdomain = strings.ToLower(strings.TrimSpace(subdomain))
	if subdomain == "" {
		ws.Close()
		return
	}

	_, found := s.routeCache.Load(subdomain)
	if !found {
		if s.db != nil {
			id, err := s.db.RegisterSubdomain(userID, subdomain, "tunnel", "")
			if err != nil {
				ws.Close()
				return
			}
			info := RouteInfo{
				SubdomainID: id,
				Subdomain:   subdomain,
				RoutingType: "tunnel",
				IsActive:    true,
			}
			s.routeCache.Store(subdomain, info)
		}
	} else {
		if s.db != nil {
			var ownerID string
			err := s.db.db.QueryRow("SELECT user_id FROM subdomains WHERE subdomain = ?", subdomain).Scan(&ownerID)
			if err == nil && ownerID != userID {
				log.Printf("[Server WS] Subdomain %s is owned by another user (%s)", subdomain, ownerID)
				ws.Close()
				return
			}
		}
	}

	wsTunnel := &WsTunnel{
		Subdomain: subdomain,
		Conn:      ws,
	}

	session := &ClientSession{
		Tunnel:    wsTunnel,
		Subdomain: subdomain,
		Token:     token,
		Type:      "websocket",
	}

	s.clientsMu.Lock()
	if oldSession, exists := s.clients[subdomain]; exists {
		log.Printf("[Server WS] Subdomain %s already registered. Disconnecting old session.", subdomain)
		oldSession.Tunnel.Close()
	}
	s.clients[subdomain] = session
	s.clientsMu.Unlock()

	log.Printf("[Server WS] Client authenticated! Registered subdomain: %s.%s (User: %s)", subdomain, s.Domain, userID)

	if s.db != nil {
		_ = s.db.LogAuditEvent(userID, "expose.ws", fmt.Sprintf("Opened WebSocket control tunnel for subdomain %s", subdomain))
	}

	buf := make([]byte, 1024)
	for {
		_, err := ws.Read(buf)
		if err != nil {
			break
		}
	}

	log.Printf("[Server WS] Control connection disconnected for subdomain %s", subdomain)
	s.removeClient(subdomain)
}

func (s *TunnelServer) handleWSData(ws *websocket.Conn) {
	r := ws.Request()
	streamID := r.URL.Query().Get("stream_id")

	log.Printf("[Server WS] Incoming data connection request for stream ID %s", streamID)

	s.clientsMu.RLock()
	var matchedTunnel *WsTunnel
	for _, c := range s.clients {
		if c.Type == "websocket" {
			if t, ok := c.Tunnel.(*WsTunnel); ok {
				if _, ok := t.Streams.Load(streamID); ok {
					matchedTunnel = t
					break
				}
			}
		}
	}
	s.clientsMu.RUnlock()

	if matchedTunnel == nil {
		log.Printf("[Server WS] No pending stream matched for ID %s", streamID)
		ws.Close()
		return
	}

	if val, ok := matchedTunnel.Streams.Load(streamID); ok {
		if ch, ok := val.(chan net.Conn); ok {
			done := make(chan struct{})
			connWrap := &wsConnWrap{
				Conn: ws,
				done: done,
			}
			ch <- connWrap
			<-done
		}
	}
}

func (s *TunnelServer) handleQRRedirect(w http.ResponseWriter, r *http.Request) {
	subdomain := strings.TrimPrefix(r.URL.Path, "/qr/")
	if subdomain == "" {
		http.Error(w, "Missing subdomain", http.StatusBadRequest)
		return
	}

	qrURL := fmt.Sprintf("https://api.qrserver.com/v1/create-qr-code/?size=250x250&data=https://%s.%s", subdomain, s.Domain)
	http.Redirect(w, r, qrURL, http.StatusFound)
}
