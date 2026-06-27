package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

type ResponseLog struct {
	Status     string              `json:"status"`
	StatusCode int                 `json:"statusCode"`
	Headers    map[string][]string `json:"headers"`
	Body       string              `json:"body,omitempty"`
}

type RequestLog struct {
	ID        string              `json:"id"`
	Timestamp time.Time           `json:"timestamp"`
	Method    string              `json:"method"`
	URL       string              `json:"url"`
	Headers   map[string][]string `json:"headers"`
	Body      string              `json:"body,omitempty"`
	Response  *ResponseLog        `json:"response,omitempty"`
	RawReq    []byte              `json:"-"`
	TargetURL string              `json:"-"`
}

type InspectorServer struct {
	Port       string
	TargetAddr string
	logsMu     sync.RWMutex
	logs       []*RequestLog
}

func NewInspectorServer(port, targetAddr string) *InspectorServer {
	return &InspectorServer{
		Port:       port,
		TargetAddr: targetAddr,
		logs:       make([]*RequestLog, 0),
	}
}

func (i *InspectorServer) Start() {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/requests", i.handleGetRequests)
	mux.HandleFunc("/api/replay", i.handleReplayRequest)
	mux.HandleFunc("/", i.handleDashboard)

	addr := ":" + i.Port
	log.Printf("[Inspector] Dashboard running at http://localhost:%s", i.Port)
	go func() {
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Printf("[Inspector] Dashboard server error: %v", err)
		}
	}()
}

func (i *InspectorServer) Log(reqLog *RequestLog) {
	i.logsMu.Lock()
	defer i.logsMu.Unlock()
	// Limit storage to last 100 requests to avoid memory bloat
	if len(i.logs) >= 100 {
		i.logs = i.logs[1:]
	}
	i.logs = append(i.logs, reqLog)
}

func (i *InspectorServer) handleGetRequests(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	i.logsMu.RLock()
	defer i.logsMu.RUnlock()
	json.NewEncoder(w).Encode(i.logs)
}

func (i *InspectorServer) handleReplayRequest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "*")
	if r.Method == http.MethodOptions {
		return
	}

	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "Missing request ID", http.StatusBadRequest)
		return
	}

	i.logsMu.RLock()
	var targetLog *RequestLog
	for _, l := range i.logs {
		if l.ID == id {
			targetLog = l
			break
		}
	}
	i.logsMu.RUnlock()

	if targetLog == nil {
		http.Error(w, "Request not found", http.StatusNotFound)
		return
	}

	log.Printf("[Inspector] Replaying request %s %s to localhost local target", targetLog.Method, targetLog.URL)

	// Send HTTP request to local port
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(targetLog.Method, "http://"+i.TargetAddr+targetLog.URL, bytes.NewBuffer([]byte(targetLog.Body)))
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to create request: %v", err), http.StatusInternalServerError)
		return
	}

	// Copy headers
	for k, vv := range targetLog.Headers {
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to connect to local target: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	// Log the replayed response as a new item or update
	replayedLog := &RequestLog{
		ID:        fmt.Sprintf("replay-%d", time.Now().UnixNano()),
		Timestamp: time.Now(),
		Method:    targetLog.Method,
		URL:       targetLog.URL + " (REPLAY)",
		Headers:   targetLog.Headers,
		Body:      targetLog.Body,
		Response: &ResponseLog{
			Status:     resp.Status,
			StatusCode: resp.StatusCode,
			Headers:    resp.Header,
			Body:       string(respBody),
		},
	}
	i.Log(replayedLog)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(replayedLog)
}

func (i *InspectorServer) handleDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(dashboardHTML))
}

const dashboardHTML = `
<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <title>MTM - Tunnel Traffic Inspector</title>
    <link href="https://fonts.googleapis.com/css2?family=Plus+Jakarta+Sans:wght@400;500;600;700&family=JetBrains+Mono:wght@400;500&display=swap" rel="stylesheet">
    <style>
        :root {
            --bg-base: #0b0f19;
            --bg-surface: #151b2c;
            --bg-element: #1e263d;
            --text-main: #f3f4f6;
            --text-muted: #9ca3af;
            --primary: #6366f1;
            --primary-glow: rgba(99, 102, 241, 0.15);
            --success: #10b981;
            --error: #ef4444;
            --border: #273049;
        }
        * { box-sizing: border-box; margin: 0; padding: 0; }
        body {
            background-color: var(--bg-base);
            color: var(--text-main);
            font-family: 'Plus Jakarta Sans', sans-serif;
            display: flex;
            flex-direction: column;
            height: 100vh;
            overflow: hidden;
        }
        header {
            background-color: var(--bg-surface);
            border-bottom: 1px solid var(--border);
            padding: 16px 24px;
            display: flex;
            justify-content: space-between;
            align-items: center;
        }
        header h1 {
            font-size: 20px;
            font-weight: 700;
            background: linear-gradient(135deg, #a5b4fc, #6366f1);
            -webkit-background-clip: text;
            -webkit-text-fill-color: transparent;
            display: flex;
            align-items: center;
            gap: 10px;
        }
        header .badge {
            background-color: var(--primary-glow);
            color: #a5b4fc;
            padding: 4px 8px;
            border-radius: 6px;
            font-size: 12px;
            font-weight: 600;
            border: 1px solid rgba(99, 102, 241, 0.3);
        }
        .main-container {
            display: flex;
            flex: 1;
            overflow: hidden;
        }
        .sidebar {
            width: 40%;
            border-right: 1px solid var(--border);
            display: flex;
            flex-direction: column;
            background-color: var(--bg-base);
        }
        .sidebar-header {
            padding: 16px 24px;
            border-bottom: 1px solid var(--border);
            display: flex;
            justify-content: space-between;
            align-items: center;
            font-weight: 600;
            font-size: 14px;
            color: var(--text-muted);
        }
        .request-list {
            flex: 1;
            overflow-y: auto;
        }
        .request-item {
            padding: 16px 24px;
            border-bottom: 1px solid var(--border);
            cursor: pointer;
            transition: all 0.2s ease;
            display: flex;
            flex-direction: column;
            gap: 8px;
        }
        .request-item:hover {
            background-color: var(--bg-surface);
        }
        .request-item.active {
            background-color: var(--bg-surface);
            border-left: 4px solid var(--primary);
        }
        .request-meta {
            display: flex;
            align-items: center;
            gap: 8px;
        }
        .method {
            font-family: 'JetBrains Mono', monospace;
            font-weight: 700;
            font-size: 12px;
            padding: 2px 6px;
            border-radius: 4px;
            text-transform: uppercase;
        }
        .method.GET { background-color: rgba(16, 185, 129, 0.15); color: var(--success); }
        .method.POST { background-color: rgba(99, 102, 241, 0.15); color: #a5b4fc; }
        .method.PUT { background-color: rgba(245, 158, 11, 0.15); color: #f59e0b; }
        .method.DELETE { background-color: rgba(239, 68, 68, 0.15); color: var(--error); }
        
        .status {
            font-size: 12px;
            font-weight: 600;
        }
        .status.ok { color: var(--success); }
        .status.err { color: var(--error); }

        .url {
            font-family: 'JetBrains Mono', monospace;
            font-size: 13px;
            white-space: nowrap;
            overflow: hidden;
            text-overflow: ellipsis;
            color: var(--text-main);
        }
        .time {
            font-size: 11px;
            color: var(--text-muted);
            margin-left: auto;
        }

        .details-pane {
            width: 60%;
            padding: 24px;
            overflow-y: auto;
            background-color: var(--bg-surface);
            display: flex;
            flex-direction: column;
            gap: 24px;
        }
        .placeholder-text {
            color: var(--text-muted);
            text-align: center;
            margin-top: 100px;
            font-size: 16px;
        }
        .details-header {
            display: flex;
            justify-content: space-between;
            align-items: center;
            border-bottom: 1px solid var(--border);
            padding-bottom: 16px;
        }
        .details-title {
            display: flex;
            flex-direction: column;
            gap: 4px;
        }
        .details-title h2 {
            font-size: 18px;
            font-weight: 700;
        }
        .details-title p {
            font-family: 'JetBrains Mono', monospace;
            font-size: 14px;
            color: var(--text-muted);
        }
        .btn-replay {
            background-color: var(--primary);
            color: #fff;
            border: none;
            padding: 8px 16px;
            border-radius: 6px;
            font-weight: 600;
            font-size: 13px;
            cursor: pointer;
            display: flex;
            align-items: center;
            gap: 6px;
            transition: opacity 0.2s;
        }
        .btn-replay:hover {
            opacity: 0.9;
        }
        .section-title {
            font-size: 14px;
            font-weight: 600;
            color: #a5b4fc;
            margin-bottom: 8px;
            text-transform: uppercase;
            letter-spacing: 0.5px;
        }
        .code-block {
            background-color: var(--bg-base);
            border: 1px solid var(--border);
            border-radius: 8px;
            padding: 16px;
            font-family: 'JetBrains Mono', monospace;
            font-size: 13px;
            overflow-x: auto;
            white-space: pre-wrap;
            word-break: break-all;
            color: #cbd5e1;
        }
        .headers-table {
            width: 100%;
            border-collapse: collapse;
            margin-top: 8px;
        }
        .headers-table th, .headers-table td {
            padding: 8px 12px;
            border: 1px solid var(--border);
            text-align: left;
            font-size: 13px;
        }
        .headers-table th {
            background-color: var(--bg-element);
            color: var(--text-muted);
            font-weight: 600;
            width: 30%;
        }
        .headers-table td {
            font-family: 'JetBrains Mono', monospace;
        }
    </style>
</head>
<body>
    <header>
        <h1>Magic Tunnel Mesh (MTM) <span>🚀</span></h1>
        <div class="badge">Traffic Inspector (Port 4040)</div>
    </header>
    <div class="main-container">
        <div class="sidebar">
            <div class="sidebar-header">
                <span>Forwarded Requests</span>
                <span id="req-count">0 total</span>
            </div>
            <div class="request-list" id="req-list">
                <!-- Items dynamic -->
            </div>
        </div>
        <div class="details-pane" id="details-pane">
            <div class="placeholder-text">Select a request from the list to view headers, payload body, and responses.</div>
        </div>
    </div>

    <script>
        let requests = [];
        let selectedId = null;

        async function fetchRequests() {
            try {
                const res = await fetch('/api/requests');
                requests = await res.json();
                document.getElementById('req-count').innerText = requests.length + ' total';
                renderList();
                if (selectedId) {
                    renderDetails();
                }
            } catch (e) {
                console.error("Failed fetching request history", e);
            }
        }

        function renderList() {
            const list = document.getElementById('req-list');
            list.innerHTML = '';
            for (let i = requests.length - 1; i >= 0; i--) {
                const req = requests[i];
                const item = document.createElement('div');
                item.className = 'request-item' + (req.id === selectedId ? ' active' : '');
                
                const isOK = req.response && req.response.statusCode < 400;
                const statusClass = isOK ? 'ok' : 'err';
                const statusText = req.response ? req.response.statusCode : 'PENDING';

                item.innerHTML = '<div class="request-meta">' +
                    '<span class="method ' + req.method + '">' + req.method + '</span>' +
                    '<span class="status ' + statusClass + '">' + statusText + '</span>' +
                    '<span class="time">' + new Date(req.timestamp).toLocaleTimeString() + '</span>' +
                    '</div>' +
                    '<div class="url">' + req.url + '</div>';

                item.onclick = () => {
                    selectedId = req.id;
                    renderList();
                    renderDetails();
                };
                list.appendChild(item);
            }
        }

        async function replayRequest(id) {
            try {
                const btn = document.getElementById('btn-replay');
                btn.innerText = 'Replaying...';
                btn.disabled = true;
                const res = await fetch('/api/replay?id=' + id);
                const data = await res.json();
                btn.innerText = 'Replay Request ⚡';
                btn.disabled = false;
                await fetchRequests();
                selectedId = data.id; 
                renderDetails();
            } catch (e) {
                alert("Failed to replay request: " + e);
            }
        }

        function renderDetails() {
            const pane = document.getElementById('details-pane');
            const req = requests.find(r => r.id === selectedId);
            if (!req) {
                pane.innerHTML = '<div class="placeholder-text">Request details no longer available.</div>';
                return;
            }

            let headersRows = '';
            for (const [key, val] of Object.entries(req.headers)) {
                headersRows += '<tr><th>' + key + '</th><td>' + val.join(', ') + '</td></tr>';
            }

            let respHeadersRows = '';
            if (req.response) {
                for (const [key, val] of Object.entries(req.response.headers)) {
                    respHeadersRows += '<tr><th>' + key + '</th><td>' + val.join(', ') + '</td></tr>';
                }
            }

            const bodyContent = req.body ? escapeHtml(req.body) : '<i>[No request body payload]</i>';
            const respBodyContent = req.response ? (req.response.body ? escapeHtml(req.response.body) : '<i>[No response body payload]</i>') : '<i>[Response pending]</i>';

            pane.innerHTML = 
                '<div class="details-header">' +
                    '<div class="details-title">' +
                        '<h2>' + req.method + ' ' + req.url + '</h2>' +
                        '<p>Time: ' + new Date(req.timestamp).toLocaleString() + '</p>' +
                    '</div>' +
                    '<button class="btn-replay" id="btn-replay" onclick="replayRequest(\'' + req.id + '\')">Replay Request ⚡</button>' +
                '</div>' +
                
                '<div>' +
                    '<h3 class="section-title">Request Headers</h3>' +
                    '<table class="headers-table">' +
                        headersRows +
                    '</table>' +
                '</div>' +

                '<div>' +
                    '<h3 class="section-title">Request Body</h3>' +
                    '<div class="code-block">' + bodyContent + '</div>' +
                '</div>' +

                '<div style="border-top: 1px solid var(--border); padding-top: 24px;">' +
                    '<h3 class="section-title" style="color: var(--success)">Response - ' + (req.response ? req.response.status : 'PENDING') + '</h3>' +
                    (req.response ? 
                        ('<table class="headers-table" style="margin-bottom: 16px;">' +
                            respHeadersRows +
                        '</table>' +
                        '<h4 class="section-title">Response Body</h4>' +
                        '<div class="code-block">' + respBodyContent + '</div>')
                     : '<p>Waiting for response from localhost...</p>') +
                '</div>';
        }

        function escapeHtml(text) {
            return text
                .replace(/&/g, "&amp;")
                .replace(/&lt;/g, "&lt;")
                .replace(/&gt;/g, "&gt;")
                .replace(/"/g, "&quot;")
                .replace(/'/g, "&#039;");
        }

        // Poll every 1 second
        setInterval(fetchRequests, 1000);
        fetchRequests();
    </script>
</body>
</html>
`
