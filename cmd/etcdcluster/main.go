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
	PodName        string
	PodNamespace   string
	ServiceName    string
	CliName        string
	Max            int
	ClientPort     string
	ClientHTTPPort string
	PeerPort       string
	DataDir        string
	CertDir        string
	NodeIPDir      string
	CAFile         string
	CAKeyFile      string
	Interfaces     []string
	Labels         string

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
	klog.Info("Loading configuration from environment variables...")
	cfg, err := loadConfig()
	if err != nil {
		klog.Errorf("Configuration validation failed: %v", err)
		klog.Error("")
		klog.Error("Required environment variables:")
		klog.Error("  POD_NAME         - Pod name (e.g., etcd-0)")
		klog.Error("  NAMESPACE    	   - Pod namespace")
		klog.Error("  SERVICE_NAME     - Headless service name")
		klog.Error("  NODEIP_DIR       - Directory containing node IP mappings")
		klog.Error("  MAX              - Maximum number of etcd nodes")
		klog.Error("  INTERFACES       - Network interfaces (comma-separated)")
		klog.Error("")
		klog.Error("Optional environment variables (with defaults):")
		klog.Error("  CLI_NAME         - Client service name (optional)")
		klog.Error("  CLIENT_PORT      - Client port (default: 2479)")
		klog.Error("  CLIENT_HTTP_PORT - Client HTTP port (default: 2481)")
		klog.Error("  PEER_PORT        - Peer port (default: 2480)")
		klog.Error("  ETCD_DATA_DIR    - Data directory (default: /run/etcd/data)")
		klog.Error("  CERT_DIR         - Certificate directory (default: /run/etcd/ssl)")
		klog.Error("  CA_FILE          - CA certificate file (default: /run/ssl/ca.pem)")
		klog.Error("  CA_KEY_FILE      - CA key file (default: /run/ssl/ca-key.pem)")
		klog.Error("  LABELS           - Pod labels (default: component=etcd)")
		klog.Fatalf("Startup aborted due to configuration errors")
	}

	// Find etcd binary
	klog.Info("Locating etcd binary...")
	ETCDBIN, err = exec.LookPath("etcd")
	if err != nil {
		klog.Fatalf("etcd binary not found in PATH. Please ensure etcd is installed and available")
	}
	klog.Infof("Found etcd binary at: %s", ETCDBIN)

	// Initialize environment
	klog.Info("Initializing environment (directories, certificates, environment variables)...")
	if err := initializeEnvironment(cfg); err != nil {
		klog.Fatalf("Failed to initialize environment: %v", err)
	}

	klog.Info("=== Initialization complete, starting cluster management ===")

	// Run cluster management loop
	runClusterLoop(cfg)
}

// loadConfig loads configuration from environment variables
func loadConfig() (*Config, error) {
	cfg := &Config{
		PodName:        os.Getenv("POD_NAME"),
		PodNamespace:   os.Getenv("NAMESPACE"),
		ServiceName:    os.Getenv("SERVICE_NAME"),
		CliName:        os.Getenv("CLI_NAME"),
		ClientPort:     getEnvOrDefault("CLIENT_PORT", "2479"),
		ClientHTTPPort: getEnvOrDefault("CLIENT_HTTP_PORT", "2481"),
		PeerPort:       getEnvOrDefault("PEER_PORT", "2480"),
		DataDir:        getEnvOrDefault("ETCD_DATA_DIR", "/run/etcd/data"),
		CertDir:        getEnvOrDefault("CERT_DIR", "/run/etcd/ssl"),
		NodeIPDir:      os.Getenv("NODEIP_DIR"),
		CAFile:         getEnvOrDefault("CA_FILE", "/run/ssl/ca.pem"),
		CAKeyFile:      getEnvOrDefault("CA_KEY_FILE", "/run/ssl/ca-key.pem"),
		Labels:         getEnvOrDefault("LABELS", "component=etcd"),
	}

	// Validate required environment variables
	if err := validateRequiredEnvVars(cfg); err != nil {
		return nil, err
	}

	// Parse MAX
	maxStr := os.Getenv("MAX")
	if maxStr == "" {
		return nil, fmt.Errorf("MAX environment variable is required")
	}
	cfg.Max = util.MustAtoi(maxStr)
	if cfg.Max <= 0 {
		return nil, fmt.Errorf("MAX must be a positive integer, got: %d", cfg.Max)
	}

	// Parse interfaces (exact names)
	interfacesStr := os.Getenv("INTERFACES")
	if interfacesStr != "" {
		cfg.Interfaces = strings.Split(interfacesStr, ",")
		// Trim spaces from interface names
		for i, iface := range cfg.Interfaces {
			cfg.Interfaces[i] = strings.TrimSpace(iface)
		}
	}

	// At least one interface configuration method is required
	if len(cfg.Interfaces) == 0 {
		return nil, fmt.Errorf("INTERFACES environment variable is required")
	}

	// Parse POD_NAME to get prefix and index
	prefix, index, err := parsePodName(cfg.PodName)
	if err != nil {
		return nil, fmt.Errorf("failed to parse POD_NAME: %w", err)
	}
	cfg.Prefix = prefix
	cfg.MyIndex = index

	// Validate CA files exist
	if err := validateCAFiles(cfg.CAFile, cfg.CAKeyFile); err != nil {
		return nil, err
	}

	// Extract IPs from network interfaces
	networkInfo, err := etcdinit.ExtractNetworkInfo(&etcdinit.NetworkConfig{
		Interfaces:     cfg.Interfaces,
		ClientPort:     cfg.ClientPort,
		ClientHTTPPort: cfg.ClientHTTPPort,
		PeerPort:       cfg.PeerPort,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to extract network info: %w", err)
	}
	cfg.MyIPs = networkInfo.IPs

	if len(cfg.MyIPs) == 0 {
		return nil, fmt.Errorf("no IP addresses found on configured interfaces")
	}

	klog.Infof("Configuration loaded successfully:")
	klog.Infof("  PodName: %s", cfg.PodName)
	klog.Infof("  PodNamespace: %s", cfg.PodNamespace)
	klog.Infof("  ServiceName: %s", cfg.ServiceName)
	klog.Infof("  Index: %d", cfg.MyIndex)
	klog.Infof("  IPs: %v", cfg.MyIPs)
	klog.Infof("  Interfaces: %v", cfg.Interfaces)
	klog.Infof("  Max nodes: %d", cfg.Max)
	return cfg, nil
}

// validateRequiredEnvVars validates that all required environment variables are set
func validateRequiredEnvVars(cfg *Config) error {
	required := map[string]string{
		"POD_NAME":      cfg.PodName,
		"POD_NAMESPACE": cfg.PodNamespace,
		"SERVICE_NAME":  cfg.ServiceName,
		"NODEIP_DIR":    cfg.NodeIPDir,
	}

	var missing []string
	for name, value := range required {
		if value == "" {
			missing = append(missing, name)
		}
	}

	if len(missing) > 0 {
		return fmt.Errorf("required environment variables are not set: %v", missing)
	}

	return nil
}

// validateCAFiles validates that CA certificate files exist
func validateCAFiles(caFile, caKeyFile string) error {
	if _, err := os.Stat(caFile); os.IsNotExist(err) {
		return fmt.Errorf("CA certificate file does not exist: %s", caFile)
	}

	if _, err := os.Stat(caKeyFile); os.IsNotExist(err) {
		return fmt.Errorf("CA key file does not exist: %s", caKeyFile)
	}

	klog.V(2).Infof("CA files validated: cert=%s, key=%s", caFile, caKeyFile)
	return nil
}

// initializeEnvironment initializes directories, certificates, and environment variables
func initializeEnvironment(cfg *Config) error {
	// Validate MyIPs
	if len(cfg.MyIPs) == 0 {
		return fmt.Errorf("no IP addresses available for initialization")
	}

	// Create directories
	if err := etcdinit.CreateDirectories(cfg.DataDir, cfg.CertDir); err != nil {
		return fmt.Errorf("failed to create directories: %w", err)
	}

	// Generate certificates with all host network IPs
	certCfg := &etcdinit.CertConfig{
		PodName:     cfg.PodName,
		Namespace:   cfg.PodNamespace,
		ServiceName: cfg.ServiceName,
		CliName:     cfg.CliName,
		IPs:         cfg.MyIPs,
		CertDir:     cfg.CertDir,
		CAFile:      cfg.CAFile,
		CAKeyFile:   cfg.CAKeyFile,
	}

	if err := etcdinit.GenerateCertificate(certCfg); err != nil {
		return fmt.Errorf("failed to generate certificate: %w", err)
	}

	// Extract network info for URLs
	networkInfo, err := etcdinit.ExtractNetworkInfo(&etcdinit.NetworkConfig{
		Interfaces:     cfg.Interfaces,
		ClientPort:     cfg.ClientPort,
		ClientHTTPPort: cfg.ClientHTTPPort,
		PeerPort:       cfg.PeerPort,
	})
	if err != nil {
		return fmt.Errorf("failed to extract network info: %w", err)
	}

	// Setup environment variables
	envCfg := &etcdinit.EnvConfig{
		PodName:        cfg.PodName,
		PodNamespace:   cfg.PodNamespace,
		PodIPs:         cfg.MyIPs,
		ServiceName:    cfg.ServiceName,
		CliName:        cfg.CliName,
		Max:            cfg.Max,
		ClientPort:     cfg.ClientPort,
		ClientHTTPPort: cfg.ClientHTTPPort,
		PeerPort:       cfg.PeerPort,
		ClientURLs:     networkInfo.ClientURLs,
		ClientHTTPURLs: networkInfo.ClientHTTPURLs,
		PeerURLs:       networkInfo.PeerURLs,
		DataDir:        cfg.DataDir,
		CertDir:        cfg.CertDir,
		NodeIPDir:      cfg.NodeIPDir,
		Labels:         cfg.Labels,
	}

	if err := etcdinit.SetupEnvironment(envCfg); err != nil {
		return fmt.Errorf("failed to setup environment: %w", err)
	}

	// Write etcdctl environment file for easy debugging
	etcdctlEnvPath := getEnvOrDefault("ETCDCTL_ENV_FILE", "/run/etcd/env")
	if err := etcdinit.WriteEtcdctlEnvFile(envCfg, etcdctlEnvPath); err != nil {
		return fmt.Errorf("failed to write etcdctl env file: %w", err)
	}

	// Write health check scripts
	readyzScriptPath := getEnvOrDefault("READYZ_SCRIPT", "/run/etcd/readyz.sh")
	if err := etcdinit.WriteReadyzScript("/run/etcd/env", readyzScriptPath); err != nil {
		return fmt.Errorf("failed to write readyz script: %w", err)
	}

	livezScriptPath := getEnvOrDefault("LIVEZ_SCRIPT", "/run/etcd/livez.sh")
	if err := etcdinit.WriteLivezScript(cfg.PodName, cfg.CertDir, cfg.ClientHTTPPort, livezScriptPath); err != nil {
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

	retryCount := 0

	for {
		retryCount++
		klog.Infof("=== Cluster management loop iteration %d ===", retryCount)

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
			continue
		}

		switch len(endpointInfo.AliveEndpoints) {
		case 0:
			// No alive endpoints
			if cfg.MyIndex != 0 {
				klog.Warningf("No alive endpoints found for '%s', waiting for pod-0 to initialize cluster", cfg.PodName)
				break
			}
			// Initialize new cluster (only pod-0)
			klog.Info("No alive endpoints and I'm pod-0, initializing new cluster")
			cmd, err := clusterMgr.InitializeNewCluster(cfg.MyIPs, ETCDBIN)
			if err != nil {
				klog.Errorf("Failed to initialize new cluster: %v", err)
				break
			}

			// Start brain split checker for pod-0
			brainSplitCfg := &cluster.BrainSplitCheckConfig{
				NodeIPDir:   cfg.NodeIPDir,
				ClientPort:  cfg.ClientPort,
				CertFile:    fmt.Sprintf("%s/%s.pem", cfg.CertDir, cfg.PodName),
				KeyFile:     fmt.Sprintf("%s/%s-key.pem", cfg.CertDir, cfg.PodName),
				MyPodName:   cfg.PodName,
				MyIPs:       cfg.MyIPs,
				CheckPeriod: 5 * time.Second,
				MaxChecks:   3,
			}
			waitExitWithBrainSplitCheck(cmd, clusterMgr, brainSplitCfg)

		default:
			// Join existing cluster
			klog.Infof("Found %d alive endpoints, attempting to join cluster", len(endpointInfo.AliveEndpoints))
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

// waitExitWithBrainSplitCheck waits for etcd process to exit, signal, or brain split detection
func waitExitWithBrainSplitCheck(cmd *exec.Cmd, clusterMgr *cluster.Manager, brainSplitCfg *cluster.BrainSplitCheckConfig) {
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)

	processExitChan := make(chan bool, 1)
	stopBrainSplitCheck := make(chan struct{})

	go func() {
		err := cmd.Wait()
		if err != nil {
			klog.Infof("Process exited with error: %v", err)
		} else {
			klog.Info("Process exited successfully")
		}
		processExitChan <- true
	}()

	// Start brain split checker
	brainSplitResultCh := clusterMgr.StartBrainSplitChecker(brainSplitCfg, stopBrainSplitCheck)

	klog.Info("Main process waiting for signal, subprocess exit, or brain split detection...")
	select {
	case s := <-signalChan:
		close(stopBrainSplitCheck)
		klog.Infof("Main process received signal: %v, preparing to exit", s)
		if err := cmd.Process.Kill(); err != nil {
			klog.Fatalf("Failed to kill subprocess: %v", err)
		}
		klog.Info("Subprocess killed")
	case <-processExitChan:
		close(stopBrainSplitCheck)
		klog.Info("Subprocess exited, main process preparing to exit")
	case result := <-brainSplitResultCh:
		if result.BrainSplitDetected {
			klog.Warningf("Brain split detected: %s", result.Reason)
			klog.Warning("Terminating etcd to rejoin the existing cluster...")
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
				klog.Errorf("Failed to send SIGTERM to etcd process: %v", err)
				if err := cmd.Process.Kill(); err != nil {
					klog.Fatalf("Failed to kill subprocess: %v", err)
				}
			}
			// Wait for process to exit
			<-processExitChan
			klog.Info("Etcd process terminated due to brain split, will restart and rejoin cluster")
			// Return instead of os.Exit to allow the main loop to retry
			return
		}
		klog.Info("Brain split checker completed successfully, no brain split detected")
		// Continue waiting for normal exit
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
