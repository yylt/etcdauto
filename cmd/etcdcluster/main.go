package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/yylt/etcdauto/pkg/util"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	etcdcli "go.etcd.io/etcd/client/v3"
	"k8s.io/klog/v2"
)

var (
	ETCDBIN string
)

// printBuildInfo prints version and VCS information
func printBuildInfo() {
	// Get build info from runtime/debug
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
			klog.Infof("Build information, reversion: %s, time: %s, modified: %s", vcsModified, vcsTime, vcsRevision)
		}
	}
}

func main() {
	var (
		err   error
		myips []string
	)
	printBuildInfo()
	// 1. Check required environment variables
	if err := checkRequiredEnvVars(); err != nil {
		klog.Fatal("Missing required environment variables")
	}
	ETCDBIN, err = exec.LookPath("etcd")
	if err != nil {
		klog.Fatal("etcd binary not found")
	}

	iplist := strings.Split(strings.TrimSpace(os.Getenv("POD_IPS")), ",")
	for _, ip := range iplist {
		if ip != "" {
			myips = append(myips, ip)
		}
	}
	// Parse POD_NAME
	prefix, myIndex, err := parsePodName(os.Getenv("POD_NAME"))
	if err != nil {
		klog.Fatalf("Failed to parse POD_NAME: %s", err)
	}

	klog.Infof("Current pod IP: %s", myips)
	if len(myips) == 0 {
		klog.Fatal("No available IP for current pod")
	}
	// Get alive endpoints
	for {
		aliveEndpoints, deadnames := getAliveEndpoints(prefix, util.MustAtoi(os.Getenv("MAX")), myIndex)
		switch len(aliveEndpoints) {
		case 0:
			if myIndex != 0 {
				klog.Warningf("Failed to get '%s' endpoints, retrying...", os.Getenv("POD_NAME"))
				break
			}
			cmd, err := initializeNewCluster(myips)
			if err != nil {
				klog.Errorf("Failed to initialize new cluster: %v", err)
				break
			}
			waitExit(cmd)
		default:
			client, err := getCurrentClient(aliveEndpoints)
			if err != nil {
				klog.Errorf("Failed to get current client: %v", err)
				break
			}
			cmd, err := joinExistingCluster(client, myips, deadnames)
			if err == nil {
				waitExit(cmd)
			} else {
				klog.Errorf("Failed to join existing cluster: %v", err)
			}
		}
		time.Sleep(2 * time.Second)
	}
}

func checkRequiredEnvVars() error {
	requiredVars := []string{
		"SERVICE_NAME", "MAX", "NODEIP_DIR",
		"POD_NAME", "POD_NAMESPACE", "POD_IPS",
		"ETCDCTL_CACERT", "ETCDCTL_CERT", "ETCDCTL_KEY", "ETCD_DATA_DIR",
		"CLIENT_PORT", "PEER_PORT",
	}

	missingVars := []string{}
	for _, varName := range requiredVars {
		if os.Getenv(varName) == "" {
			missingVars = append(missingVars, varName)
		}
	}

	if len(missingVars) > 0 {
		return fmt.Errorf("Missing required environment variables: %v", missingVars)
	}
	klog.Info("Starting cluster initialization",
		"SERVICE_NAME", os.Getenv("SERVICE_NAME"),
		"MAX", os.Getenv("MAX"),
		"POD_NAME", os.Getenv("POD_NAME"),
		"POD_NAMESPACE", os.Getenv("POD_NAMESPACE"),
		"ETCD_DATA_DIR", os.Getenv("ETCD_DATA_DIR"),
		"NODEIP_DIR", os.Getenv("NODEIP_DIR"),
		"CLIENT_PORT", os.Getenv("CLIENT_PORT"),
		"PEER_PORT", os.Getenv("PEER_PORT"),
	)
	return nil
}

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

func domainIPList(domain string) ([]string, error) {
	ips, err := net.LookupHost(domain)
	if err != nil {
		klog.Errorf("Failed to resolve domain %s: %v", domain, err)
		return nil, err
	}

	if len(ips) == 0 {
		return nil, fmt.Errorf("no IPs found for domain %s", domain)
	}

	iplist, err := readIPFile(ips[0])
	klog.Infof("Resolved domain %s, iplist: %s", domain, strings.Join(iplist, ","))
	return iplist, err
}

func readIPFile(ip string) ([]string, error) {
	ipFile := filepath.Join(os.Getenv("NODEIP_DIR"), ip)
	if !util.FileExists(ipFile) {
		klog.Warning("IP file not found, using resolved IP directly",
			"ipFile", ipFile, "reslovip", ip)
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

func checkReadyz(ipport string, tlscfg *tls.Config) bool {
	var url = fmt.Sprintf("https://%s/readyz", ipport)

	// Setup HTTPS client
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

	klog.Warning("Etcd node is not ready",
		"ipport", ipport,
		"statusCode", resp.StatusCode)
	return false
}

func peerEndponits(endpoints map[string][]string, port string, withname bool, withscheme bool) []string {
	var addresses []string
	for k, iplist := range endpoints {
		for _, ip := range iplist {
			if withscheme {
				ip = fmt.Sprintf("https://%s", ip)
			}
			if withname {
				addresses = append(addresses, fmt.Sprintf("%s=%s:%s", k, ip, port))
			} else {
				addresses = append(addresses, fmt.Sprintf("%s:%s", ip, port))
			}
		}
	}
	return addresses
}

func getaliveByk8s() (map[string]string, error) {
	label := os.Getenv("LABELS")
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	clientset := kubernetes.NewForConfigOrDie(config)
	pods, err := clientset.CoreV1().Pods(os.Getenv("POD_NAMESPACE")).List(context.TODO(), metav1.ListOptions{
		ResourceVersion: "0",
		LabelSelector:   label,
	})
	if err != nil {
		return nil, err
	}
	var podip = make(map[string]string)

	for _, pod := range pods.Items {
		podip[pod.Name] = pod.Status.HostIP
	}
	return podip, nil
}

func getAliveEndpoints(prefix string, maxNodes, myIndex int) (aliveEndpoints map[string][]string, deadnames map[string]struct{}) {
	var (
		wg         sync.WaitGroup
		mu         sync.Mutex
		clientPort = os.Getenv("CLIENT_PORT")
		fromdns    = false
	)
	aliveEndpoints = make(map[string][]string)
	deadnames = make(map[string]struct{})
	// Load client cert
	cert, err := tls.LoadX509KeyPair(os.Getenv("ETCDCTL_CERT"), os.Getenv("ETCDCTL_KEY"))
	if err != nil {
		klog.Fatalf("Failed to load key pair: %v", err)
	}
	tlscfg := &tls.Config{
		Certificates:       []tls.Certificate{cert},
		InsecureSkipVerify: true,
	}
	podip, err := getaliveByk8s()
	if err != nil {
		klog.Errorf("Failed to read from k8s client: %v", err)
		fromdns = true
	}
	for i := 0; i < maxNodes; i++ {
		if i == myIndex {
			continue
		}
		var ips []string
		podName := fmt.Sprintf("%s-%d", prefix, i)
		if !fromdns {
			hostip, ok := podip[podName]
			if !ok {
				klog.Infof("Failed to get pod '%s' ip from k8s client", podName)
			} else {
				ips, err = readIPFile(hostip)
			}
		} else {
			domain := fmt.Sprintf("%s.%s.%s.svc.cluster.local",
				podName, os.Getenv("SERVICE_NAME"), os.Getenv("POD_NAMESPACE"))
			ips, err = domainIPList(domain)
		}

		if err != nil || len(ips) == 0 {
			deadnames[podName] = struct{}{}
			continue
		}
		wg.Add(1)
		go func(iplist []string, podname string) {
			defer wg.Done()
			var ready bool
			// Check if node is healthy
			for _, ip := range iplist {
				ipport := net.JoinHostPort(ip, clientPort)
				if checkReadyz(ipport, tlscfg) {
					aliveEndpoints[podName] = iplist
					ready = true
					break
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if ready {
				aliveEndpoints[podname] = iplist
			}
		}(ips, podName)
	}
	wg.Wait()
	klog.Infof("Total endpoints info, alive: '%s', dead: '%s'", aliveEndpoints, deadnames)
	return aliveEndpoints, deadnames
}

func getCurrentClient(aliveEndpoints map[string][]string) (etcdcli.Cluster, error) {
	if len(aliveEndpoints) == 0 {
		return nil, fmt.Errorf("no alive endpoints found")
	}
	var endpoints []string
	for _, iplist := range aliveEndpoints {
		for _, ip := range iplist {
			endpoints = append(endpoints, net.JoinHostPort(ip, os.Getenv("CLIENT_PORT")))
		}
	}
	// Load client cert
	cert, err := tls.LoadX509KeyPair(os.Getenv("ETCDCTL_CERT"), os.Getenv("ETCDCTL_KEY"))
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
	return etcdcli.NewCluster(client), nil
}

func joinExistingCluster(client etcdcli.Cluster, myIPs []string, deadnames map[string]struct{}) (*exec.Cmd, error) {
	var (
		cluster     []string
		mypeers     = make([]string, 0, len(myIPs))
		addResp     *etcdcli.MemberAddResponse
		myMemberID  uint64
		podname     = os.Getenv("POD_NAME")
		peerport    = os.Getenv("PEER_PORT")
		nonLearn    int
		bgct        = context.Background()
		ctx, cancle = context.WithTimeout(bgct, 3*time.Second)
	)
	defer cancle()
	resp, err := client.MemberList(etcdcli.WithRequireLeader(ctx))
	if err != nil {
		return nil, err
	}

	for _, member := range resp.Members {
		if member.Name == podname {
			myMemberID = member.ID
			if util.DirExists(filepath.Join(os.Getenv("ETCD_DATA_DIR"), "member")) {
				if member.GetIsLearner() {
					_, err = client.MemberPromote(ctx, myMemberID)
					if err != nil {
						return nil, err
					}
				}
				klog.Info("memberdir exists, and i'm in cluster, start etcd")
				return startEtcd()
			}
		}
		if !member.IsLearner {
			nonLearn++
		}
		// remove not exist member
		if _, ok := deadnames[member.Name]; ok {
			klog.Infof("member '%s' had dead but in cluster, remove it", member.Name)
			_, err = client.MemberRemove(ctx, member.ID)
			if err != nil {
				return nil, err
			}
		}
	}
	// must remove member, because memberdir not exists, but member exist
	if myMemberID != 0 {
		klog.Infof("%s(%04d...) in cluster, but data not exist, should remove then rejoin", podname, myMemberID)
		_, err = client.MemberRemove(ctx, myMemberID)
		if err != nil {
			return nil, err
		}
	}
	os.RemoveAll(filepath.Join(os.Getenv("ETCD_DATA_DIR"), "member"))
	os.Setenv("ETCD_INITIAL_CLUSTER_STATE", "existing")
	for _, ip := range myIPs {
		mypeers = append(mypeers, fmt.Sprintf("https://%s:%s", ip, peerport))
	}
	ctx2, cancle2 := context.WithTimeout(bgct, 3*time.Second)
	defer cancle2()
	if len(resp.Members) != 1 {
		if nonLearn == 1 {
			klog.Info("member count > 1, but non learner is 1")
			return nil, fmt.Errorf("non learner is 1")
		}
		klog.Info("add member then start etcd")
		addResp, err = client.MemberAdd(ctx2, mypeers)
	} else {
		klog.Info("one master, start etcd as learner then prompt")
		addResp, err = client.MemberAddAsLearner(ctx2, mypeers)
	}
	if err != nil {
		return nil, err
	}
	for _, member := range addResp.Members {
		for _, url := range member.PeerURLs {
			if member.Name == "" {
				member.Name = podname
			}
			cluster = append(cluster, fmt.Sprintf("%s=%s", member.Name, url))
		}
	}
	os.Setenv("ETCD_INITIAL_CLUSTER", strings.Join(cluster, ","))
	cmd, err := startEtcd()
	if err != nil {
		return nil, err
	}
	if len(resp.Members) == 1 {
		count := 1
		for count < 10 {
			time.Sleep(2 * time.Second)
			_, err = client.MemberPromote(bgct, addResp.Member.ID)
			if err == nil {
				klog.Infof("promote member '%s' success", podname)
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

func initializeNewCluster(ips []string) (*exec.Cmd, error) {
	klog.Info("Initializing new etcd cluster")
	os.Setenv("ETCD_INITIAL_CLUSTER_STATE", "new")
	cluster := peerEndponits(
		map[string][]string{os.Getenv("POD_NAME"): ips},
		os.Getenv("PEER_PORT"),
		true, true)

	os.Setenv("ETCD_INITIAL_CLUSTER", strings.Join(cluster, ","))
	os.RemoveAll(filepath.Join(os.Getenv("ETCD_DATA_DIR"), "member"))
	klog.Info("Starting etcd with new cluster configuration")
	return startEtcd()
}

func startEtcd() (*exec.Cmd, error) {
	cmd := exec.Command(ETCDBIN)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

func waitExit(cmd *exec.Cmd) {
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)

	processExitChan := make(chan bool, 1)

	go func() {
		err := cmd.Wait()
		if err != nil {
			klog.Infof("进程退出并出现错误：%v", err)
		} else {
			klog.Info("进程成功退出。")
		}
		processExitChan <- true
	}()

	klog.Info("主进程正在等待信号或子进程退出...")
	select {
	case s := <-signalChan:
		klog.Infof("主进程收到信号：%v，准备退出。\n", s)
		if err := cmd.Process.Kill(); err != nil {
			klog.Fatalf("无法杀死子进程: %v", err)
		}
		klog.Info("已杀死子进程。")
	case <-processExitChan:
		klog.Info("子进程已退出，主进程也准备退出。")
	}
	klog.Info("主进程退出。")
	os.Exit(0)
}
