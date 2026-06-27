# Magic Tunnel Mesh (MTM) / GoInstant 🚀

MTM (Magic Tunnel Mesh) / GoInstant is a modern, high-performance Zero-Trust Tunnel system written in pure Go. It allows developers to securely expose local applications (HTTP/HTTPS, SSH, DBs, etc.) running on home servers, mini-PCs (Proxmox, SBCs), or IoT devices to the public internet without public IP addresses, NAT configuration, or port forwarding.

It utilizes **QUIC (HTTP/3-based transport over UDP)** for lightning-fast and firewall-resistant tunneling, and integrates Caddy's **CertMagic** engine for automatic Let's Encrypt / ZeroSSL TLS certificate termination on the server.

---

## Key Features

- **Zero-Dependency Static Exposing**: Instantly expose a local static folder or HTML file (e.g., `D:\file client`) to the public internet. No Python, Node.js, or separate HTTP server installation needed!
- **UDP/QUIC Under the Hood**: Excellent NAT-traversal, instant reconnection on network switches, and high throughput.
- **Automatic SSL/TLS Termination**: Caddy's `CertMagic` engine automatically handles HTTP-01 and DNS-01 challenges to secure public subdomains.
- **Local Dashboard & Web Inspector**: A premium dark-mode web console running on `localhost:4040` allowing you to inspect request/response headers, bodies, and **replay** requests with a single click.
- **Docker Ready**: Built-in multi-stage `Dockerfile` and `docker-compose.yml` configuration for the server.

---

## Project Structure

```
├── main.go               # Command line entrypoint (client/server router)
├── server/
├── server/
│   ├── server.go         # QUIC Server listener, traffic proxy, and dynamic HTTP routing
│   └── certs.go          # CertMagic and self-signed TLS handlers
├── client/
│   ├── client.go         # QUIC Client tunnel connection and local proxy
│   └── inspector.go      # Local web dashboard inspector & replay engine
├── common/
│   └── protocol.go       # Handshake frames, stream headers, and JSON serialization
├── Dockerfile            # Multi-stage optimized Docker build
└── docker-compose.yml    # Server service orchestration
```

---

## Getting Started

### Prerequisites
- Go 1.22+ (if building from source)
- Docker & Docker Compose (for server deployment)

### 1. Setup Configuration
Create a `.env` file from the example:
```bash
cp .env.example .env
```
Edit the `.env` file to set your root domains, server addresses, tokens, and Cloudflare credentials (if using DNS-01 wildcards):
- `MTM_SERVER_ADDR`: Bind address for server QUIC listener (e.g. `0.0.0.0:9000`).
- `MTM_DOMAIN`: Root domain (e.g., `goinstant.my.id`).
- `MTM_AUTH_TOKEN`: Secret key for client authorization.
- `MTM_ACME_EMAIL`: Let's Encrypt recovery email.

---

## Running the Server (VPS)

To start the server using Docker Compose (which exposes port 80, 443, and 9000 UDP, and persists SSL certificates):
```bash
docker compose up -d --build
```

To run the server directly using the Go binary:
```bash
go run main.go server -addr 0.0.0.0:9000 -domain goinstant.my.id -token mtm_secure_handshake_token_2026
```

---

## Running the Client (Local Machine)

Build the client binary:
```bash
go build -o goinstant.exe main.go
```

Expose a local port or deploy a static folder to the public internet using your server:

### A. Expose Local Port (Ngrok-style Tunnel)
To expose an application already running locally on port `8080`:
```powershell
# Using flags
go run main.go expose --port 8080

# Using positional fallback
go run main.go 8080
```

### B. Deploy Static Directory (Netlify-style Hosting)
To upload and host a static folder (like `D:\file client` containing `index.html`) directly on your VPS server:
```powershell
# Using flags
go run main.go deploy --dir "D:\file client"

# Using positional fallback
go run main.go "D:\file client"
```
*Behind the scenes, the client packages the folder into a zip file, uploads it to the VPS's `/api/deploy` endpoint over TLS (authenticated with your token), and extracts it. The VPS then hosts your files statically under an SSL-secured subdomain (e.g. `https://magic-site-482.goinstant.my.id`).*

---

## Traffic Inspector Dashboard

Open **[http://localhost:4040](http://localhost:4040)** in your browser to access the local developer panel:
- **Inspect Traffic**: Read request parameters, JSON payloads, headers, and responses.
- **Request Replay (1-Click)**: Resend any webhook or request to your local application instantly.

---

## AI Agent & Developer Knowledge Base 🧠

> [!IMPORTANT]
> **Guidelines for AI Coding Assistants working on this repo:**
> 1. **Subcommands**:
>    * `expose`: Establishes a live QUIC tunnel to expose a local port or address. (Flags: `--port`, `--subdomain`, `--server`, `--token`).
>    * `deploy`: Packages a local static directory into a zip archive and uploads it to the VPS (`/api/deploy`) for hosting. (Flags: `--dir`, `--subdomain`, `--server`, `--token`).
> 2. **Fallback Auto-detection**: If no subcommand is specified (e.g., `go run main.go <arg>`), the CLI auto-detects. If the argument is a valid directory on disk, it executes `deploy`; otherwise, it treats it as a port and executes `expose`.
> 3. **Server Static Hosting**: The VPS server dynamically intercepts HTTP requests. If an active QUIC tunnel exists for a subdomain, it proxies requests to the tunnel. If the tunnel is offline but a deployed static folder exists under `./deployed/<subdomain>/`, it serves files statically.
> 4. **Default VPS Domain**: The default domain is `goinstant.my.id:9000`.
> 5. **Subdomain Generation**: If no subdomain is specified, the client uses `generateRandomSubdomain()` to yield user-friendly Vercel-style subdomains (e.g. `clean-node-305`).
