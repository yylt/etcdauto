package cluster

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/yylt/etcdauto/pkg/util"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	etcdcli "go.etcd.io/etcd/client/v3"
	"k8s.io/klog/v2"
)

// Config holds configuration for cluster operations
type Config struct {
	PodName    string
	PeerPort   string
	ClientPort string
	DataDir    string
	CertFile   string
	KeyFile    string
}

// Manager handles etcd cluster operations
type Manager struct {
	cfg    *Config
	client etcdcli.Cluster
}

// NewManager creates a new cluster manager
func NewManager(cfg *Config) *Manager {
	return &Manager{
		cfg: cfg,
	}
}

// GetClient creates an etcd client from alive endpoints
func (m *Manager) GetClient(aliveEndpoints map[string][]string) (etcdcli.Cluster, error) {
	if len(aliveEndpoints) == 0 {
		return nil, fmt.Errorf("no alive endpoints found")
	}

	var endpoints []string
	for _, iplist := range aliveEndpoints {
		for _, ip := range iplist {
			endpoints = append(endpoints, net.JoinHostPort(ip, m.cfg.ClientPort))
		}
	}

	// Load client cert
	cert, err := tls.LoadX509KeyPair(m.cfg.CertFile, m.cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load key pair: %w", err)
	}

	cliconfig := etcdcli.Config{
		Endpoints:       endpoints,
		MaxUnaryRetries: 14,
		DialTimeout:     2 * time.Second,
		TLS: &tls.Config{
			InsecureSkipVerify: true,
			Certificates:       []tls.Certificate{cert},
		},
	}

	client, err := etcdcli.New(cliconfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create etcd client: %w", err)
	}

	m.client = etcdcli.NewCluster(client)
	return m.client, nil
}

// InitializeNewCluster initializes a new etcd cluster
func (m *Manager) InitializeNewCluster(myIPs []string, etcdBin string) (*exec.Cmd, error) {
	klog.Infof("=== Initializing new etcd cluster ===")
	klog.Infof("Pod name: %s", m.cfg.PodName)
	klog.Infof("My IPs: %v", myIPs)
	klog.Infof("Data directory: %s", m.cfg.DataDir)

	os.Setenv("ETCD_INITIAL_CLUSTER_STATE", "new")
	klog.V(2).Info("Set ETCD_INITIAL_CLUSTER_STATE=new")

	cluster := BuildPeerEndpoints(
		map[string][]string{m.cfg.PodName: myIPs},
		m.cfg.PeerPort,
		true, true)

	clusterStr := strings.Join(cluster, ",")
	os.Setenv("ETCD_INITIAL_CLUSTER", clusterStr)
	klog.Infof("Initial cluster configuration: %s", clusterStr)

	memberDir := filepath.Join(m.cfg.DataDir, "member")
	if util.DirExists(memberDir) {
		klog.Warningf("Removing existing member directory: %s", memberDir)
		os.RemoveAll(memberDir)
	}

	klog.Info("Starting etcd with new cluster configuration")
	return m.startEtcd(etcdBin)
}

// JoinExistingCluster joins an existing etcd cluster
func (m *Manager) JoinExistingCluster(client etcdcli.Cluster, myIPs []string, deadnames map[string]struct{}, etcdBin string) (*exec.Cmd, error) {
	klog.Infof("=== Joining existing etcd cluster ===")
	klog.Infof("Pod name: %s", m.cfg.PodName)
	klog.Infof("My IPs: %v", myIPs)
	klog.Infof("Dead members to remove: %v", getDeadNames(deadnames))

	ctx := context.Background()

	klog.V(2).Info("Fetching current cluster member list")
	resp, err := client.MemberList(etcdcli.WithRequireLeader(ctx))
	if err != nil {
		return nil, fmt.Errorf("failed to list members: %w", err)
	}

	myMemberID, myIsLearner, aliveNumExceptMe := m.processMembers(ctx, client, resp.Members, deadnames)

	// Handle existing member or add new member
	return m.handleMemberJoin(ctx, client, myMemberID, myIsLearner, myIPs, aliveNumExceptMe, etcdBin)
}

// processMembers processes the member list, removes dead members, and finds current pod
func (m *Manager) processMembers(ctx context.Context, client etcdcli.Cluster, members []*pb.Member, deadnames map[string]struct{}) (uint64, bool, int) {
	var (
		myMemberID       uint64
		myIsLearner      bool
		aliveNumExceptMe int
	)

	for _, member := range members {
		if member.Name == m.cfg.PodName {
			myMemberID = member.ID
			myIsLearner = member.GetIsLearner()
			klog.Infof("Found myself in cluster: ID=%016x, IsLearner=%v", member.ID, member.IsLearner)
			continue
		}

		// Check if member's PeerURLs contain any dead master IP
		shouldRemove := false
		for _, peerURL := range member.PeerURLs {
			for deadMasterIP := range deadnames {
				if strings.Contains(peerURL, deadMasterIP) {
					klog.Infof("Member %s (ID=%016x) has dead master IP %s in PeerURL: %s",
						member.Name, member.ID, deadMasterIP, peerURL)
					shouldRemove = true
					break
				}
			}
			if shouldRemove {
				break
			}
		}

		if shouldRemove {
			m.removeDeadMember(ctx, client, member)
		} else {
			aliveNumExceptMe++
		}
	}

	return myMemberID, myIsLearner, aliveNumExceptMe
}

// removeDeadMember removes a dead member from the cluster
func (m *Manager) removeDeadMember(ctx context.Context, client etcdcli.Cluster, member *pb.Member) {
	klog.Infof("Removing dead member: ID=%016x, Name=%s", member.ID, member.Name)
	_, err := client.MemberRemove(ctx, member.ID)
	if err != nil {
		klog.Errorf("failed to remove member %s (%016x): %s", member.Name, member.ID, err)
	} else {
		klog.Infof("Successfully removed dead member %s", member.Name)
	}
}

// handleMemberJoin handles joining logic based on member state
func (m *Manager) handleMemberJoin(ctx context.Context, client etcdcli.Cluster, myMemberID uint64, myIsLearner bool, myIPs []string, aliveNumExceptMe int, etcdBin string) (*exec.Cmd, error) {
	dataDir := filepath.Join(m.cfg.DataDir, "member")

	if myMemberID != 0 {
		if util.DirExists(dataDir) {
			return m.handleExistingMemberWithData(client, myMemberID, myIsLearner, etcdBin)
		}
		return m.handleExistingMemberWithoutData(ctx, client, myMemberID, myIPs, aliveNumExceptMe, etcdBin)
	}

	if util.DirExists(dataDir) {
		klog.Infof("not found myid, but dataDir exists at %s, will remove it", dataDir)
		os.RemoveAll(dataDir)
	}

	// Add member and start etcd
	return m.addMemberAndStart(client, myIPs, aliveNumExceptMe, etcdBin)
}

// handleExistingMemberWithData handles case where member exists in cluster and has data
func (m *Manager) handleExistingMemberWithData(client etcdcli.Cluster, myMemberID uint64, myIsLearner bool, etcdBin string) (*exec.Cmd, error) {
	dataDir := filepath.Join(m.cfg.DataDir, "member")
	klog.Infof("my id=%016x, IsLearner=%v, dataDir exists at %s", myMemberID, myIsLearner, dataDir)

	if myIsLearner {
		err := m.promoteLearner(client, myMemberID)
		if err != nil {
			return nil, fmt.Errorf("failed to promote learner: %w", err)
		}
	}
	return m.startEtcd(etcdBin)
}

// handleExistingMemberWithoutData handles case where member exists in cluster but has no data
func (m *Manager) handleExistingMemberWithoutData(ctx context.Context, client etcdcli.Cluster, myMemberID uint64, myIPs []string, aliveNumExceptMe int, etcdBin string) (*exec.Cmd, error) {
	_, err := client.MemberRemove(ctx, myMemberID)
	if err != nil {
		klog.Errorf("failed to remove my by id(%016x): %s", myMemberID, err)
		return nil, err
	}
	klog.Infof("Successfully removed member %016x, will rejoin as new member", myMemberID)

	// Add member and start etcd
	return m.addMemberAndStart(client, myIPs, aliveNumExceptMe, etcdBin)
}

// getDeadNames converts dead names map to slice for logging
func getDeadNames(deadnames map[string]struct{}) []string {
	names := make([]string, 0, len(deadnames))
	for name := range deadnames {
		names = append(names, name)
	}
	return names
}

// addMemberAndStart adds member to cluster and starts etcd
func (m *Manager) addMemberAndStart(client etcdcli.Cluster, myIPs []string, alive int, etcdBin string) (*exec.Cmd, error) {
	// Build peer URLs
	mypeers := make([]string, 0, len(myIPs))
	for _, ip := range myIPs {
		mypeers = append(mypeers, fmt.Sprintf("https://%s:%s", ip, m.cfg.PeerPort))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Add member as learner or regular member
	var addResp *etcdcli.MemberAddResponse
	var err error
	var shouldPromote bool

	// Decision logic based on cluster state:
	// 1. If only 1 non-learner member exists: add as learner, then promote after etcd starts
	// 2. If multiple non-learner members but only 1 would remain: reject (quorum protection)
	// 3. Otherwise: add as regular member
	switch {
	case alive == 1:
		klog.Infof("adding %s as learner then will promote", m.cfg.PodName)
		addResp, err = client.MemberAddAsLearner(ctx, mypeers)
		shouldPromote = true
	case alive > 1:
		// Check if adding would leave only 1 non-learner (shouldn't happen in normal flow)
		klog.Infof("adding %s to cluster then start etcd", m.cfg.PodName)
		addResp, err = client.MemberAdd(ctx, mypeers)
	default:
		// nonLearn == 0, this shouldn't happen in a healthy cluster
		klog.Warning("Cluster has no non-learner members, this is an unhealthy state")
		return nil, fmt.Errorf("cluster has no non-learner members, cannot add new member safely")
	}

	if err != nil {
		return nil, fmt.Errorf("failed to add member: %w", err)
	}
	klog.Infof("Add to cluster successfully: ID=%016x, Name=%s, IsLearner=%v",
		addResp.Member.ID, m.cfg.PodName, addResp.Member.IsLearner)

	// Build cluster configuration
	cluster := m.buildClusterConfig(addResp.Members)
	clusterStr := strings.Join(cluster, ",")

	os.Setenv("ETCD_INITIAL_CLUSTER_STATE", "existing")
	os.Setenv("ETCD_INITIAL_CLUSTER", clusterStr)
	klog.Infof("Cluster configuration: %s", clusterStr)

	cmd, err := m.startEtcd(etcdBin)
	if err != nil {
		return nil, fmt.Errorf("failed to start etcd: %w", err)
	}

	// Promote learner after etcd starts (following README workflow)
	if shouldPromote {
		klog.Infof("Promoting learner member %016x to voting member", addResp.Member.ID)
		if err := m.promoteLearner(client, addResp.Member.ID); err != nil {
			klog.Errorf("Failed to promote learner: %v, terminating etcd", err)
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
				klog.Errorf("Failed to send SIGTERM to etcd process: %v", err)
			}
			return nil, fmt.Errorf("failed to promote learner: %w", err)
		}
	}

	klog.Info("Member successfully joined cluster and etcd started")
	return cmd, nil
}

// buildClusterConfig builds cluster configuration from members
func (m *Manager) buildClusterConfig(members []*pb.Member) []string {
	var cluster []string
	for _, member := range members {
		for _, url := range member.PeerURLs {
			if member.Name == "" {
				member.Name = m.cfg.PodName
			}
			cluster = append(cluster, fmt.Sprintf("%s=%s", member.Name, url))
		}
	}
	return cluster
}

// promoteLearner promotes a learner member to voting member
func (m *Manager) promoteLearner(client etcdcli.Cluster, memberID uint64) error {
	const maxRetries = 100
	const retryInterval = 200 * time.Millisecond

	var ctx = context.Background()
	klog.Infof("Starting learner promotion for member %016x (max retries: %d)", memberID, maxRetries)

	for attempt := 1; attempt <= maxRetries; attempt++ {
		if attempt > 1 {
			klog.V(2).Infof("Retry attempt %d/%d for promoting member %016x", attempt, maxRetries, memberID)
			time.Sleep(retryInterval)
		}
		_, err := client.MemberPromote(ctx, memberID)

		if err == nil {
			klog.Infof("Successfully promoted learner member %016x to voting member", memberID)
			return nil
		}

		klog.Warningf("Failed to promote member %016x (attempt %d/%d): %v", memberID, attempt, maxRetries, err)
	}

	return fmt.Errorf("failed to promote member %016x after %d attempts", memberID, maxRetries)
}

// startEtcd starts the etcd process
func (m *Manager) startEtcd(etcdBin string) (*exec.Cmd, error) {
	cmd := exec.Command(etcdBin)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	return cmd, nil
}

// BrainSplitCheckConfig holds configuration for brain split detection
type BrainSplitCheckConfig struct {
	NodeIPDir   string
	ClientPort  string
	CertFile    string
	KeyFile     string
	MyPodName   string
	MyIPs       []string // My own IPs to exclude from checking
	CheckPeriod time.Duration
	MaxChecks   int // Maximum number of successful checks before stopping (0 = unlimited)
}

// BrainSplitCheckResult represents the result of brain split detection
type BrainSplitCheckResult struct {
	BrainSplitDetected bool
	Reason             string
}

// StartBrainSplitChecker starts a goroutine to periodically check for brain split.
// For etcd-0, after initializing a new cluster, we need to verify that other nodes
// joining the cluster include etcd-0 in their member list.
// Returns a channel that will receive the result when brain split is detected or check completes.
func (m *Manager) StartBrainSplitChecker(cfg *BrainSplitCheckConfig, stopCh <-chan struct{}) <-chan BrainSplitCheckResult {
	resultCh := make(chan BrainSplitCheckResult, 1)

	go func() {
		defer close(resultCh)

		if cfg.CheckPeriod <= 0 {
			cfg.CheckPeriod = 5 * time.Second
		}
		if cfg.MaxChecks <= 0 {
			cfg.MaxChecks = 60 // Default: check for up to 5 minutes (60 * 5s)
		}

		tlscfg, err := loadTLSConfig(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			klog.Errorf("Brain split checker: failed to load TLS config: %v", err)
			return
		}

		ticker := time.NewTicker(cfg.CheckPeriod)
		defer ticker.Stop()

		successCount := 0
		klog.Infof("Brain split checker started for %s, checking every %v", cfg.MyPodName, cfg.CheckPeriod)

		for {
			select {
			case <-stopCh:
				klog.Info("Brain split checker stopped by signal")
				return
			case <-ticker.C:
				result := m.checkBrainSplit(cfg, tlscfg)
				if result.BrainSplitDetected {
					klog.Warningf("Brain split detected: %s", result.Reason)
					resultCh <- result
					return
				}

				// If we found other nodes and they all include us, increment success count
				if result.Reason == "verified" {
					successCount++
					klog.Infof("Brain split check passed (%d/%d)", successCount, cfg.MaxChecks)
					if successCount >= cfg.MaxChecks {
						klog.Info("Brain split checker: all checks passed, stopping checker")
						resultCh <- BrainSplitCheckResult{
							BrainSplitDetected: false,
							Reason:             "all checks passed",
						}
						return
					}
				}
			}
		}
	}()

	return resultCh
}

// checkBrainSplit performs a single brain split check
func (m *Manager) checkBrainSplit(cfg *BrainSplitCheckConfig, tlscfg *tls.Config) BrainSplitCheckResult {
	// Build a set of my own IPs for quick lookup
	myIPSet := make(map[string]struct{})
	for _, ip := range cfg.MyIPs {
		myIPSet[ip] = struct{}{}
	}

	// Read all IP files from nodeIPDir
	entries, err := os.ReadDir(cfg.NodeIPDir)
	if err != nil {
		klog.V(2).Infof("Brain split check: failed to read nodeIPDir: %v", err)
		return BrainSplitCheckResult{BrainSplitDetected: false, Reason: "read error"}
	}

	// Find other healthy nodes (excluding ourselves)
	var otherEndpoints []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		masterIP := entry.Name()
		if net.ParseIP(masterIP) == nil {
			continue
		}

		// Skip if this is my own master IP
		if _, isMyIP := myIPSet[masterIP]; isMyIP {
			continue
		}

		// Read IP list from file
		ips, err := readIPFile(cfg.NodeIPDir, masterIP)
		if err != nil {
			continue
		}

		// Check if any IP is healthy (skip my own IPs)
		for _, ip := range ips {
			if _, isMyIP := myIPSet[ip]; isMyIP {
				continue
			}
			endpoint := net.JoinHostPort(ip, cfg.ClientPort)
			if checkSingleEndpoint(context.Background(), endpoint, tlscfg) {
				otherEndpoints = append(otherEndpoints, endpoint)
			}
		}
	}

	if len(otherEndpoints) == 0 {
		// No other healthy nodes found yet, continue checking
		klog.V(2).Info("Brain split check: no other healthy nodes found yet")
		return BrainSplitCheckResult{BrainSplitDetected: false, Reason: "no other nodes"}
	}

	// Connect to other nodes and check if they include us in their member list
	for _, endpoint := range otherEndpoints {
		cliconfig := etcdcli.Config{
			Endpoints:   []string{endpoint},
			DialTimeout: 3 * time.Second,
			TLS:         tlscfg,
		}

		client, err := etcdcli.New(cliconfig)
		if err != nil {
			klog.V(2).Infof("Brain split check: failed to connect to %s: %v", endpoint, err)
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		resp, err := client.MemberList(ctx)
		cancel()
		client.Close()

		if err != nil {
			klog.V(2).Infof("Brain split check: failed to list members from %s: %v", endpoint, err)
			continue
		}

		// Check if we are in the member list
		foundSelf := false
		for _, member := range resp.Members {
			if member.Name == cfg.MyPodName {
				foundSelf = true
				break
			}
		}

		if !foundSelf {
			// Brain split detected: another cluster exists without us
			return BrainSplitCheckResult{
				BrainSplitDetected: true,
				Reason:             fmt.Sprintf("endpoint %s has a cluster without %s", endpoint, cfg.MyPodName),
			}
		}

		klog.V(2).Infof("Brain split check: endpoint %s includes %s in member list", endpoint, cfg.MyPodName)
	}

	return BrainSplitCheckResult{BrainSplitDetected: false, Reason: "verified"}
}
