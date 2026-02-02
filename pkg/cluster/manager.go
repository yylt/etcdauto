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
	klog.Info("Initializing new etcd cluster")

	os.Setenv("ETCD_INITIAL_CLUSTER_STATE", "new")

	cluster := BuildPeerEndpoints(
		map[string][]string{m.cfg.PodName: myIPs},
		m.cfg.PeerPort,
		true, true)

	os.Setenv("ETCD_INITIAL_CLUSTER", strings.Join(cluster, ","))
	os.RemoveAll(filepath.Join(m.cfg.DataDir, "member"))

	klog.Info("Starting etcd with new cluster configuration")
	return m.startEtcd(etcdBin)
}

// JoinExistingCluster joins an existing etcd cluster
func (m *Manager) JoinExistingCluster(client etcdcli.Cluster, myIPs []string, deadnames map[string]struct{}, etcdBin string) (*exec.Cmd, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	resp, err := client.MemberList(etcdcli.WithRequireLeader(ctx))
	if err != nil {
		return nil, err
	}

	// Check if current pod is already in cluster and handle it
	myMemberID, nonLearn, err := m.handleExistingMember(client, resp.Members, etcdBin)
	if err != nil {
		return nil, err
	}

	// Remove dead members
	if err := m.removeDeadMembers(client, resp.Members, deadnames); err != nil {
		return nil, err
	}

	// Must remove member if data doesn't exist but member exists
	if err := m.removeMemberWithoutData(client, myMemberID); err != nil {
		return nil, err
	}

	os.RemoveAll(filepath.Join(m.cfg.DataDir, "member"))
	os.Setenv("ETCD_INITIAL_CLUSTER_STATE", "existing")

	// Add member and start etcd
	return m.addMemberAndStart(client, myIPs, resp.Members, nonLearn, etcdBin)
}

// handleExistingMember checks if current pod is already in cluster and handles it
func (m *Manager) handleExistingMember(client etcdcli.Cluster, members []*pb.Member, etcdBin string) (uint64, int, error) {
	var myMemberID uint64
	var nonLearn int

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	for _, member := range members {
		if member.Name != m.cfg.PodName {
			continue
		}
		myMemberID = member.ID
		if util.DirExists(filepath.Join(m.cfg.DataDir, "member")) {
			if member.GetIsLearner() {
				_, err := client.MemberPromote(ctx, myMemberID)
				if err != nil {
					return 0, 0, err
				}
			}
			klog.Info("memberdir exists, and i'm in cluster, start etcd")
			cmd, err := m.startEtcd(etcdBin)
			if err != nil {
				return 0, 0, err
			}
			// Exit early by waiting for the command
			return 0, 0, cmd.Wait()
		}

		if !member.IsLearner {
			nonLearn++
		}
	}

	return myMemberID, nonLearn, nil
}

// removeDeadMembers removes dead members from the cluster
func (m *Manager) removeDeadMembers(client etcdcli.Cluster, members []*pb.Member, deadnames map[string]struct{}) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	for _, member := range members {
		if _, ok := deadnames[member.Name]; ok {
			klog.Infof("member '%s' had dead but in cluster, remove it", member.Name)
			_, err := client.MemberRemove(ctx, member.ID)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// removeMemberWithoutData removes member if data doesn't exist but member exists
func (m *Manager) removeMemberWithoutData(client etcdcli.Cluster, myMemberID uint64) error {
	if myMemberID != 0 {
		klog.Infof("%s(%04d...) in cluster, but data not exist, should remove then rejoin", m.cfg.PodName, myMemberID)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, err := client.MemberRemove(ctx, myMemberID)
		if err != nil {
			return err
		}
	}
	return nil
}

// addMemberAndStart adds member to cluster and starts etcd
func (m *Manager) addMemberAndStart(client etcdcli.Cluster, myIPs []string, members []*pb.Member, nonLearn int, etcdBin string) (*exec.Cmd, error) {
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

	if len(members) != 1 {
		if nonLearn == 1 {
			klog.Info("member count > 1, but non learner is 1")
			return nil, fmt.Errorf("non learner is 1")
		}
		klog.Info("add member then start etcd")
		addResp, err = client.MemberAdd(ctx, mypeers)
	} else {
		klog.Info("one master, start etcd as learner then promote")
		addResp, err = client.MemberAddAsLearner(ctx, mypeers)
	}

	if err != nil {
		return nil, err
	}

	// Build cluster configuration
	cluster := m.buildClusterConfig(addResp.Members)
	os.Setenv("ETCD_INITIAL_CLUSTER", strings.Join(cluster, ","))

	cmd, err := m.startEtcd(etcdBin)
	if err != nil {
		return nil, err
	}

	// Promote learner if single master
	if len(members) == 1 {
		if err := m.promoteLearner(client, addResp.Member.ID); err != nil {
			return nil, cmd.Process.Signal(syscall.SIGTERM)
		}
	}

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
	count := 1
	for count < 10 {
		time.Sleep(2 * time.Second)
		_, err := client.MemberPromote(context.Background(), memberID)
		if err == nil {
			klog.Infof("promote member '%s' success", m.cfg.PodName)
			return nil
		}
		count++
	}
	return fmt.Errorf("failed to promote member after 10 attempts")
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
