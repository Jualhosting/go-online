package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"net"
	"os"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/libdns/cloudflare"
)

// DomainValidator is a callback used to check if a domain is authorized for On-Demand TLS.
var DomainValidator func(domain string) bool

// GetTLSConfig returns a tls.Config. If the domain is localhost, it generates a self-signed cert.
// Otherwise, it initializes CertMagic for automatic Let's Encrypt SSL/TLS.
func GetTLSConfig(domain string, email string) (*tls.Config, error) {
	if domain == "localhost" || domain == "127.0.0.1" {
		log.Println("[TLS] Localhost detected. Generating self-signed certificate...")
		return GenerateSelfSignedConfig()
	}

	log.Printf("[TLS] Domain %s detected. Initializing CertMagic for dynamic SSL...", domain)
	
	// Configure CertMagic
	certmagic.DefaultACME.Email = email
	certmagic.DefaultACME.CA = certmagic.LetsEncryptProductionCA

	// Configure Cloudflare DNS-01 solver if token is provided
	if cfToken := os.Getenv("CLOUDFLARE_API_TOKEN"); cfToken != "" {
		log.Println("[TLS] Cloudflare API Token found. Configuring DNS-01 Challenge solver for wildcards...")
		certmagic.DefaultACME.DNS01Solver = &certmagic.DNS01Solver{
			DNSManager: certmagic.DNSManager{
				DNSProvider: &cloudflare.Provider{
					APIToken: cfToken,
				},
			},
		}
	}

	storageDir := "./certs"
	if err := os.MkdirAll(storageDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create certs storage: %w", err)
	}
	certmagic.Default.Storage = &certmagic.FileStorage{Path: storageDir}

	// We enable On-Demand TLS so certificates are requested the first time a client's subdomain is visited
	certmagic.Default.OnDemand = &certmagic.OnDemandConfig{
		DecisionFunc: func(ctx context.Context, name string) error {
			log.Printf("[CertMagic] Deciding whether to allow certificate for: %s", name)
			if DomainValidator != nil {
				if DomainValidator(name) {
					return nil
				}
				return fmt.Errorf("domain %s is not allowed for On-Demand TLS", name)
			}
			return nil
		},
	}

	var magic *certmagic.Config
	magicCache := certmagic.NewCache(certmagic.CacheOptions{
		GetConfigForCert: func(cert certmagic.Certificate) (*certmagic.Config, error) {
			return magic, nil
		},
	})
	magic = certmagic.New(magicCache, certmagic.Default)

	return magic.TLSConfig(), nil
}

// GenerateSelfSignedConfig creates a standard self-signed tls.Config for local testing.
func GenerateSelfSignedConfig() (*tls.Config, error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}

	keyUsage := x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature
	notBefore := time.Now()
	notAfter := notBefore.Add(365 * 24 * time.Hour)

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return nil, err
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"Magic Tunnel Mesh (MTM)"},
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              keyUsage,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	template.IPAddresses = append(template.IPAddresses, net.ParseIP("127.0.0.1"))
	template.DNSNames = append(template.DNSNames, "localhost")

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return nil, err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
	}, nil
}
