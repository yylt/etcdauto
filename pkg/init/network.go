package init

import (
	"fmt"
	"net"
	"strings"

	"k8s.io/klog/v2"
)

// NetworkConfig holds network interface configuration
type NetworkConfig struct {
	Interfaces       []string // Network interfaces to extract IPs from (exact names or prefixes)
	InterfacePrefixes []string // Network interface prefixes to match (e.g., "eth" matches eth0, eth1, etc.)
	ClientPort       string   // Client port for etcd
	PeerPort         string   // Peer port for etcd
}

// NetworkInfo holds extracted network information
type NetworkInfo struct {
	IPs         []string // All IP addresses from interfaces
	PeerURLs    []string // Peer URLs (https://ip:peerPort)
	ClientURLs  []string // Client URLs (https://ip:clientPort)
}

// ExtractNetworkInfo extracts IP addresses from network interfaces
// Supports both exact interface names and prefix matching
func ExtractNetworkInfo(cfg *NetworkConfig) (*NetworkInfo, error) {
	if len(cfg.Interfaces) == 0 && len(cfg.InterfacePrefixes) == 0 {
		return nil, fmt.Errorf("no interfaces or interface prefixes specified")
	}

	info := &NetworkInfo{
		IPs:        make([]string, 0),
		PeerURLs:   make([]string, 0),
		ClientURLs: make([]string, 0),
	}

	// Get all network interfaces
	allInterfaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("failed to get network interfaces: %w", err)
	}

	// Build a set of interfaces to process
	interfacesToProcess := make(map[string]bool)

	// Add exact interface names
	for _, ifaceName := range cfg.Interfaces {
		interfacesToProcess[ifaceName] = true
	}

	// Add interfaces matching prefixes
	for _, iface := range allInterfaces {
		for _, prefix := range cfg.InterfacePrefixes {
			if strings.HasPrefix(iface.Name, prefix) {
				interfacesToProcess[iface.Name] = true
				klog.Infof("Interface %s matches prefix %s", iface.Name, prefix)
			}
		}
	}

	if len(interfacesToProcess) == 0 {
		return nil, fmt.Errorf("no matching interfaces found")
	}

	// Extract IPs from matched interfaces
	for ifaceName := range interfacesToProcess {
		iface, err := net.InterfaceByName(ifaceName)
		if err != nil {
			klog.Warningf("Failed to get interface %s: %v", ifaceName, err)
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			klog.Warningf("Failed to get addresses for interface %s: %v", ifaceName, err)
			continue
		}

		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}

			// Only use IPv4 addresses
			if ip == nil || ip.To4() == nil {
				continue
			}

			ipStr := ip.String()
			info.IPs = append(info.IPs, ipStr)

			// Build URLs
			if cfg.PeerPort != "" {
				info.PeerURLs = append(info.PeerURLs, fmt.Sprintf("https://%s:%s", ipStr, cfg.PeerPort))
			}
			if cfg.ClientPort != "" {
				info.ClientURLs = append(info.ClientURLs, fmt.Sprintf("https://%s:%s", ipStr, cfg.ClientPort))
			}

			klog.Infof("Found IP %s on interface %s", ipStr, ifaceName)
			break // Only take first IPv4 address per interface
		}
	}

	if len(info.IPs) == 0 {
		return nil, fmt.Errorf("no IPv4 addresses found on matching interfaces")
	}

	return info, nil
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
		switch v := addr.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}

		// Only use IPv4 addresses
		if ip != nil && ip.To4() != nil {
			return ip.String(), nil
		}
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

	var ips []string
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
