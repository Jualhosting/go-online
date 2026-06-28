package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

type DebuggerServer struct {
	srv      *TunnelServer
	listener *chanListener
}

func NewDebuggerServer(srv *TunnelServer, listener *chanListener) *DebuggerServer {
	return &DebuggerServer{
		srv:      srv,
		listener: listener,
	}
}

func (ds *DebuggerServer) Start() {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/index.html" {
			http.NotFound(w, r)
			return
		}
		
		data, err := templatesFS.ReadFile("templates/debugger.html")
		if err != nil {
			data, err = os.ReadFile("server/templates/debugger.html")
			if err != nil {
				http.Error(w, "Debugger UI not found", http.StatusInternalServerError)
				return
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	})

	mux.HandleFunc("/api/logs", func(w http.ResponseWriter, r *http.Request) {
		subdomain, _ := r.Context().Value("subdomain").(string)
		if subdomain == "" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		w.Header().Set("X-Subdomain", subdomain)
		w.Header().Set("Content-Type", "application/json")

		if ds.srv.DBPath == "" {
			w.Write([]byte("[]"))
			return
		}

		rows, err := ds.srv.db.db.Query(
			"SELECT id, method, url, headers, body, received_at FROM webhook_logs WHERE subdomain = ? ORDER BY id DESC LIMIT 50",
			subdomain,
		)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		type DebugLog struct {
			ID         int64  `json:"id"`
			Method     string `json:"method"`
			URL        string `json:"url"`
			Headers    string `json:"headers"`
			Body       string `json:"body"`
			ReceivedAt string `json:"received_at"`
		}

		var logs []DebugLog
		for rows.Next() {
			var l DebugLog
			if err := rows.Scan(&l.ID, &l.Method, &l.URL, &l.Headers, &l.Body, &l.ReceivedAt); err == nil {
				logs = append(logs, l)
			}
		}

		if logs == nil {
			logs = []DebugLog{}
		}

		json.NewEncoder(w).Encode(logs)
	})

	mux.HandleFunc("/api/logs/sse", func(w http.ResponseWriter, r *http.Request) {
		subdomain, _ := r.Context().Value("subdomain").(string)
		if subdomain == "" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()

		var lastID int64
		_ = ds.srv.db.db.QueryRow("SELECT COALESCE(MAX(id), 0) FROM webhook_logs WHERE subdomain = ?", subdomain).Scan(&lastID)

		for {
			select {
			case <-ticker.C:
				rows, err := ds.srv.db.db.Query(
					"SELECT id FROM webhook_logs WHERE subdomain = ? AND id > ? ORDER BY id ASC",
					subdomain, lastID,
				)
				if err != nil {
					return
				}
				
				var hasNew bool
				for rows.Next() {
					var id int64
					if err := rows.Scan(&id); err == nil {
						hasNew = true
						if id > lastID {
							lastID = id
						}
					}
				}
				rows.Close()

				if hasNew {
					allRows, err := ds.srv.db.db.Query(
						"SELECT id, method, url, headers, body, received_at FROM webhook_logs WHERE subdomain = ? ORDER BY id DESC LIMIT 50",
						subdomain,
					)
					if err == nil {
						type DebugLog struct {
							ID         int64  `json:"id"`
							Method     string `json:"method"`
							URL        string `json:"url"`
							Headers    string `json:"headers"`
							Body       string `json:"body"`
							ReceivedAt string `json:"received_at"`
						}
						var logs []DebugLog
						for allRows.Next() {
							var l DebugLog
							if err := allRows.Scan(&l.ID, &l.Method, &l.URL, &l.Headers, &l.Body, &l.ReceivedAt); err == nil {
								logs = append(logs, l)
							}
						}
						allRows.Close()

						if logs == nil {
							logs = []DebugLog{}
						}

						data, _ := json.Marshal(logs)
						fmt.Fprintf(w, "data: %s\n\n", string(data))
						w.(http.Flusher).Flush()
					}
				}
			case <-r.Context().Done():
				return
			}
		}
	})

	mux.HandleFunc("/api/replay", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		subdomain, _ := r.Context().Value("subdomain").(string)
		if subdomain == "" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		idStr := r.URL.Query().Get("id")
		if idStr == "" {
			http.Error(w, "Missing log ID", http.StatusBadRequest)
			return
		}

		var method, uri, headersStr, bodyStr string
		err := ds.srv.db.db.QueryRow(
			"SELECT method, url, headers, body FROM webhook_logs WHERE id = ? AND subdomain = ?",
			idStr, subdomain,
		).Scan(&method, &uri, &headersStr, &bodyStr)
		if err != nil {
			http.Error(w, "Log not found", http.StatusNotFound)
			return
		}

		session := ds.srv.getClient(subdomain)
		if session == nil {
			http.Error(w, "Client offline", http.StatusServiceUnavailable)
			return
		}

		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			conn, err := session.Tunnel.OpenStream(ctx, "http", subdomain+"."+ds.srv.Domain)
			if err != nil {
				log.Printf("[Debugger Replay] Failed to open stream: %v", err)
				return
			}
			defer conn.Close()

			mockReq, err := http.NewRequest(method, uri, strings.NewReader(bodyStr))
			if err != nil {
				return
			}

			var headers map[string][]string
			if err := json.Unmarshal([]byte(headersStr), &headers); err == nil {
				for k, v := range headers {
					for _, val := range v {
						mockReq.Header.Add(k, val)
					}
				}
			}

			_ = mockReq.Write(conn)
		}()

		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"success":true}`))
	})

	server := &http.Server{
		Handler: mux,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if cw, ok := c.(*sshConnWrap); ok {
				return context.WithValue(ctx, "subdomain", cw.subdomain)
			}
			return ctx
		},
	}

	log.Println("[Debugger] Web debugger server started on internal listener")
	if err := server.Serve(ds.listener); err != nil && err != http.ErrServerClosed {
		log.Printf("[Debugger] Server stopped: %v", err)
	}
}
