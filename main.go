package main

import (
	"archive/zip"
	"bytes"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go-online/client"
	"go-online/common"

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
	case "expose":
		runExpose(os.Args[2:])
	case "deploy":
		runDeploy(os.Args[2:])
	case "client": // legacy support
		runExpose(os.Args[2:])
	default:
		// Fallback auto-detection:
		// If first arg is a valid directory/file path on disk, run deploy, otherwise run expose
		arg := os.Args[1]
		if _, err := os.Stat(arg); err == nil {
			runDeploy(os.Args[1:])
		} else {
			runExpose(os.Args[1:])
		}
	}
}

func printUsage() {
	fmt.Println("Usage: goinstant [expose|deploy|server] [options]")
	fmt.Println("\nSubcommands:")
	fmt.Println("  server    Start the MTM tunnel server (usually run on public VPS)")
	fmt.Println("  expose    Start local tunnel client and expose a target port (default)")
	fmt.Println("  deploy    Directly deploy a static directory (HTML/CSS/JS) to the server")
	fmt.Println("\nExamples:")
	fmt.Println("  goinstant expose --port 8080")
	fmt.Println("  goinstant deploy --dir \"D:\\file client\"")
	fmt.Println("  goinstant 8080")
	fmt.Println("  goinstant \"D:\\file client\"")
}

func getEnvOrDefault(key, defaultValue string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultValue
}



func runExpose(args []string) {
	fs := flag.NewFlagSet("expose", flag.ExitOnError)

	serverAddr := fs.String("server", getEnvOrDefault("MTM_SERVER_ADDR", "goinstant.my.id:9000"), "QUIC tunnel server address")
	subdomain := fs.String("subdomain", getEnvOrDefault("MTM_CLIENT_SUBDOMAIN", ""), "Requested subdomain prefix")
	defaultToken := common.LoadLocalToken()
	if defaultToken == "" {
		defaultToken = getEnvOrDefault("MTM_AUTH_TOKEN", "mtm_secure_handshake_token_2026")
	}
	token := fs.String("token", defaultToken, "Shared authorization secret token")
	port := fs.String("port", "", "Target local port to expose (e.g. 8080)")
	target := fs.String("target", "", "Target local address (e.g. localhost:8080)")
	inspectorPort := fs.String("inspector", "4040", "Port to run the local Web Traffic Inspector Dashboard")

	_ = fs.Parse(args)

	// Determine local target address
	targetVal := *target
	if targetVal == "" {
		targetVal = *port
	}
	if targetVal == "" && fs.NArg() > 0 {
		targetVal = fs.Arg(0)
	}
	if targetVal == "" {
		targetVal = getEnvOrDefault("MTM_CLIENT_TARGET_PORT", "8080")
	}

	// Standardize target format (e.g. "8080" -> "127.0.0.1:8080")
	targetAddr := targetVal
	if !strings.Contains(targetAddr, ":") {
		targetAddr = "127.0.0.1:" + targetAddr
	}

	subdomainVal := *subdomain
	if subdomainVal == "" {
		subdomainVal = generateRandomSubdomain()
	}

	// Extract domain name from the server address (remove port if exists)
	hostOnly, _, err := net.SplitHostPort(*serverAddr)
	if err != nil {
		hostOnly = *serverAddr
	}
	
	publicDomain := hostOnly
	if publicDomain == "127.0.0.1" || publicDomain == "localhost" || publicDomain == "0.0.0.0" {
		publicDomain = "goinstant.my.id"
	}

	// Print beautiful premium output
	fmt.Println()
	fmt.Println("  🚀  goinstant expose is exposing your local service!")
	fmt.Println("  ==================================================")
	fmt.Printf("  🔌  Local Service:  http://%s\n", targetAddr)
	fmt.Printf("  🔒  Public URL:     https://%s.%s\n", subdomainVal, publicDomain)
	fmt.Printf("  📊  Web Inspector:  http://localhost:%s\n", *inspectorPort)
	fmt.Println("  ==================================================")
	fmt.Println("  Press Ctrl+C to stop exposing.")
	fmt.Println()

	// Start local dashboard inspector
	inspector := client.NewInspectorServer(*inspectorPort, targetAddr)
	inspector.Start()

	// Start QUIC client tunnel
	cli := client.NewTunnelClient(*serverAddr, subdomainVal, *token, targetAddr, inspector)
	cli.Start()
}

func runDeploy(args []string) {
	fs := flag.NewFlagSet("deploy", flag.ExitOnError)

	serverAddr := fs.String("server", getEnvOrDefault("MTM_SERVER_ADDR", "goinstant.my.id:9000"), "QUIC tunnel server address")
	subdomain := fs.String("subdomain", getEnvOrDefault("MTM_CLIENT_SUBDOMAIN", ""), "Requested subdomain prefix")
	defaultToken := common.LoadLocalToken()
	if defaultToken == "" {
		defaultToken = getEnvOrDefault("MTM_AUTH_TOKEN", "mtm_secure_handshake_token_2026")
	}
	token := fs.String("token", defaultToken, "Shared secret token")
	dir := fs.String("dir", "", "Local static directory to deploy")

	_ = fs.Parse(args)

	dirVal := *dir
	if dirVal == "" && fs.NArg() > 0 {
		dirVal = fs.Arg(0)
	}
	if dirVal == "" {
		dirVal = "."
	}

	subdomainVal := *subdomain
	if subdomainVal == "" {
		subdomainVal = generateRandomSubdomain()
	}

	log.Printf("[Deploy] Zipping directory: %s...", dirVal)
	zipData, err := zipDirectory(dirVal)
	if err != nil {
		log.Fatalf("[Deploy] Error zipping directory: %v", err)
	}

	// Determine HTTP/HTTPS deploy URL
	hostOnly, _, err := net.SplitHostPort(*serverAddr)
	if err != nil {
		hostOnly = *serverAddr
	}

	scheme := "https"
	portStr := ""
	if hostOnly == "127.0.0.1" || hostOnly == "localhost" {
		scheme = "http"
		if httpPort := os.Getenv("MTM_HTTP_PORT"); httpPort != "" {
			portStr = ":" + httpPort
		}
	}
	
	deployURL := fmt.Sprintf("%s://%s%s/api/deploy", scheme, hostOnly, portStr)
	totalSize := int64(len(zipData))
	log.Printf("[Deploy] Target server: %s", deployURL)
	log.Printf("[Deploy] Package payload size: %s", formatBytes(totalSize))

	pr := &progressReader{
		r:     bytes.NewReader(zipData),
		total: totalSize,
	}

	req, err := http.NewRequest("POST", deployURL, pr)
	if err != nil {
		log.Fatalf("[Deploy] Failed to create HTTP request: %v", err)
	}
	req.ContentLength = totalSize
	req.Header.Set("Authorization", "Bearer "+*token)
	req.Header.Set("X-Subdomain", subdomainVal)
	req.Header.Set("Content-Type", "application/zip")

	// Transport configured to skip verification if local or IP testing
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	httpClient := &http.Client{Transport: tr, Timeout: 90 * time.Second}

	resp, err := httpClient.Do(req)
	if err != nil {
		log.Fatalf("[Deploy] Upload failed: %v", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		log.Fatalf("[Deploy] Upload failed with status %s: %s", resp.Status, string(respBody))
	}

	if newToken := resp.Header.Get("X-GoInstant-Token"); newToken != "" {
		log.Printf("[Deploy] Server assigned new session token: %s. Saving to ~/.goinstant/config.json", newToken)
		_ = common.SaveLocalToken(newToken)
	}

	publicDomain := hostOnly
	if publicDomain == "127.0.0.1" || publicDomain == "localhost" || publicDomain == "0.0.0.0" {
		publicDomain = "goinstant.my.id"
	}

	fmt.Println()
	fmt.Println("  🚀  goinstant deploy successful!")
	fmt.Println("  ==================================================")
	fmt.Printf("  📂  Source Directory:  %s\n", dirVal)
	fmt.Printf("  🔒  Public URL:        https://%s.%s\n", subdomainVal, publicDomain)
	fmt.Println("  ==================================================")
	fmt.Println()
}

func zipDirectory(source string) ([]byte, error) {
	var buf bytes.Buffer
	archive := zip.NewWriter(&buf)

	fileCount := 0
	err := filepath.Walk(source, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			name := info.Name()
			if name == "node_modules" || name == ".git" || name == ".github" || name == ".vscode" || name == ".idea" || name == "tmp" {
				return filepath.SkipDir
			}
			return nil
		}

		// Get relative path
		relPath, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if relPath == "." {
			return nil
		}

		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}

		header.Name = filepath.ToSlash(relPath)
		header.Method = zip.Deflate

		writer, err := archive.CreateHeader(header)
		if err != nil {
			return err
		}

		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()

		_, err = io.Copy(writer, file)
		if err == nil {
			fileCount++
		}
		return err
	})

	if err != nil {
		return nil, err
	}

	err = archive.Close()
	if err != nil {
		return nil, err
	}

	log.Printf("[Deploy] Zipped %d files successfully.", fileCount)
	return buf.Bytes(), nil
}

func generateRandomSubdomain() string {
	adjectives := []string{"happy", "swift", "magic", "bright", "cool", "clean", "fresh", "silent", "flying", "super"}
	nouns := []string{"project", "site", "page", "app", "demo", "server", "mesh", "tunnel", "node", "code"}
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	return fmt.Sprintf("%s-%s-%d", adjectives[r.Intn(len(adjectives))], nouns[r.Intn(len(nouns))], r.Intn(900)+100)
}

type progressReader struct {
	r       io.Reader
	total   int64
	read    int64
	lastPct int
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.r.Read(p)
	if n > 0 {
		pr.read += int64(n)
		pct := int(float64(pr.read) / float64(pr.total) * 100)
		if pct != pr.lastPct {
			fmt.Printf("\r  ⏳  Uploading website files: %d%% (%s/%s)...", pct, formatBytes(pr.read), formatBytes(pr.total))
			pr.lastPct = pct
		}
	}
	if err == io.EOF {
		fmt.Println("\n  ✅  Upload complete! Waiting for server to deploy to Cloudflare R2...")
	}
	return n, err
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
