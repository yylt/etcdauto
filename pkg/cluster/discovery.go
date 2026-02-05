package cluster

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/yylt/etcdauto/pkg/util"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
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

// EndpointInfo holds information about discovered endpoints
type EndpointInfo struct {
	AliveEndpoints map[string][]string // podName -> []IP
	DeadNames      map[string]struct{} // podName -> struct
}

// DiscoverEndpoints discovers alive and dead etcd endpoints
func DiscoverEndpoints(cfg *DiscoveryConfig) (*EndpointInfo, error) {
	klog.Infof("Starting endpoint discovery: prefix=%s, maxNodes=%d, myIndex=%d",
		cfg.Prefix, cfg.MaxNodes, cfg.MyIndex)

	info := &EndpointInfo{
		AliveEndpoints: make(map[string][]string),
		DeadNames:      make(map[string]struct{}),
	}

	// Load client cert for health checks
	klog.V(2).Infof("Loading TLS config from cert: %s, key: %s", cfg.CertFile, cfg.KeyFile)
	tlscfg, err := loadTLSConfig(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load TLS config: %w", err)
	}
	klog.V(2).Info("TLS config loaded successfully")

	// Try to get pod IPs from Kubernetes API
	klog.V(2).Infof("Fetching pod IP mapping from Kubernetes API: namespace=%s, labels=%s",
		cfg.Namespace, cfg.Labels)
	podip, fromdns := getPodIPMapping(cfg.Namespace, cfg.Labels)
	if fromdns {
		klog.Info("Using DNS-based discovery (Kubernetes API unavailable)")
	} else {
		klog.Infof("Using Kubernetes API-based discovery, found %d pods", len(podip))
	}

	// Check each pod
	checkPods(cfg, info, tlscfg, podip, fromdns)

	klog.Infof("Endpoint discovery complete: %d alive, %d dead",
		len(info.AliveEndpoints), len(info.DeadNames))
	klog.V(2).Infof("Alive endpoints: %v", info.AliveEndpoints)
	klog.V(2).Infof("Dead endpoints: %v", getMapKeys(info.DeadNames))
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

// getPodIPMapping gets pod IP mapping from Kubernetes API or falls back to DNS
func getPodIPMapping(namespace, labels string) (map[string]string, bool) {
	podip, err := getPodIPsFromK8s(namespace, labels)
	if err != nil {
		klog.Errorf("Failed to read from k8s client: %v, falling back to DNS", err)
		return nil, true
	}
	return podip, false
}

// checkPods checks health of all pods in the cluster
func checkPods(cfg *DiscoveryConfig, info *EndpointInfo, tlscfg *tls.Config, podip map[string]string, fromdns bool) {
	var wg sync.WaitGroup
	var mu sync.Mutex

	for i := 0; i < cfg.MaxNodes; i++ {
		if i == cfg.MyIndex {
			continue
		}

		podName := fmt.Sprintf("%s-%d", cfg.Prefix, i)
		ips := getPodIPs(podName, cfg, podip, fromdns)

		if len(ips) == 0 {
			info.DeadNames[podName] = struct{}{}
			continue
		}

		wg.Add(1)
		go checkPodHealth(podName, ips, cfg.ClientPort, tlscfg, info, &mu, &wg)
	}

	wg.Wait()
}

// getPodIPs gets IP addresses for a pod
func getPodIPs(podName string, cfg *DiscoveryConfig, podip map[string]string, fromdns bool) []string {
	var ips []string
	var err error

	if !fromdns {
		hostip, ok := podip[podName]
		if !ok {
			klog.Infof("Failed to get pod '%s' ip from k8s client", podName)
			return nil
		}
		ips, err = readIPFile(cfg.NodeIPDir, hostip)
	} else {
		domain := fmt.Sprintf("%s.%s.%s.svc.cluster.local",
			podName, cfg.ServiceName, cfg.Namespace)
		ips, err = resolveDomainIPs(domain, cfg.NodeIPDir)
	}

	if err != nil {
		return nil
	}
	return ips
}

// checkPodHealth checks if a pod is healthy
func checkPodHealth(podname string, iplist []string, clientPort string, tlscfg *tls.Config, info *EndpointInfo, mu *sync.Mutex, wg *sync.WaitGroup) {
	defer wg.Done()
	var ready bool

	klog.V(2).Infof("Checking health for pod %s with IPs: %v", podname, iplist)

	// Check if node is healthy
	for _, ip := range iplist {
		ipport := net.JoinHostPort(ip, clientPort)
		if checkReadyz(ipport, tlscfg) {
			klog.V(2).Infof("Pod %s is healthy at %s", podname, ipport)
			ready = true
			break
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if ready {
		info.AliveEndpoints[podname] = iplist
		klog.Infof("Pod %s marked as alive with IPs: %v", podname, iplist)
	} else {
		info.DeadNames[podname] = struct{}{}
		klog.Infof("Pod %s marked as dead", podname)
	}
}

// getMapKeys returns keys from a map as a slice
func getMapKeys(m map[string]struct{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// getPodIPsFromK8s retrieves pod IPs from Kubernetes API
func getPodIPsFromK8s(namespace, labels string) (map[string]string, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}

	clientset := kubernetes.NewForConfigOrDie(config)
	pods, err := clientset.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{
		ResourceVersion: "0",
		LabelSelector:   labels,
	})
	if err != nil {
		return nil, err
	}

	podip := make(map[string]string)
	for _, pod := range pods.Items {
		podip[pod.Name] = pod.Status.HostIP
	}

	return podip, nil
}

// resolveDomainIPs resolves domain to IPs and reads IP file
func resolveDomainIPs(domain, nodeIPDir string) ([]string, error) {
	ips, err := net.LookupHost(domain)
	if err != nil {
		klog.Errorf("Failed to resolve domain %s: %v", domain, err)
		return nil, err
	}

	if len(ips) == 0 {
		return nil, fmt.Errorf("no IPs found for domain %s", domain)
	}

	iplist, err := readIPFile(nodeIPDir, ips[0])
	klog.Infof("Resolved domain %s, iplist: %s", domain, strings.Join(iplist, ","))
	return iplist, err
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
func checkReadyz(ipport string, tlscfg *tls.Config) bool {
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

// GetAllDiscoveredIPs returns all discovered IPs from endpoint info
func GetAllDiscoveredIPs(info *EndpointInfo) []string {
	ipSet := make(map[string]bool)
	for _, iplist := range info.AliveEndpoints {
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
