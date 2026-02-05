package init

import (
	"fmt"
	"net"
	"strings"

	"k8s.io/klog/v2"
)

// NetworkConfig holds network interface configuration
type NetworkConfig struct {
	Interfaces []string // Network interfaces to extract IPs from (exact names)
	ClientPort string   // Client port for etcd
	PeerPort   string   // Peer port for etcd
}

// NetworkInfo holds extracted network information
type NetworkInfo struct {
	IPs        []string // All IP addresses from interfaces
	PeerURLs   []string // Peer URLs (https://ip:peerPort)
	ClientURLs []string // Client URLs (https://ip:clientPort)
}

// ExtractNetworkInfo extracts IP addresses from network interfaces
func ExtractNetworkInfo(cfg *NetworkConfig) (*NetworkInfo, error) {
	if len(cfg.Interfaces) == 0 {
		return nil, fmt.Errorf("no interfaces specified")
	}

	info := &NetworkInfo{
		IPs:        make([]string, 0),
		PeerURLs:   make([]string, 0),
		ClientURLs: make([]string, 0),
	}

	extractIPsFromInterfaces(cfg, info)

	if len(info.IPs) == 0 {
		return nil, fmt.Errorf("no IPv4 addresses found on matching interfaces")
	}

	return info, nil
}

// extractIPsFromInterfaces extracts IPs from the given interfaces
func extractIPsFromInterfaces(cfg *NetworkConfig, info *NetworkInfo) {
	for _, ifaceName := range cfg.Interfaces {
		ipStr, err := GetInterfaceIP(ifaceName)
		if err != nil {
			klog.Warningf("Failed to get interface %s: %v", ifaceName, err)
			continue
		}

		info.IPs = append(info.IPs, ipStr)

		// Build URLs
		if cfg.PeerPort != "" {
			info.PeerURLs = append(info.PeerURLs, fmt.Sprintf("https://%s:%s", ipStr, cfg.PeerPort))
		}
		if cfg.ClientPort != "" {
			info.ClientURLs = append(info.ClientURLs, fmt.Sprintf("https://%s:%s", ipStr, cfg.ClientPort))
		}
	}
}

// GetInterfaceIP gets the first IPv4 address from a specific interface
func GetInterfaceIP(ifaceName string) (string, error) {
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return "", fmt.Errorf("failed to get interface %s: %w", ifaceName, err)
	}

	addrs, err := iface.Addrs()
	if err != nil {
		return "", fmt.Errorf("failed to get addresses for interface %s: %w", ifaceName, err)
	}

	for _, addr := range addrs {
		var ip net.IP
		var ones, bits int
		switch v := addr.(type) {
		case *net.IPNet:
			ip = v.IP
			ones, bits = v.Mask.Size()
		case *net.IPAddr:
			ip = v.IP
		}

		// Only use IPv4 addresses
		if ip == nil || ip.To4() == nil {
			continue
		}
		if bits == 32 && ones == 32 {
			continue
		}
		return ip.String(), nil
	}

	return "", fmt.Errorf("no IPv4 address found on interface %s", ifaceName)
}

// GetInterfacesByPrefix returns all interface names matching the given prefix
func GetInterfacesByPrefix(prefix string) ([]string, error) {
	allInterfaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("failed to get network interfaces: %w", err)
	}

	var matched []string
	for _, iface := range allInterfaces {
		if strings.HasPrefix(iface.Name, prefix) {
			matched = append(matched, iface.Name)
		}
	}

	if len(matched) == 0 {
		return nil, fmt.Errorf("no interfaces found with prefix: %s", prefix)
	}

	return matched, nil
}

// GetIPsByPrefix gets all IPv4 addresses from interfaces matching the prefix
func GetIPsByPrefix(prefix string) ([]string, error) {
	interfaces, err := GetInterfacesByPrefix(prefix)
	if err != nil {
		return nil, err
	}

	ips := make([]string, 0, len(interfaces))
	for _, ifaceName := range interfaces {
		ip, err := GetInterfaceIP(ifaceName)
		if err != nil {
			klog.Warningf("Failed to get IP from interface %s: %v", ifaceName, err)
			continue
		}
		ips = append(ips, ip)
		klog.Infof("Found IP %s on interface %s (prefix: %s)", ip, ifaceName, prefix)
	}

	if len(ips) == 0 {
		return nil, fmt.Errorf("no IPv4 addresses found on interfaces with prefix: %s", prefix)
	}

	return ips, nil
}
