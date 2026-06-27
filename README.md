# Magic Tunnel Mesh (MTM) 🚀

MTM (Magic Tunnel Mesh) is a modern, high-performance Zero-Trust Tunnel system written in pure Go. It allows developers to securely expose local applications (HTTP/HTTPS, SSH, DBs, etc.) running on home servers, mini-PCs (Proxmox, SBCs), or IoT devices to the public internet without public IP addresses, NAT configuration, or port forwarding.

It utilizes **QUIC (HTTP/3-based transport over UDP)** for lightning-fast and firewall-resistant tunneling, and integrates Caddy's **CertMagic** engine for automatic Let's Encrypt / ZeroSSL TLS certificate termination on the server.

---

## Key Features

- **UDP/QUIC Under the Hood**: Excellent NAT-traversal, instant reconnection on network switches, and high throughput.
- **Automatic SSL/TLS Termination**: Caddy's `CertMagic` engine automatically handles HTTP-01 challenges and secures your public subdomains in real-time.
- **Local Dashboard & Web Inspector**: A premium dark-mode web console running on `localhost:4040` allowing you to inspect request/response headers, bodies, and **replay** requests with a single click.
- **Docker Ready**: Built-in multi-stage `Dockerfile` and `docker-compose.yml` configuration.
- **Cross-Platform**: Compile into a single, dependency-free binary for Windows, macOS, Linux, and FreeBSD.

---

## Project Structure

```
├── main.go               # Command line entrypoint (client/server router)
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

Edit the `.env` file to set your domains, server addresses, and tokens:
- `MTM_SERVER_ADDR`: Bind address for server QUIC listener (e.g. `0.0.0.0:9000`).
- `MTM_DOMAIN`: Root domain (e.g., `yourdomain.com`).
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
go run main.go server -addr 0.0.0.0:9000 -domain yourdomain.com -token mtm_secret_key
```

---

## Running the Client (Local Machine / Home Server)

Expose a local application (e.g. running on port `8080`) to the public internet using your server:

```bash
go run main.go client -server your_vps_ip:9000 -subdomain myapp -token mtm_secret_key -target 8080
```

Once connected, accessing `https://myapp.yourdomain.com` will route traffic through the QUIC tunnel straight to your local application running on port `8080`!

---

## Traffic Inspector Dashboard

Open **[http://localhost:4040](http://localhost:4040)** in your browser to access the local developer panel:
- **Inspect Traffic**: Read request parameters, JSON payloads, headers, and responses.
- **Request Replay (1-Click)**: Resend any webhook or request to your local application instantly, making API/webhook development highly efficient.

---

## Security Best Practices

1. **Keep `.env` Private**: The `.env` file is excluded from git tracking in `.gitignore`. Do not commit actual tokens to public repositories.
2. **ACME Wildcards**: For production, point wildcard DNS record `*.yourdomain.com` to your VPS public IP.
