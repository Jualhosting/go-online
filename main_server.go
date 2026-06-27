//go:build !client

package main

import (
	"flag"
	"log"
	"os"

	"go-online/server"
)

func runServer() {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	
	addr := fs.String("addr", getEnvOrDefault("MTM_SERVER_ADDR", "0.0.0.0:443"), "QUIC tunnel listening address")
	domain := fs.String("domain", getEnvOrDefault("MTM_DOMAIN", "localhost"), "Root domain name for subdomains")
	token := fs.String("token", getEnvOrDefault("MTM_AUTH_TOKEN", "mtm_secure_handshake_token_2026"), "Shared secret token for client authorization")
	email := fs.String("email", getEnvOrDefault("MTM_ACME_EMAIL", "admin@localhost"), "Email address for ACME Let's Encrypt certificates")
	httpPort := fs.String("http", getEnvOrDefault("MTM_HTTP_PORT", "80"), "Port to listen on for HTTP traffic")
	httpsPort := fs.String("https", getEnvOrDefault("MTM_HTTPS_PORT", "443"), "Port to listen on for HTTPS traffic")
	dbPath := fs.String("db", getEnvOrDefault("MTM_DB_PATH", "./config.db"), "Path to the SQLite configuration database")
	deployDir := fs.String("deploy-dir", getEnvOrDefault("MTM_DEPLOY_DIR", "./deployed"), "Directory to save static deployment assets")

	_ = fs.Parse(os.Args[2:])

	log.Printf("[Server] Starting Magic Tunnel Mesh server...")
	log.Printf("[Server] Subdomain root: *.%s", *domain)
	log.Printf("[Server] HTTP Port: %s | HTTPS Port: %s", *httpPort, *httpsPort)
	log.Printf("[Server] Database Path: %s | Deploy Directory: %s", *dbPath, *deployDir)

	srv := server.NewTunnelServer(*addr, *domain, *token, *email, *httpPort, *httpsPort)
	srv.DBPath = *dbPath
	srv.DeployDir = *deployDir
	if err := srv.Start(); err != nil {
		log.Fatalf("[Server] Fatal error: %v", err)
	}
}
