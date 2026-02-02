package init

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/klog/v2"
)

// EnvConfig holds environment configuration for etcd
type EnvConfig struct {
	// Pod information
	PodName      string
	PodNamespace string
	PodIPs       []string

	// Service information
	ServiceName string
	CliName     string
	Max         int

	// Network configuration
	ClientPort string
	PeerPort   string
	ClientURLs []string
	PeerURLs   []string

	// Directory configuration
	DataDir   string
	CertDir   string
	NodeIPDir string

	// Labels for pod selection
	Labels string
}

// SetupEnvironment sets up all required environment variables for etcd
func SetupEnvironment(cfg *EnvConfig) error {
	envVars := map[string]string{
		// Pod information
		"POD_NAME":      cfg.PodName,
		"POD_NAMESPACE": cfg.PodNamespace,
		"POD_IPS":       strings.Join(cfg.PodIPs, ","),

		// Service information
		"SERVICE_NAME": cfg.ServiceName,
		"MAX":          fmt.Sprintf("%d", cfg.Max),
		"LABELS":       cfg.Labels,

		// Network ports
		"CLIENT_PORT": cfg.ClientPort,
		"PEER_PORT":   cfg.PeerPort,

		// Directory configuration
		"NODEIP_DIR": cfg.NodeIPDir,

		// ETCD configuration
		"ETCD_DATA_DIR":                                 cfg.DataDir,
		"ETCD_NAME":                                     cfg.PodName,
		"ETCD_ADVERTISE_CLIENT_URLS":                    strings.Join(cfg.ClientURLs, ","),
		"ETCD_INITIAL_ADVERTISE_PEER_URLS":              strings.Join(cfg.PeerURLs, ","),
		"ETCD_LISTEN_CLIENT_URLS":                       fmt.Sprintf("https://0.0.0.0:%s", cfg.ClientPort),
		"ETCD_LISTEN_PEER_URLS":                         fmt.Sprintf("https://0.0.0.0:%s", cfg.PeerPort),
		"ETCD_AUTO_COMPACTION_RETENTION":                "1",
		"ETCD_EXPERIMENTAL_BACKEND_BBOLT_FREELIST_TYPE": "map",
		"ETCD_QUOTA_BACKEND_BYTES":                      "8589934592",
		"ETCD_LOGGER":                                   "zap",
		"ETCD_EXPERIMENTAL_INITIAL_CORRUPT_CHECK":       "true",
		"ETCD_METRICS":                                  "basic",
		"ETCD_ELECTION_TIMEOUT":                         "2000",
		"ETCD_HEARTBEAT_INTERVAL":                       "100",
		"ETCD_INITIAL_CLUSTER_TOKEN":                    "etcd_tmpfs",
		"ETCD_UNSUPPORTED_ARCH":                         "arm64",

		// TLS settings
		"ETCD_TRUSTED_CA_FILE":       filepath.Join(cfg.CertDir, "ca.pem"),
		"ETCD_CERT_FILE":             filepath.Join(cfg.CertDir, fmt.Sprintf("%s.pem", cfg.PodName)),
		"ETCD_KEY_FILE":              filepath.Join(cfg.CertDir, fmt.Sprintf("%s-key.pem", cfg.PodName)),
		"ETCD_CLIENT_CERT_AUTH":      "true",
		"ETCD_PEER_TRUSTED_CA_FILE":  filepath.Join(cfg.CertDir, "ca.pem"),
		"ETCD_PEER_CERT_FILE":        filepath.Join(cfg.CertDir, fmt.Sprintf("%s.pem", cfg.PodName)),
		"ETCD_PEER_KEY_FILE":         filepath.Join(cfg.CertDir, fmt.Sprintf("%s-key.pem", cfg.PodName)),
		"ETCD_PEER_CLIENT_CERT_AUTH": "true",

		// etcdctl configuration
		"ETCDCTL_CACERT":    filepath.Join(cfg.CertDir, "ca.pem"),
		"ETCDCTL_CERT":      filepath.Join(cfg.CertDir, fmt.Sprintf("%s.pem", cfg.PodName)),
		"ETCDCTL_KEY":       filepath.Join(cfg.CertDir, fmt.Sprintf("%s-key.pem", cfg.PodName)),
		"ETCDCTL_ENDPOINTS": fmt.Sprintf("https://127.0.0.1:%s", cfg.ClientPort),
		"TIMEOUT":           "5",
	}

	// Set all environment variables
	for key, value := range envVars {
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("failed to set env var %s: %w", key, err)
		}
	}

	klog.Infof("Environment variables configured successfully")
	return nil
}

// CreateDirectories creates required directories for etcd
func CreateDirectories(dataDir, certDir string) error {
	dirs := []string{dataDir, certDir}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
		klog.Infof("Created directory: %s", dir)
	}

	return nil
}

// WriteEnvFile writes environment variables to a file (for debugging/inspection)
func WriteEnvFile(cfg *EnvConfig, filePath string) error {
	var sb strings.Builder

	sb.WriteString("#!/bin/bash\n\n")
	sb.WriteString(fmt.Sprintf("export POD_IPS=%s\n", strings.Join(cfg.PodIPs, ",")))
	sb.WriteString(fmt.Sprintf("export LABELS=\"%s\"\n", cfg.Labels))
	sb.WriteString(fmt.Sprintf("export MAX=%d\n", cfg.Max))
	sb.WriteString(fmt.Sprintf("export PEER_PORT=%s\n", cfg.PeerPort))
	sb.WriteString(fmt.Sprintf("export SERVICE_NAME=%s\n", cfg.ServiceName))
	sb.WriteString(fmt.Sprintf("export CLIENT_PORT=%s\n", cfg.ClientPort))
	sb.WriteString(fmt.Sprintf("export NODEIP_DIR=%s\n", cfg.NodeIPDir))
	sb.WriteString(fmt.Sprintf("export POD_NAME=%s\n", cfg.PodName))
	sb.WriteString(fmt.Sprintf("export POD_NAMESPACE=%s\n", cfg.PodNamespace))
	sb.WriteString(fmt.Sprintf("export ETCD_DATA_DIR=%s\n", cfg.DataDir))
	sb.WriteString(fmt.Sprintf("export ETCD_ADVERTISE_CLIENT_URLS=%s\n", strings.Join(cfg.ClientURLs, ",")))
	sb.WriteString(fmt.Sprintf("export ETCD_INITIAL_ADVERTISE_PEER_URLS=%s\n", strings.Join(cfg.PeerURLs, ",")))
	sb.WriteString(fmt.Sprintf("export ETCD_LISTEN_CLIENT_URLS=https://0.0.0.0:%s\n", cfg.ClientPort))
	sb.WriteString(fmt.Sprintf("export ETCD_LISTEN_PEER_URLS=https://0.0.0.0:%s\n", cfg.PeerPort))
	sb.WriteString(fmt.Sprintf("export ETCD_NAME=%s\n", cfg.PodName))
	sb.WriteString("export ETCD_AUTO_COMPACTION_RETENTION=1\n")
	sb.WriteString("export ETCD_EXPERIMENTAL_BACKEND_BBOLT_FREELIST_TYPE=map\n")
	sb.WriteString("export ETCD_QUOTA_BACKEND_BYTES=8589934592\n")
	sb.WriteString("export ETCD_LOGGER=zap\n")
	sb.WriteString("export ETCD_EXPERIMENTAL_INITIAL_CORRUPT_CHECK=true\n")
	sb.WriteString("export ETCD_METRICS=basic\n")
	sb.WriteString("export ETCD_ELECTION_TIMEOUT=2000\n")
	sb.WriteString("export ETCD_HEARTBEAT_INTERVAL=100\n")
	sb.WriteString("export ETCD_INITIAL_CLUSTER_TOKEN=etcd_tmpfs\n")
	sb.WriteString(fmt.Sprintf("export ETCD_TRUSTED_CA_FILE=%s/ca.pem\n", cfg.CertDir))
	sb.WriteString(fmt.Sprintf("export ETCD_CERT_FILE=%s/%s.pem\n", cfg.CertDir, cfg.PodName))
	sb.WriteString(fmt.Sprintf("export ETCD_KEY_FILE=%s/%s-key.pem\n", cfg.CertDir, cfg.PodName))
	sb.WriteString("export ETCD_CLIENT_CERT_AUTH=true\n")
	sb.WriteString(fmt.Sprintf("export ETCD_PEER_TRUSTED_CA_FILE=%s/ca.pem\n", cfg.CertDir))
	sb.WriteString(fmt.Sprintf("export ETCD_PEER_CERT_FILE=%s/%s.pem\n", cfg.CertDir, cfg.PodName))
	sb.WriteString(fmt.Sprintf("export ETCD_PEER_KEY_FILE=%s/%s-key.pem\n", cfg.CertDir, cfg.PodName))
	sb.WriteString("export ETCD_PEER_CLIENT_CERT_AUTH=true\n")
	sb.WriteString("export ETCD_UNSUPPORTED_ARCH=arm64\n")
	sb.WriteString("export TIMEOUT=5\n")
	sb.WriteString(fmt.Sprintf("export ETCDCTL_CACERT=%s/ca.pem\n", cfg.CertDir))
	sb.WriteString(fmt.Sprintf("export ETCDCTL_CERT=%s/%s.pem\n", cfg.CertDir, cfg.PodName))
	sb.WriteString(fmt.Sprintf("export ETCDCTL_KEY=%s/%s-key.pem\n", cfg.CertDir, cfg.PodName))
	sb.WriteString(fmt.Sprintf("export ETCDCTL_ENDPOINTS=https://127.0.0.1:%s\n", cfg.ClientPort))

	if err := os.WriteFile(filePath, []byte(sb.String()), 0755); err != nil {
		return fmt.Errorf("failed to write env file: %w", err)
	}

	klog.Infof("Environment file written to: %s", filePath)
	return nil
}

// WriteReadyzScript writes the readyz health check script
func WriteReadyzScript(podName, certDir, clientPort, scriptPath string) error {
	script := fmt.Sprintf(`#!/bin/bash
# Readiness probe script for etcd
# Checks if etcd is ready to serve requests

TIMEOUT=${TIMEOUT:-5}
ETCDCTL_CERT=%s/%s.pem
ETCDCTL_KEY=%s/%s-key.pem
CLIENT_PORT=%s

timeout ${TIMEOUT} curl -k --cert ${ETCDCTL_CERT} --key ${ETCDCTL_KEY} -sf https://127.0.0.1:${CLIENT_PORT}/readyz
`,
		certDir, podName,
		certDir, podName,
		clientPort,
	)

	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		return fmt.Errorf("failed to write readyz script: %w", err)
	}

	klog.Infof("Readyz script written to: %s", scriptPath)
	return nil
}

// WriteLivezScript writes the livez health check script
func WriteLivezScript(podName, certDir, clientPort, scriptPath string) error {
	script := fmt.Sprintf(`#!/bin/bash
# Liveness probe script for etcd
# Checks if etcd process is alive

TIMEOUT=${TIMEOUT:-5}
ETCDCTL_CERT=%s/%s.pem
ETCDCTL_KEY=%s/%s-key.pem
CLIENT_PORT=%s

timeout ${TIMEOUT} curl -k --cert ${ETCDCTL_CERT} --key ${ETCDCTL_KEY} -sf https://127.0.0.1:${CLIENT_PORT}/livez
`,
		certDir, podName,
		certDir, podName,
		clientPort,
	)

	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		return fmt.Errorf("failed to write livez script: %w", err)
	}

	klog.Infof("Livez script written to: %s", scriptPath)
	return nil
}
