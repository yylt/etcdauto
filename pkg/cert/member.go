package cert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"net"
	"strings"
	"time"
)

// MemberCertConfig holds member certificate configuration
type MemberCertConfig struct {
	CommonName    string   // Member common name (e.g., pod name)
	Organization  string   // Organization name
	DNSNames      []string // DNS names for the certificate
	IPAddresses   []string // IP addresses for the certificate
	ValidityYears int      // Certificate validity in years
}

// MemberCertificate holds member certificate and private key
type MemberCertificate struct {
	Certificate *x509.Certificate
	PrivateKey  *ecdsa.PrivateKey
	CertPEM     []byte
	KeyPEM      []byte
}

// GenerateMemberCert generates a member certificate signed by the CA
func GenerateMemberCert(ca *CACertificate, cfg *MemberCertConfig) (*MemberCertificate, error) {
	// Generate private key
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to generate private key: %w", err)
	}

	// Calculate validity period
	notBefore := time.Now()
	notAfter := notBefore.AddDate(cfg.ValidityYears, 0, 0)

	// Create certificate template
	serialNumber, err := generateSerialNumber()
	if err != nil {
		return nil, fmt.Errorf("failed to generate serial number: %w", err)
	}

	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName:   cfg.CommonName,
			Organization: []string{cfg.Organization},
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		DNSNames:              cfg.DNSNames,
	}

	// Add IP addresses
	for _, ipStr := range cfg.IPAddresses {
		ip := net.ParseIP(strings.TrimSpace(ipStr))
		if ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		}
	}

	// Sign certificate with CA
	certDER, err := x509.CreateCertificate(rand.Reader, template, ca.Certificate, &privateKey.PublicKey, ca.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create certificate: %w", err)
	}

	// Parse the certificate
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("failed to parse certificate: %w", err)
	}

	// Encode certificate to PEM
	certPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certDER,
	})

	// Encode private key to PEM
	keyBytes, err := x509.MarshalECPrivateKey(privateKey)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal private key: %w", err)
	}

	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "EC PRIVATE KEY",
		Bytes: keyBytes,
	})

	return &MemberCertificate{
		Certificate: cert,
		PrivateKey:  privateKey,
		CertPEM:     certPEM,
		KeyPEM:      keyPEM,
	}, nil
}

// LoadMemberCert loads member certificate and private key from PEM data
func LoadMemberCert(certPEM, keyPEM []byte) (*MemberCertificate, error) {
	cert, privateKey, err := loadCertAndKey(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}

	return &MemberCertificate{
		Certificate: cert,
		PrivateKey:  privateKey,
		CertPEM:     certPEM,
		KeyPEM:      keyPEM,
	}, nil
}

// VerifyCertificate verifies that a certificate is valid and signed by the CA
func VerifyCertificate(cert *x509.Certificate, ca *CACertificate) error {
	roots := x509.NewCertPool()
	roots.AddCert(ca.Certificate)

	opts := x509.VerifyOptions{
		Roots:     roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}

	if _, err := cert.Verify(opts); err != nil {
		return fmt.Errorf("certificate verification failed: %w", err)
	}

	return nil
}

// IsCertificateExpiringSoon checks if certificate will expire within the given duration
func IsCertificateExpiringSoon(cert *x509.Certificate, threshold time.Duration) bool {
	return time.Until(cert.NotAfter) < threshold
}
