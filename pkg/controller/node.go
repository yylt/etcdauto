package controller

import (
	"context"
	"fmt"
	"os"

	netinit "github.com/yylt/etcdauto/pkg/init"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

const (
	NodeAnnotationPrefix = "interface.etcdnode.io/"
)

type NodeConfig struct {
	Interfaces []string `json:"interfaces" yaml:"interfaces"`
}

func (c *NodeConfig) Valid() error {
	if len(c.Interfaces) == 0 {
		return fmt.Errorf("interfaces must not be empty")
	}
	return nil
}

type NodeCtrl struct {
	config   *NodeConfig
	client   ctrclient.Client
	nodeName string
}

func NewNode(config *NodeConfig, mgr manager.Manager) (*NodeCtrl, error) {
	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		return nil, fmt.Errorf("NODE_NAME environment variable is required")
	}

	nodeCtrl := &NodeCtrl{
		config:   config,
		client:   mgr.GetClient(),
		nodeName: nodeName,
	}

	err := ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Node{}).
		WithEventFilter(predicate.NewPredicateFuncs(func(object ctrclient.Object) bool {
			// Only watch the node this pod is running on
			return object.GetName() == nodeName
		})).
		Complete(nodeCtrl)
	if err != nil {
		return nil, fmt.Errorf("create node controller failed: %w", err)
	}

	return nodeCtrl, nil
}

func (h *NodeCtrl) NeedLeaderElection() bool {
	return false
}

func (h *NodeCtrl) Start(_ context.Context) error {
	// Trigger initial reconciliation
	klog.Infof("node controller started for node: %s", h.nodeName)
	return nil
}

func (h *NodeCtrl) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	klog.V(2).Infof("start node reconcile: %v", req.NamespacedName)

	node := &corev1.Node{}
	err := h.client.Get(ctx, req.NamespacedName, node)
	if err != nil {
		if apierrors.IsNotFound(err) {
			klog.Warningf("node %s not found", req.Name)
			return ctrl.Result{}, nil
		}
		klog.Errorf("get node %s failed: %v", req.Name, err)
		return ctrl.Result{}, err
	}

	// Check if annotations need to be added
	needsUpdate := false
	if node.Annotations == nil {
		node.Annotations = make(map[string]string)
	}

	// Get IPs for each interface
	for _, ifaceName := range h.config.Interfaces {
		annotationKey := NodeAnnotationPrefix + ifaceName

		// Skip if annotation already exists
		if _, exists := node.Annotations[annotationKey]; exists {
			klog.V(2).Infof("annotation %s already exists on node %s, skipping", annotationKey, node.Name)
			continue
		}

		// Get IPv4 address for this interface
		ip, err := netinit.GetInterfaceIP(ifaceName)
		if err != nil {
			klog.Warningf("failed to get IP for interface %s: %v", ifaceName, err)
			continue
		}

		// Add annotation
		node.Annotations[annotationKey] = ip
		needsUpdate = true
		klog.Infof("adding annotation %s=%s to node %s", annotationKey, ip, node.Name)
	}

	// Update node if needed
	if needsUpdate {
		err = h.client.Update(ctx, node)
		if err != nil {
			klog.Errorf("failed to update node %s annotations: %v", node.Name, err)
			return ctrl.Result{}, err
		}
		klog.Infof("successfully updated node %s with interface annotations", node.Name)
	} else {
		klog.V(2).Infof("no updates needed for node %s", node.Name)
	}

	return ctrl.Result{}, nil
}
