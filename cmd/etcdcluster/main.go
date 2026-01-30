package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/yylt/etcdauto/pkg/cluster"
	etcdinit "github.com/yylt/etcdauto/pkg/init"
	"github.com/yylt/etcdauto/pkg/util"

	"k8s.io/klog/v2"
)

var (
	ETCDBIN string
)

// Config holds all configuration for etcdcluster
type Config struct {
	// From environment
	PodName           string
	PodNamespace      string
	ServiceName       string
	CliName           string
	Max               int
	ClientPort        string
	PeerPort          string
	DataDir           string
	CertDir           string
	NodeIPDir         string
	CAFile            string
	CAKeyFile         string
	Interfaces        []string
	InterfacePrefixes []string // Network interface prefixes to match
	Labels            string

	// Parsed
	Prefix  string
	MyIndex int
	MyIPs   []string
}

// printBuildInfo prints version and VCS information
func printBuildInfo() {
	if info, ok := debug.ReadBuildInfo(); ok {
		var vcsRevision, vcsTime, vcsModified string
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				vcsRevision = setting.Value
			case "vcs.time":
				vcsTime = setting.Value
			case "vcs.modified":
				vcsModified = setting.Value
			}
		}

		if vcsRevision != "" {
			vcsRevision = vcsRevision[:8]
			klog.Infof("Build information, reversion: %s, time: %s, modified: %s", vcsRevision, vcsTime, vcsModified)
		}
	}
}

func main() {
	printBuildInfo()

	// Load configuration from environment
	cfg, err := loadConfig()
	if err != nil {
		klog.Fatalf("Failed to load configuration: %v", err)
	}

	// Find etcd binary
	ETCDBIN, err = exec.LookPath("etcd")
	if err != nil {
		klog.Fatal("etcd binary not found")
	}

	// Initialize environment
	if err := initializeEnvironment(cfg); err != nil {
		klog.Fatalf("Failed to initialize environment: %v", err)
	}

	// Run cluster management loop
	runClusterLoop(cfg)
}

// loadConfig loads configuration from environment variables
func loadConfig() (*Config, error) {
	cfg := &Config{
		PodName:      os.Getenv("POD_NAME"),
		PodNamespace: os.Getenv("POD_NAMESPACE"),
		ServiceName:  os.Getenv("SERVICE_NAME"),
		CliName:      os.Getenv("CLI_NAME"),
		ClientPort:   getEnvOrDefault("CLIENT_PORT", "2479"),
		PeerPort:     getEnvOrDefault("PEER_PORT", "2480"),
		DataDir:      getEnvOrDefault("ETCD_DATA_DIR", "/run/etcd/data"),
		CertDir:      getEnvOrDefault("CERT_DIR", "/run/etcd/ssl"),
		NodeIPDir:    os.Getenv("NODEIP_DIR"),
		CAFile:       getEnvOrDefault("CA_FILE", "/run/ssl/ca.pem"),
		CAKeyFile:    getEnvOrDefault("CA_KEY_FILE", "/run/ssl/ca-key.pem"),
		Labels:       getEnvOrDefault("LABELS", "component=etcd"),
	}

	// Parse MAX
	maxStr := os.Getenv("MAX")
	if maxStr == "" {
		return nil, fmt.Errorf("MAX environment variable is required")
	}
	cfg.Max = util.MustAtoi(maxStr)

	// Parse interfaces (exact names)
	interfacesStr := os.Getenv("INTERFACES")
	if interfacesStr != "" {
		cfg.Interfaces = strings.Split(interfacesStr, ",")
	}

	// Parse interface prefixes
	interfacePrefixesStr := os.Getenv("INTERFACE_PREFIXES")
	if interfacePrefixesStr != "" {
		cfg.InterfacePrefixes = strings.Split(interfacePrefixesStr, ",")
	}

	// At least one interface configuration method is required
	if len(cfg.Interfaces) == 0 && len(cfg.InterfacePrefixes) == 0 {
		return nil, fmt.Errorf("either INTERFACES or INTERFACE_PREFIXES environment variable is required")
	}

	// Parse POD_NAME to get prefix and index
	prefix, index, err := parsePodName(cfg.PodName)
	if err != nil {
		return nil, fmt.Errorf("failed to parse POD_NAME: %w", err)
	}
	cfg.Prefix = prefix
	cfg.MyIndex = index

	// Extract IPs from network interfaces
	networkInfo, err := etcdinit.ExtractNetworkInfo(&etcdinit.NetworkConfig{
		Interfaces:        cfg.Interfaces,
		InterfacePrefixes: cfg.InterfacePrefixes,
		ClientPort:        cfg.ClientPort,
		PeerPort:          cfg.PeerPort,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to extract network info: %w", err)
	}
	cfg.MyIPs = networkInfo.IPs

	klog.Infof("Configuration loaded: PodName=%s, Index=%d, IPs=%v", cfg.PodName, cfg.MyIndex, cfg.MyIPs)
	return cfg, nil
}

// initializeEnvironment initializes directories, certificates, and environment variables
func initializeEnvironment(cfg *Config) error {
	// Create directories
	if err := etcdinit.CreateDirectories(cfg.DataDir, cfg.CertDir); err != nil {
		return fmt.Errorf("failed to create directories: %w", err)
	}

	// Generate certificates
	certCfg := &etcdinit.CertConfig{
		PodName:     cfg.PodName,
		Namespace:   cfg.PodNamespace,
		ServiceName: cfg.ServiceName,
		CliName:     cfg.CliName,
		PodIP:       cfg.MyIPs[0],
		ExtraIPs:    cfg.MyIPs[1:],
		CertDir:     cfg.CertDir,
		CAFile:      cfg.CAFile,
		CAKeyFile:   cfg.CAKeyFile,
	}

	if err := etcdinit.GenerateCertificate(certCfg); err != nil {
		return fmt.Errorf("failed to generate certificate: %w", err)
	}

	// Extract network info for URLs
	networkInfo, err := etcdinit.ExtractNetworkInfo(&etcdinit.NetworkConfig{
		Interfaces:        cfg.Interfaces,
		InterfacePrefixes: cfg.InterfacePrefixes,
		ClientPort:        cfg.ClientPort,
		PeerPort:          cfg.PeerPort,
	})
	if err != nil {
		return fmt.Errorf("failed to extract network info: %w", err)
	}

	// Setup environment variables
	envCfg := &etcdinit.EnvConfig{
		PodName:      cfg.PodName,
		PodNamespace: cfg.PodNamespace,
		PodIPs:       cfg.MyIPs,
		ServiceName:  cfg.ServiceName,
		CliName:      cfg.CliName,
		Max:          cfg.Max,
		ClientPort:   cfg.ClientPort,
		PeerPort:     cfg.PeerPort,
		ClientURLs:   networkInfo.ClientURLs,
		PeerURLs:     networkInfo.PeerURLs,
		DataDir:      cfg.DataDir,
		CertDir:      cfg.CertDir,
		NodeIPDir:    cfg.NodeIPDir,
		Labels:       cfg.Labels,
	}

	if err := etcdinit.SetupEnvironment(envCfg); err != nil {
		return fmt.Errorf("failed to setup environment: %w", err)
	}

	// Write health check scripts
	readyzScriptPath := getEnvOrDefault("READYZ_SCRIPT", "/run/etcd/readyz.sh")
	if err := etcdinit.WriteReadyzScript(cfg.PodName, cfg.CertDir, cfg.ClientPort, readyzScriptPath); err != nil {
		return fmt.Errorf("failed to write readyz script: %w", err)
	}

	livezScriptPath := getEnvOrDefault("LIVEZ_SCRIPT", "/run/etcd/livez.sh")
	if err := etcdinit.WriteLivezScript(cfg.PodName, cfg.CertDir, cfg.ClientPort, livezScriptPath); err != nil {
		return fmt.Errorf("failed to write livez script: %w", err)
	}

	klog.Info("Environment initialized successfully")
	return nil
}

// runClusterLoop runs the main cluster management loop
func runClusterLoop(cfg *Config) {
	clusterMgr := cluster.NewManager(&cluster.Config{
		PodName:    cfg.PodName,
		PeerPort:   cfg.PeerPort,
		ClientPort: cfg.ClientPort,
		DataDir:    cfg.DataDir,
		CertFile:   fmt.Sprintf("%s/%s.pem", cfg.CertDir, cfg.PodName),
		KeyFile:    fmt.Sprintf("%s/%s-key.pem", cfg.CertDir, cfg.PodName),
	})

	for {
		// Discover endpoints
		discoveryCfg := &cluster.DiscoveryConfig{
			Prefix:      cfg.Prefix,
			MaxNodes:    cfg.Max,
			MyIndex:     cfg.MyIndex,
			ClientPort:  cfg.ClientPort,
			Namespace:   cfg.PodNamespace,
			ServiceName: cfg.ServiceName,
			NodeIPDir:   cfg.NodeIPDir,
			Labels:      cfg.Labels,
			CertFile:    fmt.Sprintf("%s/%s.pem", cfg.CertDir, cfg.PodName),
			KeyFile:     fmt.Sprintf("%s/%s-key.pem", cfg.CertDir, cfg.PodName),
		}

		endpointInfo, err := cluster.DiscoverEndpoints(discoveryCfg)
		if err != nil {
			klog.Errorf("Failed to discover endpoints: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}

		switch len(endpointInfo.AliveEndpoints) {
		case 0:
			// No alive endpoints
			if cfg.MyIndex != 0 {
				klog.Warningf("Failed to get '%s' endpoints, retrying...", cfg.PodName)
				break
			}

			// Initialize new cluster (only pod-0)
			cmd, err := clusterMgr.InitializeNewCluster(cfg.MyIPs, ETCDBIN)
			if err != nil {
				klog.Errorf("Failed to initialize new cluster: %v", err)
				break
			}
			waitExit(cmd)

		default:
			// Join existing cluster
			client, err := clusterMgr.GetClient(endpointInfo.AliveEndpoints)
			if err != nil {
				klog.Errorf("Failed to get etcd client: %v", err)
				break
			}

			cmd, err := clusterMgr.JoinExistingCluster(client, cfg.MyIPs, endpointInfo.DeadNames, ETCDBIN)
			if err == nil {
				waitExit(cmd)
			} else {
				klog.Errorf("Failed to join existing cluster: %v", err)
			}
		}

		time.Sleep(2 * time.Second)
	}
}

// parsePodName parses pod name to extract prefix and index
func parsePodName(podName string) (string, int, error) {
	re := regexp.MustCompile(`^(.*)-(\d+)$`)
	matches := re.FindStringSubmatch(podName)
	if len(matches) != 3 {
		return "", 0, fmt.Errorf("invalid POD_NAME format: %s", podName)
	}

	prefix := matches[1]
	index, err := strconv.Atoi(matches[2])
	if err != nil {
		return "", 0, fmt.Errorf("invalid index in POD_NAME: %s", podName)
	}

	klog.InfoS("Parsed POD_NAME", "prefix", prefix, "index", index)
	return prefix, index, nil
}

// waitExit waits for etcd process to exit or signal
func waitExit(cmd *exec.Cmd) {
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)

	processExitChan := make(chan bool, 1)

	go func() {
		err := cmd.Wait()
		if err != nil {
			klog.Infof("Process exited with error: %v", err)
		} else {
			klog.Info("Process exited successfully")
		}
		processExitChan <- true
	}()

	klog.Info("Main process waiting for signal or subprocess exit...")
	select {
	case s := <-signalChan:
		klog.Infof("Main process received signal: %v, preparing to exit", s)
		if err := cmd.Process.Kill(); err != nil {
			klog.Fatalf("Failed to kill subprocess: %v", err)
		}
		klog.Info("Subprocess killed")
	case <-processExitChan:
		klog.Info("Subprocess exited, main process preparing to exit")
	}

	klog.Info("Main process exiting")
	os.Exit(0)
}

// getEnvOrDefault gets environment variable or returns default value
func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
