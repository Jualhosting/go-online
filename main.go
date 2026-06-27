package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"go-online/client"
	"go-online/server"

	"github.com/joho/godotenv"
)

func main() {
	// Attempt to load .env file if present (won't error if missing in container environments)
	_ = godotenv.Load()

	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	subcommand := strings.ToLower(os.Args[1])

	switch subcommand {
	case "server":
		runServer()
	case "client":
		runClient()
	default:
		log.Printf("Unknown subcommand: %s", subcommand)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println("Usage: mtm [server|client] [options]")
	fmt.Println("\nSubcommands:")
	fmt.Println("  server    Start the MTM tunnel server (usually run on public VPS)")
	fmt.Println("  client    Start the local tunnel client (exposes local ports to VPS)")
	fmt.Println("\nUse 'mtm server -help' or 'mtm client -help' to see specific flags.")
}

func getEnvOrDefault(key, defaultValue string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultValue
}

func runServer() {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	
	addr := fs.String("addr", getEnvOrDefault("MTM_SERVER_ADDR", "0.0.0.0:9000"), "QUIC tunnel listening address")
	domain := fs.String("domain", getEnvOrDefault("MTM_DOMAIN", "localhost"), "Root domain name for subdomains")
	token := fs.String("token", getEnvOrDefault("MTM_AUTH_TOKEN", "mtm_secure_handshake_token_2026"), "Shared secret token for client authorization")
	email := fs.String("email", getEnvOrDefault("MTM_ACME_EMAIL", "admin@localhost"), "Email address for ACME Let's Encrypt certificates")
	httpPort := fs.String("http", getEnvOrDefault("MTM_HTTP_PORT", "80"), "Port to listen on for HTTP traffic")
	httpsPort := fs.String("https", getEnvOrDefault("MTM_HTTPS_PORT", "443"), "Port to listen on for HTTPS traffic")

	_ = fs.Parse(os.Args[2:])

	log.Printf("[Server] Starting Magic Tunnel Mesh server...")
	log.Printf("[Server] Subdomain root: *.%s", *domain)
	log.Printf("[Server] HTTP Port: %s | HTTPS Port: %s", *httpPort, *httpsPort)

	srv := server.NewTunnelServer(*addr, *domain, *token, *email, *httpPort, *httpsPort)
	if err := srv.Start(); err != nil {
		log.Fatalf("[Server] Fatal error: %v", err)
	}
}

func runClient() {
	fs := flag.NewFlagSet("client", flag.ExitOnError)

	serverAddr := fs.String("server", getEnvOrDefault("MTM_SERVER_ADDR", "127.0.0.1:9000"), "QUIC tunnel server address (e.g. vps_ip:9000)")
	subdomain := fs.String("subdomain", getEnvOrDefault("MTM_CLIENT_SUBDOMAIN", "myapp"), "Requested subdomain prefix")
	token := fs.String("token", getEnvOrDefault("MTM_AUTH_TOKEN", "mtm_secure_handshake_token_2026"), "Shared authorization secret token")
	target := fs.String("target", getEnvOrDefault("MTM_CLIENT_TARGET_PORT", "8080"), "Target local port or address (e.g. 8080 or localhost:3000)")
	inspectorPort := fs.String("inspector", "4040", "Port to run the local Web Traffic Inspector Dashboard")

	_ = fs.Parse(os.Args[2:])

	// Standardize target format (e.g. "8080" -> "127.0.0.1:8080")
	targetAddr := *target
	if !strings.Contains(targetAddr, ":") {
		targetAddr = "127.0.0.1:" + targetAddr
	}

	log.Printf("[Client] Exposing local service %s", targetAddr)
	log.Printf("[Client] Requesting subdomain: %s", *subdomain)

	// Start local dashboard inspector
	inspector := client.NewInspectorServer(*inspectorPort, targetAddr)
	inspector.Start()

	// Start QUIC client tunnel
	cli := client.NewTunnelClient(*serverAddr, *subdomain, *token, targetAddr, inspector)
	cli.Start()
}
