package cluster

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/yylt/etcdauto/pkg/util"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	etcdcli "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap"
	"k8s.io/klog/v2"
)

// DiscoveryConfig holds configuration for endpoint discovery
type DiscoveryConfig struct {
	Prefix      string
	MaxNodes    int
	MyIndex     int
	ClientPort  string
	Namespace   string
	ServiceName string
	NodeIPDir   string
	Labels      string
	CertFile    string
	KeyFile     string
}

// EndpointHealth represents the health status of an endpoint
type EndpointHealth int

const (
	// HealthUnreachable: cannot connect — network issues only (TLS handshake failure, connection refused, timeout)
	HealthUnreachable EndpointHealth = iota
	// HealthHealthy: fully healthy — Get succeeded and no alarms
	HealthHealthy
	// HealthNotReady: connected but etcd not ready (NoLeader, starting, NOSPACE, other alarms)
	HealthNotReady
)

// EndpointInfo holds information about discovered endpoints
type EndpointInfo struct {
	ReadyNodes    map[string][]string // masterIP -> []IP (HealthHealthy)
	NotReadyNodes map[string]struct{} // masterIP -> struct{} (HealthNotReady)
	DeadNames     map[string]struct{} // masterIP -> struct{} (file read errors etc.)
}

// DiscoverEndpoints discovers alive and dead etcd endpoints from nodeIPDir
func DiscoverEndpoints(cfg *DiscoveryConfig) (*EndpointInfo, error) {
	klog.Infof("Starting endpoint discovery from nodeIPDir: %s", cfg.NodeIPDir)

	info := &EndpointInfo{
		ReadyNodes:    make(map[string][]string),
		NotReadyNodes: make(map[string]struct{}),
		DeadNames:     make(map[string]struct{}),
	}
	// Load client cert
	tlscfg, err := loadTLSConfig(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load TLS config: %w", err)
	}

	// Read all IP files from nodeIPDir
	entries, err := os.ReadDir(cfg.NodeIPDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read nodeIPDir %s: %w", cfg.NodeIPDir, err)
	}

	if len(entries) == 0 {
		klog.Warningf("nodeIPDir %s is empty", cfg.NodeIPDir)
		return info, nil
	}

	// Check each master IP file concurrently
	var wg sync.WaitGroup
	var mu sync.Mutex

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		masterIP := entry.Name()
		if net.ParseIP(masterIP) == nil {
			continue
		}
		wg.Add(1)
		go func(masterIP string) {
			defer wg.Done()

			// Read IP list from file
			ips, err := readIPFile(cfg.NodeIPDir, masterIP)
			if err != nil {
				klog.Errorf("Failed to read IP file %s: %v", masterIP, err)
				mu.Lock()
				info.DeadNames[masterIP] = struct{}{}
				mu.Unlock()
				return
			}

			// Check health of this master IP's endpoints
			health := checkEndpointHealth(ips, cfg.ClientPort, tlscfg)

			mu.Lock()
			switch health {
			case HealthHealthy:
				info.ReadyNodes[masterIP] = ips
				klog.Infof("Master IP %s marked as ready with IPs: %v", masterIP, ips)
			case HealthNotReady:
				info.NotReadyNodes[masterIP] = struct{}{}
				klog.Infof("Master IP %s marked as not-ready", masterIP)
			case HealthUnreachable:
				klog.Infof("Master IP %s is unreachable", masterIP)
			}
			mu.Unlock()
		}(masterIP)
	}

	wg.Wait()

	klog.Infof("Endpoint discovery complete: %d ready, %d not-ready",
		len(info.ReadyNodes), len(info.NotReadyNodes))
	klog.V(2).Infof("Ready nodes: %v, Not-ready nodes: %v", info.ReadyNodes, info.NotReadyNodes)
	return info, nil
}

// loadTLSConfig loads TLS configuration from cert and key files
func loadTLSConfig(certFile, keyFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load key pair: %w", err)
	}

	return &tls.Config{
		Certificates:       []tls.Certificate{cert},
		InsecureSkipVerify: true,
	}, nil
}

// checkEndpointHealth checks if any endpoint in the IP list is healthy
// Returns EndpointHealth — the best status among all IPs (Healthy > NotReady > Unreachable)
func checkEndpointHealth(iplist []string, clientPort string, tlscfg *tls.Config) EndpointHealth {
	if len(iplist) == 0 {
		klog.Warning("Empty IP list provided for health check")
		return HealthUnreachable
	}

	var (
		ctx = context.Background()

		wg     sync.WaitGroup
		health = make(chan EndpointHealth, len(iplist))
	)
	for _, ip := range iplist {
		wg.Add(1)
		go func(ip string) {
			defer wg.Done()
			health <- checkSingleEndpoint(ctx, net.JoinHostPort(ip, clientPort), tlscfg)
		}(ip)
	}
	go func() {
		wg.Wait()
		close(health)
	}()

	bestStatus := HealthUnreachable
	for h := range health {
		if h == HealthHealthy {
			return HealthHealthy
		}
		if h == HealthNotReady {
			bestStatus = HealthNotReady
		}
	}

	return bestStatus
}

// checkSingleEndpoint probes a single etcd endpoint and returns its health state.
//
// Classification logic:
//   - TCP port unreachable (2s dial timeout)           → HealthUnreachable
//   - TCP reachable, Get+AlarmList OK (3s context)     → HealthHealthy
//   - TCP reachable, but Get failed / alarms present   → HealthNotReady
//
// Timeout rationale (all intra-node / same LAN, worst case ~1ms RTT):
//   - 2s TCP dial: generous enough for overloaded host, tight enough to not stall discovery
//   - 3s API context: covers Get → 2 RTT + AlarmList → 2 RTT = 4 RTT, ample overhead
func checkSingleEndpoint(ctx context.Context, endpoint string, tlscfg *tls.Config) EndpointHealth {
	// Phase 1: TCP dial — cheap, separates "network down" from "etcd not ready"
	conn, err := net.DialTimeout("tcp", endpoint, 2*time.Second)
	if err != nil {
		return HealthUnreachable
	}
	conn.Close()

	// Phase 2: TCP is reachable — probe etcd API
	// DialTimeout is omitted from client config: TCP connectivity already verified.
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	client, err := etcdcli.New(etcdcli.Config{
		Endpoints: []string{endpoint},
		TLS:       tlscfg,
		Logger:    zap.NewNop(),
	})
	if err != nil {
		return HealthNotReady
	}
	defer client.Close()

	_, err = client.Get(ctx, "health")
	if err == nil || errors.Is(err, rpctypes.ErrPermissionDenied) {
		resp, err := client.AlarmList(ctx)
		if err == nil && len(resp.Alarms) == 0 {
			return HealthHealthy
		}
		return HealthNotReady
	}
	return HealthNotReady
}

// readIPFile reads IP addresses from a file in nodeIPDir
func readIPFile(nodeIPDir, ip string) ([]string, error) {
	ipFile := filepath.Join(nodeIPDir, ip)
	if !util.FileExists(ipFile) {
		klog.Warningf("IP file not found, using resolved IP directly: %s", ipFile)
		return []string{ip}, nil
	}

	content, err := os.ReadFile(ipFile)
	if err != nil {
		klog.Errorf("Failed to read IP file %s: %v", ip, err)
		return nil, err
	}

	ipList := strings.Split(strings.TrimSpace(string(content)), ",")
	klog.Infof("Read IPs from file %s: %v", ip, ipList)
	return ipList, nil
}

// checkReadyz checks if etcd endpoint is ready
func CheckReadyz(ipport string, tlscfg *tls.Config) bool {
	url := fmt.Sprintf("https://%s/readyz", ipport)

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: tlscfg,
		},
		Timeout: 2 * time.Second,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		klog.V(2).Infof("Failed to create request for %s: %v", url, err)
		return false
	}

	resp, err := client.Do(req)
	if err != nil {
		klog.V(2).Infof("Health check failed for %s: %v", ipport, err)
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		klog.V(2).Infof("Health check passed for %s", ipport)
		return true
	}

	klog.V(2).Infof("Health check failed for %s: status code %d", ipport, resp.StatusCode)
	return false
}

// BuildPeerEndpoints builds peer endpoint strings
func BuildPeerEndpoints(endpoints map[string][]string, port string, withName, withScheme bool) []string {
	var addresses []string
	for k, iplist := range endpoints {
		for _, ip := range iplist {
			if withScheme {
				ip = fmt.Sprintf("https://%s", ip)
			}
			if withName {
				addresses = append(addresses, fmt.Sprintf("%s=%s:%s", k, ip, port))
			} else {
				addresses = append(addresses, fmt.Sprintf("%s:%s", ip, port))
			}
		}
	}
	return addresses
}

// GetAllDiscoveredIPs returns all discovered ready IPs from endpoint info
func GetAllDiscoveredIPs(info *EndpointInfo) []string {
	ipSet := make(map[string]bool)
	for _, iplist := range info.ReadyNodes {
		for _, ip := range iplist {
			ipSet[ip] = true
		}
	}

	ips := make([]string, 0, len(ipSet))
	for ip := range ipSet {
		ips = append(ips, ip)
	}
	return ips
}
