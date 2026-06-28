package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log"
	mrand "math/rand"
	"net"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

type SshTunnel struct {
	Connection ssh.Conn
	RemotePort uint32
}

func (t *SshTunnel) OpenStream(ctx context.Context, protocol, host string) (net.Conn, error) {
	type forwardedTcpipPayload struct {
		Addr       string
		Port       uint32
		OriginAddr string
		OriginPort uint32
	}

	channel, requests, err := t.Connection.OpenChannel("forwarded-tcpip", ssh.Marshal(forwardedTcpipPayload{
		Addr:       "127.0.0.1",
		Port:       t.RemotePort,
		OriginAddr: "127.0.0.1",
		OriginPort: 0,
	}))
	if err != nil {
		return nil, err
	}
	go ssh.DiscardRequests(requests)
	return &sshConnWrap{Channel: channel, sshConn: t.Connection, subdomain: ""}, nil
}

func (t *SshTunnel) Close() error {
	return t.Connection.Close()
}

type sshConnWrap struct {
	ssh.Channel
	sshConn   ssh.Conn
	subdomain string
}

func (s *sshConnWrap) LocalAddr() net.Addr {
	return s.sshConn.LocalAddr()
}

func (s *sshConnWrap) RemoteAddr() net.Addr {
	return s.sshConn.RemoteAddr()
}

func (s *sshConnWrap) SetDeadline(t time.Time) error {
	return nil
}

func (s *sshConnWrap) SetReadDeadline(t time.Time) error {
	return nil
}

func (s *sshConnWrap) SetWriteDeadline(t time.Time) error {
	return nil
}

func getSSHHostKey() (ssh.Signer, error) {
	keyPath := "ssh_host_key"
	privateBytes, err := os.ReadFile(keyPath)
	if err != nil {
		log.Println("[SSH] Generating temporary host key...")
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return nil, err
		}
		privatePEM := pem.EncodeToMemory(&pem.Block{
			Type:  "RSA PRIVATE KEY",
			Bytes: x509.MarshalPKCS1PrivateKey(key),
		})
		_ = os.WriteFile(keyPath, privatePEM, 0600)
		privateBytes = privatePEM
	}

	return ssh.ParsePrivateKey(privateBytes)
}

func parseSSHUser(user string) (subdomain string, token string) {
	if strings.Contains(user, ":") {
		parts := strings.SplitN(user, ":", 2)
		return parts[0], parts[1]
	}
	if strings.HasPrefix(user, "tok_") {
		return "", user
	}
	low := strings.ToLower(user)
	if low == "git" || low == "ssh" || low == "admin" || low == "root" || low == "ubuntu" || low == "user" || low == "qr" {
		return "", ""
	}
	return user, ""
}

func (s *TunnelServer) StartSSHServer(lis net.Listener) {
	hostKey, err := getSSHHostKey()
	if err != nil {
		log.Fatalf("[SSH] Failed to load/generate host key: %v", err)
	}

	config := &ssh.ServerConfig{
		NoClientAuth: true,
	}
	config.AddHostKey(hostKey)

	log.Println("[SSH] SSH server handler started on multiplexed listener")

	for {
		conn, err := lis.Accept()
		if err != nil {
			log.Printf("[SSH] Accept error: %v", err)
			return
		}
		go s.handleSSHConnection(conn, config)
	}
}

func (s *TunnelServer) handleSSHConnection(conn net.Conn, config *ssh.ServerConfig) {
	sshConn, chans, reqs, err := ssh.NewServerConn(conn, config)
	if err != nil {
		log.Printf("[SSH] Handshake failed: %v", err)
		return
	}

	var subdomain string
	var userID string
	var assignedToken string

	reqSubdomain, reqToken := parseSSHUser(sshConn.User())
	subdomain = strings.ToLower(strings.TrimSpace(reqSubdomain))
	rawToken := reqToken

	if s.db != nil {
		var err error
		if rawToken != "" {
			userID, err = s.db.ValidateUserToken(rawToken)
			if err != nil {
				log.Printf("[SSH] Invalid token provided: %s. Closing connection.", rawToken)
				sshConn.Close()
				return
			}
		} else {
			// Create anonymous session
			assignedToken = "tok_" + fmt.Sprintf("%d", time.Now().UnixNano())
			anonUserID := "usr_" + fmt.Sprintf("%d", time.Now().UnixNano())
			_, errUser := s.db.db.Exec("INSERT INTO users (id, email, plan_type, token, is_anonymous) VALUES (?, ?, ?, ?, 1)", anonUserID, anonUserID+"@anonymous.goinstant.my.id", "free", assignedToken)
			if errUser == nil {
				userID = anonUserID
				rawToken = assignedToken
			} else {
				log.Printf("[SSH] Failed to create anonymous user: %v", errUser)
				sshConn.Close()
				return
			}
		}
	} else {
		userID = "user_syafri"
		if rawToken == "" {
			rawToken = s.AuthToken
		}
	}

	if subdomain == "" {
		subdomain = generateSSHRandomSubdomain()
	}

	_, found := s.routeCache.Load(subdomain)
	if !found {
		if s.db != nil {
			id, err := s.db.RegisterSubdomain(userID, subdomain, "tunnel", "")
			if err != nil {
				log.Printf("[SSH] Failed to auto-register subdomain: %v", err)
				sshConn.Close()
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
			err = s.db.db.QueryRow("SELECT user_id FROM subdomains WHERE subdomain = ?", subdomain).Scan(&ownerID)
			if err == nil && ownerID != userID {
				log.Printf("[SSH] Subdomain %s is owned by another user (%s). Assigning random.", subdomain, ownerID)
				subdomain = generateSSHRandomSubdomain()
				id, err := s.db.RegisterSubdomain(userID, subdomain, "tunnel", "")
				if err != nil {
					sshConn.Close()
					return
				}
				s.routeCache.Store(subdomain, RouteInfo{
					SubdomainID: id,
					Subdomain:   subdomain,
					RoutingType: "tunnel",
					IsActive:    true,
				})
			}
		}
	}

	var forwardPort uint32
	var hasForward bool

	go func() {
		for req := range reqs {
			switch req.Type {
			case "tcpip-forward":
				var payload struct {
					BindAddr string
					BindPort uint32
				}
				if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
					req.Reply(false, nil)
					continue
				}

				forwardPort = payload.BindPort
				hasForward = true

				var respPayload struct {
					Port uint32
				}
				respPayload.Port = forwardPort
				if respPayload.Port == 0 {
					respPayload.Port = uint32(50000 + (time.Now().UnixNano() % 10000))
					forwardPort = respPayload.Port
				}

				req.Reply(true, ssh.Marshal(respPayload))
				log.Printf("[SSH] Forward registered for subdomain %s on port %d", subdomain, forwardPort)
			default:
				req.Reply(false, nil)
			}
		}
	}()

	// Wait up to 2 seconds for forward configuration
	for i := 0; i < 20; i++ {
		if hasForward {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	sshTunnel := &SshTunnel{
		Connection: sshConn,
		RemotePort: forwardPort,
	}

	session := &ClientSession{
		Tunnel:    sshTunnel,
		Subdomain: subdomain,
		Token:     rawToken,
		Type:      "ssh",
	}

	s.clientsMu.Lock()
	if oldSession, exists := s.clients[subdomain]; exists {
		log.Printf("[SSH] Subdomain %s already registered. Disconnecting old session.", subdomain)
		oldSession.Tunnel.Close()
	}
	s.clients[subdomain] = session
	s.clientsMu.Unlock()

	defer func() {
		log.Printf("[SSH] Subdomain %s disconnected", subdomain)
		s.removeClient(subdomain)
		sshConn.Close()
	}()

	if s.db != nil {
		_ = s.db.LogAuditEvent(userID, "expose.ssh", fmt.Sprintf("Opened SSH tunnel for subdomain %s", subdomain))
	}

	for newChan := range chans {
		switch newChan.ChannelType() {
		case "session":
			channel, requests, err := newChan.Accept()
			if err != nil {
				continue
			}
			go s.handleSSHSession(channel, requests, subdomain, rawToken, assignedToken)

		case "direct-tcpip":
			var payload struct {
				Host           string
				Port           uint32
				OriginatorHost string
				OriginatorPort uint32
			}
			if err := ssh.Unmarshal(newChan.ExtraData(), &payload); err != nil {
				newChan.Reject(ssh.ConnectionFailed, "invalid payload")
				continue
			}

			if payload.Port == 4300 {
				channel, requests, err := newChan.Accept()
				if err != nil {
					continue
				}
				go ssh.DiscardRequests(requests)
				
				connWrap := &sshConnWrap{Channel: channel, sshConn: sshConn, subdomain: subdomain}
				select {
				case s.debuggerListener.ch <- connWrap:
				default:
					connWrap.Close()
				}
			} else {
				newChan.Reject(ssh.ConnectionFailed, "port not allowed")
			}
		default:
			newChan.Reject(ssh.UnknownChannelType, "unknown channel type")
		}
	}
}

func (s *TunnelServer) handleSSHSession(ch ssh.Channel, reqs <-chan *ssh.Request, subdomain, token, assignedToken string) {
	defer ch.Close()

	banner := fmt.Sprintf(`
  🚀  goinstant ssh tunnel is active!
  ==================================================
  🔌  Local Service:  127.0.0.1:8080 (via SSH -R)
  🔒  Public URL:     https://%s.%s
  📊  Web Debugger:   http://localhost:4300
  ==================================================
`, subdomain, s.Domain)

	if assignedToken != "" {
		banner += fmt.Sprintf("  🔑  Assigned Token: %s\n  (Saved for authentication in future runs)\n  ==================================================\n", assignedToken)
	}

	banner += "\n  Scan this QR Code link to open on mobile:\n"
	banner += fmt.Sprintf("  https://%s/qr/%s\n\n", s.Domain, subdomain)
	banner += "  Press Ctrl+C to close the tunnel.\n\n"

	_, _ = ch.Write([]byte(banner))

	for req := range reqs {
		switch req.Type {
		case "pty-req":
			req.Reply(true, nil)
		case "shell":
			req.Reply(true, nil)
		default:
			req.Reply(false, nil)
		}
	}

	// Keep shell open until connection closes
	buf := make([]byte, 1024)
	for {
		_, err := ch.Read(buf)
		if err != nil {
			break
		}
	}
}

func generateSSHRandomSubdomain() string {
	adjectives := []string{"happy", "swift", "magic", "bright", "cool", "clean", "fresh", "silent", "flying", "super"}
	nouns := []string{"project", "site", "page", "app", "demo", "server", "mesh", "tunnel", "node", "code"}
	r := mrand.New(mrand.NewSource(time.Now().UnixNano()))
	return fmt.Sprintf("%s-%s-%d", adjectives[r.Intn(len(adjectives))], nouns[r.Intn(len(nouns))], r.Intn(900)+100)
}
