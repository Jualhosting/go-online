package server

const LandingHTML = `
<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Magic Tunnel Mesh (MTM) - Zero-Trust Tunnel khusus IoT & Server Rumah</title>
    <link href="https://fonts.googleapis.com/css2?family=Plus+Jakarta+Sans:wght@400;500;600;700;800&family=JetBrains+Mono:wght@400;500&display=swap" rel="stylesheet">
    <style>
        :root {
            --bg-base: #060913;
            --bg-surface: #0e1326;
            --bg-element: #161e38;
            --text-main: #f3f4f6;
            --text-muted: #9ca3af;
            --primary: #6366f1;
            --primary-glow: rgba(99, 102, 241, 0.15);
            --success: #10b981;
            --border: #1f294d;
            --grad-accent: linear-gradient(135deg, #a5b4fc, #6366f1, #4f46e5);
        }
        * { box-sizing: border-box; margin: 0; padding: 0; }
        body {
            background-color: var(--bg-base);
            color: var(--text-main);
            font-family: 'Plus Jakarta Sans', sans-serif;
            line-height: 1.6;
            overflow-x: hidden;
        }
        header {
            border-bottom: 1px solid var(--border);
            background-color: rgba(14, 19, 38, 0.7);
            backdrop-filter: blur(12px);
            position: sticky;
            top: 0;
            z-index: 100;
            padding: 20px 40px;
            display: flex;
            justify-content: space-between;
            align-items: center;
        }
        header h1 {
            font-size: 22px;
            font-weight: 800;
            background: var(--grad-accent);
            -webkit-background-clip: text;
            -webkit-text-fill-color: transparent;
            display: flex;
            align-items: center;
            gap: 10px;
        }
        header nav a {
            color: var(--text-muted);
            text-decoration: none;
            margin-left: 24px;
            font-weight: 500;
            font-size: 14px;
            transition: color 0.2s;
        }
        header nav a:hover {
            color: var(--text-main);
        }
        .hero {
            padding: 100px 24px 80px;
            text-align: center;
            position: relative;
        }
        .hero::before {
            content: '';
            position: absolute;
            top: 50%;
            left: 50%;
            transform: translate(-50%, -50%);
            width: 400px;
            height: 400px;
            background: var(--primary);
            filter: blur(150px);
            opacity: 0.12;
            z-index: -1;
            border-radius: 50%;
        }
        .hero h2 {
            font-size: 48px;
            font-weight: 800;
            letter-spacing: -1px;
            margin-bottom: 20px;
            line-height: 1.2;
            background: linear-gradient(135deg, #ffffff, #9ca3af);
            -webkit-background-clip: text;
            -webkit-text-fill-color: transparent;
        }
        .hero p {
            max-width: 700px;
            margin: 0 auto 40px;
            font-size: 18px;
            color: var(--text-muted);
        }
        .btn-group {
            display: flex;
            justify-content: center;
            gap: 16px;
        }
        .btn {
            padding: 12px 28px;
            border-radius: 8px;
            font-weight: 600;
            text-decoration: none;
            transition: all 0.2s;
            font-size: 15px;
        }
        .btn-primary {
            background: var(--grad-accent);
            color: #fff;
            box-shadow: 0 4px 20px rgba(99, 102, 241, 0.4);
        }
        .btn-primary:hover {
            transform: translateY(-2px);
            box-shadow: 0 6px 24px rgba(99, 102, 241, 0.5);
        }
        .btn-secondary {
            background-color: var(--bg-element);
            color: var(--text-main);
            border: 1px solid var(--border);
        }
        .btn-secondary:hover {
            background-color: #202b4d;
        }
        .container {
            max-width: 1200px;
            margin: 0 auto;
            padding: 60px 24px;
        }
        .features {
            display: grid;
            grid-template-columns: repeat(auto-fit, minmax(300px, 1fr));
            gap: 30px;
            margin-top: 40px;
        }
        .feature-card {
            background-color: var(--bg-surface);
            border: 1px solid var(--border);
            border-radius: 12px;
            padding: 30px;
            transition: transform 0.3s, border-color 0.3s;
        }
        .feature-card:hover {
            transform: translateY(-4px);
            border-color: var(--primary);
        }
        .feature-icon {
            font-size: 32px;
            margin-bottom: 20px;
            display: inline-block;
        }
        .feature-card h3 {
            font-size: 20px;
            font-weight: 700;
            margin-bottom: 12px;
        }
        .feature-card p {
            color: var(--text-muted);
            font-size: 14px;
        }
        .docs-section {
            background-color: var(--bg-surface);
            border: 1px solid var(--border);
            border-radius: 16px;
            padding: 40px;
            margin-top: 60px;
        }
        .docs-section h3 {
            font-size: 28px;
            font-weight: 800;
            margin-bottom: 24px;
            border-bottom: 1px solid var(--border);
            padding-bottom: 16px;
        }
        .code-title {
            font-size: 14px;
            font-weight: 600;
            color: #a5b4fc;
            margin-top: 24px;
            margin-bottom: 8px;
            display: flex;
            align-items: center;
            gap: 6px;
        }
        .code-block {
            background-color: var(--bg-base);
            border: 1px solid var(--border);
            border-radius: 8px;
            padding: 20px;
            font-family: 'JetBrains Mono', monospace;
            font-size: 14px;
            overflow-x: auto;
            color: #cbd5e1;
            margin-bottom: 20px;
        }
        footer {
            border-top: 1px solid var(--border);
            padding: 40px 24px;
            text-align: center;
            color: var(--text-muted);
            font-size: 14px;
            margin-top: 80px;
        }
    </style>
</head>
<body>
    <header>
        <h1>Magic Tunnel Mesh (MTM) 🚀</h1>
        <nav>
            <a href="#features">Features</a>
            <a href="#docs">Documentation</a>
        </nav>
    </header>

    <div class="hero">
        <h2>Zero-Trust Multi-Protocol Tunneling</h2>
        <p>Expose your IoT and local home servers (Proxmox, Mini PCs) to the public internet securely. Bypasses NAT and dynamic IPs with lightning-fast UDP/QUIC streams and automatic Wildcard SSL/TLS certificates.</p>
        <div class="btn-group">
            <a href="#docs" class="btn btn-primary">Get Started Now</a>
            <a href="https://github.com/Jualhosting/go-online" class="btn btn-secondary" target="_blank">View GitHub</a>
        </div>
    </div>

    <div class="container" id="features">
        <h3 style="font-size: 32px; font-weight: 800; text-align: center; margin-bottom: 40px;">Why MTM is Built Different</h3>
        <div class="features">
            <div class="feature-card">
                <span class="feature-icon">⚡</span>
                <h3>QUIC Speed & Resilience</h3>
                <p>Built entirely on UDP-based HTTP/3 QUIC protocol. Resilient to network changes/drops with immediate reconnection and excellent NAT traversal.</p>
            </div>
            <div class="feature-card">
                <span class="feature-icon">🔒</span>
                <h3>Automatic SSL/TLS</h3>
                <p>Caddy's CertMagic engine handles wildcard TLS certificates (*.goinstant.my.id) dynamically using Cloudflare DNS-01 API challenges.</p>
            </div>
            <div class="feature-card">
                <span class="feature-icon">🔎</span>
                <h3>Web Inspector Panel</h3>
                <p>Local dashboard dashboard (localhost:4040) captures request headers, bodies, and responses. Includes one-click request replay to test webhooks.</p>
            </div>
            <div class="feature-card">
                <span class="feature-icon">📦</span>
                <h3>Docker & Go Native</h3>
                <p>Lightweight multi-stage Docker build for VPS, with single cross-platform biner client for PC/IoT (no dependencies needed).</p>
            </div>
        </div>

        <div class="docs-section" id="docs">
            <h3>Usage & Quick Start Documentation</h3>

            <div class="code-title">1. Deploy the Server (VPS Instance)</div>
            <p style="color: var(--text-muted); margin-bottom: 12px; font-size: 14px;">Clone the repository, create your configured '.env' file containing your Cloudflare token, and start the container orchestration:</p>
            <div class="code-block">
git clone https://github.com/Jualhosting/go-online.git
cd go-online
cp .env.example .env
# Edit .env and enter your CLOUDFLARE_API_TOKEN, domain and emails

docker compose up -d --build
            </div>

            <div class="code-title">2. Run the Client (Your Home Server / IoT / Local Machine)</div>
            <p style="color: var(--text-muted); margin-bottom: 12px; font-size: 14px;">Use the compiled Go binary to establish a QUIC tunnel directly back to your VPS, exposing your desired local port:</p>
            <div class="code-block">
# Compile the binary
go build -o mtm main.go

# Start the client tunnel (e.g. expose local port 8080 as subdomain "app")
./mtm client -server your_vps_ip:9000 -subdomain app -token your_mtm_auth_token -target 8080
            </div>

            <div class="code-title">3. View Web Traffic (Local Dashboard Panel)</div>
            <p style="color: var(--text-muted); margin-bottom: 12px; font-size: 14px;">Open your web browser locally to inspect forwarded headers, JSON payload bodies, and replay requests:</p>
            <div class="code-block">
Open: http://localhost:4040
            </div>
        </div>
    </div>

    <footer>
        <p>&copy; 2026 Magic Tunnel Mesh (MTM). All rights reserved.</p>
    </footer>
</body>
</html>
`
