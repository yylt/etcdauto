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
	"github.com/yylt/etcdauto/pkg/controller"

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

	mgr, err := ctrl.NewManager(restconfig, ctrl.Options{
		Scheme:                  scheme,
		LeaderElection:          true,
		LeaderElectionID:        "84x1.ecsnode.leader",
		LeaderElectionNamespace: defaultns,
		Metrics:                 metricserver.Options{BindAddress: "0"},
		HealthProbeBindAddress:  "0",
	})
	if err != nil {
		klog.Fatalf("initialize manager failed: %v", err)
	}
	if err := setupControllers(mgr, cfg, restconfig, defaultns); err != nil {
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
func setupControllers(mgr ctrl.Manager, cfg *Config, restconfig *rest.Config, defaultns string) error {
	// Add node controller if interfaces are configured
	if len(cfg.NodeConfig.Interfaces) > 0 {
		nodectl, err := controller.NewNode(&cfg.NodeConfig, mgr)
		if err != nil {
			return fmt.Errorf("create node controller failed: %w", err)
		}
		if err := mgr.Add(nodectl); err != nil {
			return fmt.Errorf("add node controller failed: %w", err)
		}
	}

	// Add node sync controller if configmap is configured
	if cfg.NodeSync.ConfigMapName != "" {
		nodesyncctl, err := controller.NewNodeSync(&cfg.NodeSync, defaultns, mgr)
		if err != nil {
			return fmt.Errorf("create node sync controller failed: %w", err)
		}
		if err := mgr.Add(nodesyncctl); err != nil {
			return fmt.Errorf("add node sync controller failed: %w", err)
		}
	}

	if len(cfg.ServiceConfig.Ports) > 0 && len(cfg.ServiceConfig.PodLabels) > 0 {
		svcctl := controller.NewServiceSync(&cfg.ServiceConfig, mgr)
		if err := mgr.Add(svcctl); err != nil {
			return fmt.Errorf("add service controller failed: %w", err)
		}
	}
	if cfg.Cert.Enabled {
		// Create Kubernetes clientset for secret controller
		clientset, err := kubernetes.NewForConfig(restconfig)
		if err != nil {
			return fmt.Errorf("failed to create kubernetes client: %w", err)
		}

		secretctl := controller.NewSecretSync(&cfg.Cert, mgr, clientset)
		if secretctl != nil {
			if err := mgr.Add(secretctl); err != nil {
				return fmt.Errorf("add secret controller failed: %w", err)
			}
		}
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
