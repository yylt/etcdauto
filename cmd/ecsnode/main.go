package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	apisv1 "github.com/yylt/etcdauto/pkg/apis/v1"
	"github.com/yylt/etcdauto/pkg/cert"
	"github.com/yylt/etcdauto/pkg/controller"
	"github.com/yylt/etcdauto/pkg/util"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	metricserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

var (
	scheme = runtime.NewScheme()
	nsenv  = "NAMESPACE"
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(apisv1.AddToScheme(scheme))
}

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
			vcsRevision = vcsRevision[:8]
			klog.Infof("Build information, reversion: %s, time: %s, modified: %s", vcsRevision, vcsTime, vcsModified)
		}
	}
}

func main() {
	config := flag.String("config", "config.yaml", "配置文件")

	ctrl.RegisterFlags(flag.CommandLine)
	klog.InitFlags(flag.CommandLine)

	flag.Parse()
	cfg, err := LoadFromYaml(*config)
	if err != nil {
		klog.Fatalf("load config error: %v", err)
	}
	defaultns := os.Getenv(nsenv)
	if defaultns == "" {
		klog.Fatalf("Missing required environment variables: %v", nsenv)
	}
	err = ApplyDefault(cfg, defaultns)
	if err != nil {
		klog.Fatalf("apply default config error: %v", err)
	}
	klog.Infof("config: %v", cfg)
	printBuildInfo()

	ctx := SetupSignalHandler()

	// controller runtime
	restconfig := ctrl.GetConfigOrDie()
	ctrl.SetLogger(klog.NewKlogr())

	// Initialize certificates if enabled
	if cfg.Cert.Enabled {
		klog.Info("Certificate management is enabled, initializing certificates...")
		if err := initializeCertificates(ctx, restconfig, &cfg.Cert); err != nil {
			klog.Fatalf("failed to initialize certificates: %v", err)
		}
		klog.Info("Certificate initialization completed successfully")
	}

	mgr, err := ctrl.NewManager(restconfig, ctrl.Options{
		Scheme:                  scheme,
		LeaderElection:          true,
		LeaderElectionID:        "89x1.ecsnode.leader",
		LeaderElectionNamespace: defaultns,
		Metrics:                 metricserver.Options{BindAddress: "0"},
		HealthProbeBindAddress:  "0",
	})
	if err != nil {
		klog.Fatalf("initialize manager failed: %v", err)
	}
	if err := setupControllers(mgr, cfg); err != nil {
		klog.Fatalf("setup controllers failed: %v", err)
	}

	// start manager

	go func() {
		if err := mgr.Start(ctx); err != nil {
			klog.Fatalf("running manager failed: %v", err)
		}
	}()

	<-ctx.Done()
}

// setupControllers initializes and adds all controllers to the manager
func setupControllers(mgr ctrl.Manager, cfg *Config) error {
	pubsub := util.NewPubSub()
	ecsctl := controller.NewEcsNode(&cfg.EcsNode, pubsub, mgr)
	poctl := controller.NewPod(&cfg.PodConfig, pubsub, mgr)

	if err := mgr.Add(ecsctl); err != nil {
		return fmt.Errorf("add ecsnode controller failed: %w", err)
	}
	if err := mgr.Add(poctl); err != nil {
		return fmt.Errorf("add pod controller failed: %w", err)
	}

	if cfg.ServiceConfig.Name != "" {
		svcctl := controller.NewServiceSync(&cfg.ServiceConfig, poctl.ListPodHostIP, pubsub, mgr)
		if err := mgr.Add(svcctl); err != nil {
			return fmt.Errorf("add service controller failed: %w", err)
		}
	}
	if cfg.Configmap.Name != "" {
		cmctl := controller.NewConfigMapSync(&cfg.Configmap, pubsub, mgr)
		if err := mgr.Add(cmctl); err != nil {
			return fmt.Errorf("add configmap controller failed: %w", err)
		}
	}
	if cfg.Secret.Name != "" {
		secretctl := controller.NewSecretSync(&cfg.Secret, mgr)
		if err := mgr.Add(secretctl); err != nil {
			return fmt.Errorf("add secret controller failed: %w", err)
		}
	}

	return nil
}

// initializeCertificates initializes CA and member certificates
func initializeCertificates(ctx context.Context, restconfig *rest.Config, certCfg *CertConfig) error {
	// Create Kubernetes client
	clientset, err := kubernetes.NewForConfig(restconfig)
	if err != nil {
		return fmt.Errorf("failed to create kubernetes client: %w", err)
	}
	found := false
	for _, name := range certCfg.MemberSecretNamespaces {
		if name == certCfg.CASecretNamespace {
			found = true
			break
		}
	}
	if !found {
		certCfg.MemberSecretNamespaces = append(certCfg.MemberSecretNamespaces, certCfg.CASecretNamespace)
	}
	// Create secret manager for CA
	caSecretMgr := cert.NewSecretManager(clientset, certCfg.CASecretNamespace)

	// Try to load existing CA certificate
	ca, err := caSecretMgr.LoadCAFromSecret(ctx, certCfg.CASecretName)
	if err != nil {
		klog.Infof("CA certificate not found, generating new CA: %v", err)

		// Generate new CA certificate
		ca, err = cert.GenerateCA(&cert.CAConfig{
			CommonName:    certCfg.CommonName,
			Organization:  certCfg.Organization,
			ValidityYears: certCfg.ValidityYears,
		})
		if err != nil {
			return fmt.Errorf("failed to generate CA certificate: %w", err)
		}

		// Create CA secret
		if err := caSecretMgr.EnsureCASecret(ctx, certCfg.CASecretName, ca); err != nil {
			return fmt.Errorf("failed to create CA secret: %w", err)
		}

		klog.Infof("Successfully created CA certificate in secret %s/%s", certCfg.CASecretNamespace, certCfg.CASecretName)
	} else {
		klog.Infof("Loaded existing CA certificate from secret %s/%s", certCfg.CASecretNamespace, certCfg.CASecretName)
	}

	// Generate client certificate
	// Note: For a generic client certificate, we use a wildcard pattern
	// Specific pod certificates should be generated by the etcdcluster binary
	clientCert, err := cert.GenerateMemberCert(ca, &cert.MemberCertConfig{
		CommonName:   "etcd-client",
		Organization: certCfg.Organization,
		DNSNames: []string{
			"127.0.0.1", "localhost",
		},
		IPAddresses:   []string{"127.0.0.1"},
		ValidityYears: certCfg.ValidityYears,
	})
	if err != nil {
		return fmt.Errorf("failed to generate client certificate: %w", err)
	}

	// Create client secrets in all specified namespaces
	for _, namespace := range certCfg.MemberSecretNamespaces {
		memberSecretMgr := cert.NewSecretManager(clientset, namespace)

		// Create member secret
		if err := memberSecretMgr.EnsureMemberSecret(ctx, certCfg.MemberSecretName, ca, clientCert); err != nil {
			return fmt.Errorf("failed to create client secret in namespace %s: %w", namespace, err)
		}

		klog.Infof("Successfully ensured client certificate in secret %s/%s", namespace, certCfg.MemberSecretName)
	}

	return nil
}

func SetupSignalHandler() context.Context {
	ctx, cancel := context.WithCancel(context.Background())

	c := make(chan os.Signal, 2)
	signal.Notify(c, []os.Signal{os.Interrupt, syscall.SIGTERM}...)
	go func() {
		<-c
		cancel()
	}()

	return ctx
}
