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
	etcdcli "go.etcd.io/etcd/client/v3"
	"k8s.io/klog/v2"
)

// ClusterConfig holds configuration for cluster operations
type ClusterConfig struct {
	PodName    string
	PeerPort   string
	ClientPort string
	DataDir    string
	CertFile   string
	KeyFile    string
}

// Manager handles etcd cluster operations
type Manager struct {
	cfg    *ClusterConfig
	client etcdcli.Cluster
}

// NewManager creates a new cluster manager
func NewManager(cfg *ClusterConfig) *Manager {
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
	var (
		cluster     []string
		mypeers     = make([]string, 0, len(myIPs))
		addResp     *etcdcli.MemberAddResponse
		myMemberID  uint64
		nonLearn    int
		bgct        = context.Background()
		ctx, cancel = context.WithTimeout(bgct, 3*time.Second)
	)
	defer cancel()

	resp, err := client.MemberList(etcdcli.WithRequireLeader(ctx))
	if err != nil {
		return nil, err
	}

	// Check if current pod is already in cluster
	for _, member := range resp.Members {
		if member.Name == m.cfg.PodName {
			myMemberID = member.ID
			if util.DirExists(filepath.Join(m.cfg.DataDir, "member")) {
				if member.GetIsLearner() {
					_, err = client.MemberPromote(ctx, myMemberID)
					if err != nil {
						return nil, err
					}
				}
				klog.Info("memberdir exists, and i'm in cluster, start etcd")
				return m.startEtcd(etcdBin)
			}
		}
		if !member.IsLearner {
			nonLearn++
		}

		// Remove dead members
		if _, ok := deadnames[member.Name]; ok {
			klog.Infof("member '%s' had dead but in cluster, remove it", member.Name)
			_, err = client.MemberRemove(ctx, member.ID)
			if err != nil {
				return nil, err
			}
		}
	}

	// Must remove member if data doesn't exist but member exists
	if myMemberID != 0 {
		klog.Infof("%s(%04d...) in cluster, but data not exist, should remove then rejoin", m.cfg.PodName, myMemberID)
		_, err = client.MemberRemove(ctx, myMemberID)
		if err != nil {
			return nil, err
		}
	}

	os.RemoveAll(filepath.Join(m.cfg.DataDir, "member"))
	os.Setenv("ETCD_INITIAL_CLUSTER_STATE", "existing")

	// Build peer URLs
	for _, ip := range myIPs {
		mypeers = append(mypeers, fmt.Sprintf("https://%s:%s", ip, m.cfg.PeerPort))
	}

	ctx2, cancel2 := context.WithTimeout(bgct, 3*time.Second)
	defer cancel2()

	// Add member as learner or regular member
	if len(resp.Members) != 1 {
		if nonLearn == 1 {
			klog.Info("member count > 1, but non learner is 1")
			return nil, fmt.Errorf("non learner is 1")
		}
		klog.Info("add member then start etcd")
		addResp, err = client.MemberAdd(ctx2, mypeers)
	} else {
		klog.Info("one master, start etcd as learner then promote")
		addResp, err = client.MemberAddAsLearner(ctx2, mypeers)
	}

	if err != nil {
		return nil, err
	}

	// Build cluster configuration
	for _, member := range addResp.Members {
		for _, url := range member.PeerURLs {
			if member.Name == "" {
				member.Name = m.cfg.PodName
			}
			cluster = append(cluster, fmt.Sprintf("%s=%s", member.Name, url))
		}
	}

	os.Setenv("ETCD_INITIAL_CLUSTER", strings.Join(cluster, ","))

	cmd, err := m.startEtcd(etcdBin)
	if err != nil {
		return nil, err
	}

	// Promote learner if single master
	if len(resp.Members) == 1 {
		count := 1
		for count < 10 {
			time.Sleep(2 * time.Second)
			_, err = client.MemberPromote(bgct, addResp.Member.ID)
			if err == nil {
				klog.Infof("promote member '%s' success", m.cfg.PodName)
				break
			}
			count++
		}
	}

	if err != nil {
		err = cmd.Process.Signal(syscall.SIGTERM)
		return nil, err
	}

	return cmd, nil
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
