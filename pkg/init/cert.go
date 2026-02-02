package init

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"k8s.io/klog/v2"
)

// CertConfig holds certificate generation configuration
type CertConfig struct {
	PodName     string
	Namespace   string
	ServiceName string
	CliName     string
	PodIP       string
	ExtraIPs    []string
	CertDir     string
	CAFile      string
	CAKeyFile   string
}

// GenerateCertificate generates TLS certificates for etcd
func GenerateCertificate(cfg *CertConfig) error {
	certFile := filepath.Join(cfg.CertDir, fmt.Sprintf("%s.pem", cfg.PodName))
	keyFile := filepath.Join(cfg.CertDir, fmt.Sprintf("%s-key.pem", cfg.PodName))

	// Check if certificate already exists
	if fileExists(certFile) && fileExists(keyFile) {
		klog.Infof("Certificate already exists at %s, skipping generation", certFile)
		return nil
	}

	// Ensure cert directory exists
	if err := os.MkdirAll(cfg.CertDir, 0755); err != nil {
		return fmt.Errorf("failed to create cert directory: %w", err)
	}

	// Load CA certificate and key
	caCert, caKey, err := loadCA(cfg.CAFile, cfg.CAKeyFile)
	if err != nil {
		return fmt.Errorf("failed to load CA: %w", err)
	}

	// Generate private key
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("failed to generate private key: %w", err)
	}

	// Create certificate template
	template := createCertTemplate(cfg)

	// Sign certificate
	certDER, err := x509.CreateCertificate(rand.Reader, template, caCert, &privateKey.PublicKey, caKey)
	if err != nil {
		return fmt.Errorf("failed to create certificate: %w", err)
	}

	// Write certificate and key files
	if err := writeCertFiles(certFile, keyFile, certDER, privateKey); err != nil {
		return err
	}

	// Copy CA certificate to cert directory
	if err := copyCACert(cfg.CAFile, cfg.CertDir); err != nil {
		return err
	}

	klog.Infof("Successfully generated certificate: %s", certFile)
	return nil
}

// createCertTemplate creates a certificate template with DNS names and IP addresses
func createCertTemplate(cfg *CertConfig) *x509.Certificate {
	dnsNames := []string{
		fmt.Sprintf("%s.%s", cfg.CliName, cfg.Namespace),
		fmt.Sprintf("%s.%s.svc", cfg.CliName, cfg.Namespace),
		fmt.Sprintf("%s.%s.svc.cluster.local", cfg.CliName, cfg.Namespace),
		fmt.Sprintf("%s.%s.%s.svc", cfg.PodName, cfg.ServiceName, cfg.Namespace),
		fmt.Sprintf("%s.%s.%s", cfg.PodName, cfg.ServiceName, cfg.Namespace),
		fmt.Sprintf("%s.%s.%s.svc.cluster.local", cfg.PodName, cfg.ServiceName, cfg.Namespace),
		"localhost",
	}

	ipAddresses := []string{"127.0.0.1", cfg.PodIP}
	ipAddresses = append(ipAddresses, cfg.ExtraIPs...)

	template := &x509.Certificate{
		SerialNumber: generateSerialNumber(),
		Subject: pkix.Name{
			CommonName: cfg.PodName,
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(87600 * time.Hour), // 10 years
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
	}

	// Add IP addresses
	for _, ip := range ipAddresses {
		if ip != "" {
			template.IPAddresses = append(template.IPAddresses, parseIP(ip))
		}
	}

	return template
}

// writeCertFiles writes certificate and private key to files
func writeCertFiles(certFile, keyFile string, certDER []byte, privateKey *ecdsa.PrivateKey) error {
	// Write certificate
	certOut, err := os.Create(certFile)
	if err != nil {
		return fmt.Errorf("failed to create cert file: %w", err)
	}
	defer certOut.Close()

	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: certDER}); err != nil {
		return fmt.Errorf("failed to write cert: %w", err)
	}

	// Write private key
	keyOut, err := os.Create(keyFile)
	if err != nil {
		return fmt.Errorf("failed to create key file: %w", err)
	}
	defer keyOut.Close()

	keyBytes, err := x509.MarshalECPrivateKey(privateKey)
	if err != nil {
		return fmt.Errorf("failed to marshal private key: %w", err)
	}

	if err := pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}); err != nil {
		return fmt.Errorf("failed to write key: %w", err)
	}

	return nil
}

// copyCACert copies CA certificate to cert directory
func copyCACert(caFile, certDir string) error {
	caPemFile := filepath.Join(certDir, "ca.pem")
	if !fileExists(caPemFile) {
		if err := copyFile(caFile, caPemFile); err != nil {
			return fmt.Errorf("failed to copy CA cert: %w", err)
		}
	}
	return nil
}

// loadCA loads CA certificate and private key
func loadCA(certFile, keyFile string) (*x509.Certificate, interface{}, error) {
	caCert, err := loadCACertificate(certFile)
	if err != nil {
		return nil, nil, err
	}

	caKey, err := loadCAPrivateKey(keyFile)
	if err != nil {
		return nil, nil, err
	}

	return caCert, caKey, nil
}

// loadCACertificate loads CA certificate from file
func loadCACertificate(certFile string) (*x509.Certificate, error) {
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read CA cert: %w", err)
	}

	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, fmt.Errorf("failed to decode CA cert PEM")
	}

	caCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse CA cert: %w", err)
	}

	return caCert, nil
}

// loadCAPrivateKey loads CA private key from file
func loadCAPrivateKey(keyFile string) (interface{}, error) {
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read CA key: %w", err)
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, fmt.Errorf("failed to decode CA key PEM")
	}

	var caKey interface{}
	switch keyBlock.Type {
	case "RSA PRIVATE KEY":
		caKey, err = x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	case "EC PRIVATE KEY":
		caKey, err = x509.ParseECPrivateKey(keyBlock.Bytes)
	case "PRIVATE KEY":
		caKey, err = x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	default:
		return nil, fmt.Errorf("unsupported key type: %s", keyBlock.Type)
	}

	if err != nil {
		return nil, fmt.Errorf("failed to parse CA key: %w", err)
	}

	return caKey, nil
}

// generateSerialNumber generates a random serial number for certificate
func generateSerialNumber() *big.Int {
	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, _ := rand.Int(rand.Reader, serialNumberLimit)
	return serialNumber
}

// parseIP parses IP address string
func parseIP(ipStr string) net.IP {
	ip := net.ParseIP(strings.TrimSpace(ipStr))
	if ip == nil {
		klog.Warningf("Failed to parse IP: %s", ipStr)
	}
	return ip
}

// fileExists checks if a file exists
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// copyFile copies a file from src to dst
func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0644)
}
