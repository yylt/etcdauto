package controller

import (
	"context"
	"fmt"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type NodeSyncConfig struct {
	// Labels to filter nodes (any match - OR logic)
	Labels map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`

	// Master interface name to use as key
	MasterInterface string `json:"master_interface" yaml:"master_interface"`

	// ConfigMap to update
	ConfigMapName string `json:"configmap_name" yaml:"configmap_name"`
}

func (c *NodeSyncConfig) Valid() error {
	if c.MasterInterface == "" {
		return fmt.Errorf("master_interface must not be empty")
	}
	if c.ConfigMapName == "" {
		return fmt.Errorf("configmap_name must not be empty")
	}
	return nil
}

type NodeSyncCtrl struct {
	config    *NodeSyncConfig
	client    ctrclient.Client
	namespace string // Runtime namespace

	mu      sync.RWMutex
	nodeIPs map[string][]string // master-ip -> all-ips

	done chan struct{}
}

func NewNodeSync(config *NodeSyncConfig, namespace string, mgr manager.Manager) (*NodeSyncCtrl, error) {
	if err := config.Valid(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	if namespace == "" {
		return nil, fmt.Errorf("namespace must not be empty")
	}

	nodeSync := &NodeSyncCtrl{
		config:    config,
		client:    mgr.GetClient(),
		namespace: namespace,
		nodeIPs:   make(map[string][]string),
		done:      make(chan struct{}),
	}

	// Build controller with node and configmap watches
	builder := ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Node{}).
		WithEventFilter(predicate.NewPredicateFuncs(func(object ctrclient.Object) bool {
			// Filter by labels if configured (OR logic - any match)
			if len(config.Labels) > 0 {
				objLabels := object.GetLabels()
				if objLabels == nil {
					return false
				}
				// Check if any label matches
				for k, v := range config.Labels {
					if objLabels[k] == v {
						return true
					}
				}
				return false
			}
			return true
		}))

	// Watch the target configmap for changes
	builder = builder.Watches(
		&corev1.ConfigMap{},
		handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj ctrclient.Object) []reconcile.Request {
			cm, ok := obj.(*corev1.ConfigMap)
			if !ok {
				return nil
			}
			if cm.Name == config.ConfigMapName && cm.Namespace == namespace {
				// Trigger reconciliation for all nodes
				return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: "sync-trigger"}}}
			}
			return nil
		}),
	)

	err := builder.Complete(nodeSync)
	if err != nil {
		return nil, fmt.Errorf("create node sync controller failed: %w", err)
	}

	return nodeSync, nil
}

func (h *NodeSyncCtrl) NeedLeaderElection() bool {
	return true
}

func (h *NodeSyncCtrl) Start(_ context.Context) error {
	klog.Infof("node sync controller started")
	return nil
}

func (h *NodeSyncCtrl) syncOnce(ctx context.Context) (bool, error) {
	select {
	case <-h.done:
		return false, nil
	default:
	}

	nodes := &corev1.NodeList{}
	// List all nodes, then filter manually for OR logic
	err := h.client.List(ctx, nodes)
	if err != nil {
		klog.Errorf("failed to list nodes: %v", err)
		return false, err
	}

	h.mu.Lock()
	h.nodeIPs = make(map[string][]string)
	for i := range nodes.Items {
		node := &nodes.Items[i]

		// Apply label filter with OR logic
		if len(h.config.Labels) > 0 {
			matched := false
			nodeLabels := node.GetLabels()
			if nodeLabels != nil {
				for k, v := range h.config.Labels {
					if nodeLabels[k] == v {
						matched = true
						break
					}
				}
			}
			if !matched {
				continue
			}
		}

		masterIP, allIPs := h.extractNodeIPs(node)
		if masterIP != "" && len(allIPs) > 0 {
			h.nodeIPs[masterIP] = allIPs
		}
	}
	h.mu.Unlock()

	klog.Infof("init node sync success, nodes: %d, ip mappings: %d", len(nodes.Items), len(h.nodeIPs))
	close(h.done)
	return true, nil
}

func (h *NodeSyncCtrl) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	klog.V(2).Infof("start node sync reconcile: %v", req.NamespacedName)

	// Initialize once
	shouldUpdate, err := h.syncOnce(ctx)
	if err != nil {
		klog.Errorf("sync once failed: %v, requeue after: %s", err, RequeuDelay)
		return ctrl.Result{RequeueAfter: RequeuDelay}, nil
	}

	// If this is initial sync, update configmap
	if shouldUpdate {
		if err := h.updateConfigMap(ctx); err != nil {
			klog.Errorf("failed to update configmap: %v", err)
			return ctrl.Result{RequeueAfter: RequeuDelay}, nil
		}
		return ctrl.Result{}, nil
	}

	// Get all nodes and rebuild the mapping
	nodes := &corev1.NodeList{}
	err = h.client.List(ctx, nodes)
	if err != nil {
		klog.Errorf("failed to list nodes: %v", err)
		return ctrl.Result{}, err
	}

	// Rebuild node IP mapping
	newNodeIPs := make(map[string][]string)
	for i := range nodes.Items {
		node := &nodes.Items[i]

		// Apply label filter with OR logic
		if len(h.config.Labels) > 0 {
			matched := false
			nodeLabels := node.GetLabels()
			if nodeLabels != nil {
				for k, v := range h.config.Labels {
					if nodeLabels[k] == v {
						matched = true
						break
					}
				}
			}
			if !matched {
				continue
			}
		}

		masterIP, allIPs := h.extractNodeIPs(node)
		if masterIP != "" && len(allIPs) > 0 {
			newNodeIPs[masterIP] = allIPs
		}
	}

	// Check if mapping changed
	h.mu.Lock()
	changed := !equalIPMaps(h.nodeIPs, newNodeIPs)
	if changed {
		h.nodeIPs = newNodeIPs
	}
	h.mu.Unlock()

	if changed {
		klog.Infof("node IP mapping changed, updating configmap")
		if err := h.updateConfigMap(ctx); err != nil {
			klog.Errorf("failed to update configmap: %v", err)
			return ctrl.Result{RequeueAfter: RequeuDelay}, nil
		}
	}

	return ctrl.Result{}, nil
}

// extractNodeIPs extracts master IP and all IPs from node annotations
func (h *NodeSyncCtrl) extractNodeIPs(node *corev1.Node) (string, []string) {
	if node.Annotations == nil {
		return "", nil
	}

	var masterIP string
	allIPs := sets.New[string]()

	// Extract IPs from annotations
	for key, value := range node.Annotations {
		if !strings.HasPrefix(key, NodeAnnotationPrefix) {
			continue
		}

		interfaceName := strings.TrimPrefix(key, NodeAnnotationPrefix)
		ip := strings.TrimSpace(value)
		if ip == "" {
			continue
		}

		allIPs.Insert(ip)

		// Check if this is the master interface
		if interfaceName == h.config.MasterInterface {
			masterIP = ip
		}
	}

	if masterIP == "" {
		klog.V(2).Infof("node %s does not have master interface %s annotation", node.Name, h.config.MasterInterface)
		return "", nil
	}

	return masterIP, allIPs.UnsortedList()
}

// updateConfigMap updates the configmap with current node IP mapping
func (h *NodeSyncCtrl) updateConfigMap(ctx context.Context) error {
	h.mu.RLock()
	defer h.mu.RUnlock()

	// Get or create configmap
	cm := &corev1.ConfigMap{}
	cmKey := types.NamespacedName{
		Name:      h.config.ConfigMapName,
		Namespace: h.namespace,
	}

	err := h.client.Get(ctx, cmKey, cm)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to get configmap: %w", err)
		}

		// Create new configmap with master_ip as keys
		data := make(map[string]string)
		for masterIP, allIPs := range h.nodeIPs {
			// Join all IPs with comma
			data[masterIP] = strings.Join(allIPs, ",")
		}

		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      h.config.ConfigMapName,
				Namespace: h.namespace,
			},
			Data: data,
		}

		err = h.client.Create(ctx, cm)
		if err != nil {
			return fmt.Errorf("failed to create configmap: %w", err)
		}
		klog.Infof("created configmap %s/%s with node IP mapping", cm.Namespace, cm.Name)
		return nil
	}

	// Update existing configmap
	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}

	// Build new data
	newData := make(map[string]string)
	for masterIP, allIPs := range h.nodeIPs {
		newData[masterIP] = strings.Join(allIPs, ",")
	}

	// Check if data changed
	if equalStringMaps(cm.Data, newData) {
		klog.V(2).Infof("configmap %s/%s data unchanged, skipping update", cm.Namespace, cm.Name)
		return nil
	}

	cm.Data = newData
	err = h.client.Update(ctx, cm)
	if err != nil {
		return fmt.Errorf("failed to update configmap: %w", err)
	}
	klog.Infof("updated configmap %s/%s with node IP mapping", cm.Namespace, cm.Name)

	return nil
}

// equalIPMaps compares two IP maps for equality
func equalIPMaps(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}

	for key, aIPs := range a {
		bIPs, ok := b[key]
		if !ok {
			return false
		}

		aSet := sets.New(aIPs...)
		bSet := sets.New(bIPs...)
		if !aSet.Equal(bSet) {
			return false
		}
	}

	return true
}

// equalStringMaps compares two string maps for equality
func equalStringMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}

	for key, aVal := range a {
		bVal, ok := b[key]
		if !ok || aVal != bVal {
			return false
		}
	}

	return true
}
