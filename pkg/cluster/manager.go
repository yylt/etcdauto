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

		// Handle other members
		if _, ok := deadnames[member.Name]; ok {
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
