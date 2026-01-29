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
	AliveEndpoints map[string][]string   // podName -> []IP
	DeadNames      map[string]struct{}   // podName -> struct
}

// DiscoverEndpoints discovers alive and dead etcd endpoints
func DiscoverEndpoints(cfg *DiscoveryConfig) (*EndpointInfo, error) {
	var (
		wg         sync.WaitGroup
		mu         sync.Mutex
		fromdns    = false
	)

	info := &EndpointInfo{
		AliveEndpoints: make(map[string][]string),
		DeadNames:      make(map[string]struct{}),
	}

	// Load client cert for health checks
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load key pair: %w", err)
	}

	tlscfg := &tls.Config{
		Certificates:       []tls.Certificate{cert},
		InsecureSkipVerify: true,
	}

	// Try to get pod IPs from Kubernetes API
	podip, err := getPodIPsFromK8s(cfg.Namespace, cfg.Labels)
	if err != nil {
		klog.Errorf("Failed to read from k8s client: %v, falling back to DNS", err)
		fromdns = true
	}

	// Check each pod
	for i := 0; i < cfg.MaxNodes; i++ {
		if i == cfg.MyIndex {
			continue
		}

		var ips []string
		podName := fmt.Sprintf("%s-%d", cfg.Prefix, i)

		if !fromdns {
			hostip, ok := podip[podName]
			if !ok {
				klog.Infof("Failed to get pod '%s' ip from k8s client", podName)
			} else {
				ips, err = readIPFile(cfg.NodeIPDir, hostip)
			}
		} else {
			domain := fmt.Sprintf("%s.%s.%s.svc.cluster.local",
				podName, cfg.ServiceName, cfg.Namespace)
			ips, err = resolveDomainIPs(domain, cfg.NodeIPDir)
		}

		if err != nil || len(ips) == 0 {
			info.DeadNames[podName] = struct{}{}
			continue
		}

		wg.Add(1)
		go func(iplist []string, podname string) {
			defer wg.Done()
			var ready bool

			// Check if node is healthy
			for _, ip := range iplist {
				ipport := net.JoinHostPort(ip, cfg.ClientPort)
				if checkReadyz(ipport, tlscfg) {
					ready = true
					break
				}
			}

			mu.Lock()
			defer mu.Unlock()
			if ready {
				info.AliveEndpoints[podname] = iplist
			} else {
				info.DeadNames[podname] = struct{}{}
			}
		}(ips, podName)
	}

	wg.Wait()
	klog.Infof("Total endpoints info, alive: '%v', dead: '%v'", info.AliveEndpoints, info.DeadNames)
	return info, nil
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
		klog.Errorf("Failed to create request %s: %v", url, err)
		return false
	}

	resp, err := client.Do(req)
	if err != nil {
		klog.Errorf("Failed to check health %s", url)
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return true
	}

	klog.Warningf("Etcd node is not ready, ipport: %s, statusCode: %d", ipport, resp.StatusCode)
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
