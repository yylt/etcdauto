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
	ClientPort     string
	ClientHTTPPort string
	PeerPort       string
	ClientURLs     []string
	ClientHTTPURLs []string
	PeerURLs       []string

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
		"CLIENT_PORT":      cfg.ClientPort,
		"CLIENT_HTTP_PORT": cfg.ClientHTTPPort,
		"PEER_PORT":        cfg.PeerPort,

		// Directory configuration
		"NODEIP_DIR": cfg.NodeIPDir,

		// ETCD configuration
		"ETCD_DATA_DIR":                    cfg.DataDir,
		"ETCD_NAME":                        cfg.PodName,
		"ETCD_LOGGER":                      "zap",
		"ETCD_METRICS":                     "basic",
		"ETCD_ELECTION_TIMEOUT":            "2000",
		"ETCD_HEARTBEAT_INTERVAL":          "200",
		"ETCD_MAX_SNAPSHOTS":               "5",
		"ETCD_MAX_WALS":                    "10",
		"ETCD_ADVERTISE_CLIENT_URLS":       strings.Join(cfg.ClientURLs, ","),
		"ETCD_INITIAL_ADVERTISE_PEER_URLS": strings.Join(cfg.PeerURLs, ","),
		"ETCD_LISTEN_CLIENT_URLS":          fmt.Sprintf("https://0.0.0.0:%s", cfg.ClientPort),
		"ETCD_LISTEN_PEER_URLS":            fmt.Sprintf("https://0.0.0.0:%s", cfg.PeerPort),
		// "ETCD_LISTEN_CLIENT_HTTP_URLS":                      fmt.Sprintf("https://127.0.0.1:%s", cfg.ClientHTTPPort),
		"ETCD_AUTO_COMPACTION_MODE":                         "periodic",
		"ETCD_AUTO_COMPACTION_RETENTION":                    "5m",
		"ETCD_BACKEND_BBOLT_FREELIST_TYPE":                  "map",
		"ETCD_QUOTA_BACKEND_BYTES":                          "8589934592", // 8GB
		"ETCD_INITIAL_CLUSTER_TOKEN":                        "etcd_tmpfs",
		"ETCD_EXPERIMENTAL_INITIAL_CORRUPT_CHECK":           "true",
		"ETCD_EXPERIMENTAL_ENABLE_LEASE_CHECKPOINT":         "true",
		"ETCD_EXPERIMENTAL_ENABLE_LEASE_CHECKPOINT_PERSIST": "true",

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

// WriteEtcdctlEnvFile writes etcdctl environment variables to a file for easy debugging
// Users can source this file to use etcdctl: source /run/etcd/env && etcdctl member list
func WriteEtcdctlEnvFile(cfg *EnvConfig, filePath string) error {
	var sb strings.Builder

	sb.WriteString("#!/bin/bash\n")
	sb.WriteString("# etcdctl environment configuration\n")
	sb.WriteString("# Usage: source /run/etcd/env && etcdctl member list\n\n")

	// etcdctl required environment variables
	sb.WriteString("export ETCDCTL_API=3\n")
	sb.WriteString(fmt.Sprintf("export ETCDCTL_CACERT=%s/ca.pem\n", cfg.CertDir))
	sb.WriteString(fmt.Sprintf("export ETCDCTL_CERT=%s/%s.pem\n", cfg.CertDir, cfg.PodName))
	sb.WriteString(fmt.Sprintf("export ETCDCTL_KEY=%s/%s-key.pem\n", cfg.CertDir, cfg.PodName))
	sb.WriteString(fmt.Sprintf("export ETCDCTL_ENDPOINTS=https://127.0.0.1:%s\n", cfg.ClientPort))

	// Write all client URLs as comment for reference
	if len(cfg.ClientURLs) > 0 {
		sb.WriteString(fmt.Sprintf("\n# All cluster endpoints: %s\n", strings.Join(cfg.ClientURLs, ",")))
	}

	if err := os.WriteFile(filePath, []byte(sb.String()), 0644); err != nil {
		return fmt.Errorf("failed to write etcdctl env file: %w", err)
	}

	klog.Infof("etcdctl environment file written to: %s", filePath)
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

// WriteReadyzScript writes the readyz health check script
func WriteReadyzScript(envpath, scriptPath string) error {
	script := fmt.Sprintf(`#!/bin/bash
# Readiness probe script for etcd

source %s

timeout 2 etcdctl --insecure-skip-tls-verify endpoint health
`,
		envpath,
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
