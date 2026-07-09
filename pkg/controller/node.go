package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/yylt/etcdauto/pkg/util"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type NodeConfig struct {
	ExcludeLabels        map[string]string `json:"excludeLabels,omitempty" yaml:"excludeLabels,omitempty"`
	StatefulSetName      string            `json:"statefulsetName" yaml:"statefulsetName"`
	StatefulSetNamespace string            `json:"statefulsetNamespace,omitempty" yaml:"statefulsetNamespace,omitempty"`
	MinReplicas          int               `json:"minReplicas,omitempty" yaml:"minReplicas,omitempty"`
	MaxReplicas          int               `json:"maxReplicas,omitempty" yaml:"maxReplicas,omitempty"`
}

func (c *NodeConfig) Valid() error {
	if c.StatefulSetName == "" {
		return fmt.Errorf("statefulsetName must not be empty")
	}
	if c.StatefulSetNamespace == "" {
		return fmt.Errorf("statefulsetNamespace must not be empty")
	}
	if c.MinReplicas <= 0 {
		return fmt.Errorf("minReplicas must be positive")
	}
	if c.MaxReplicas <= 0 {
		return fmt.Errorf("maxReplicas must be positive")
	}
	if c.MinReplicas > c.MaxReplicas {
		return fmt.Errorf("minReplicas(%d) must not exceed maxReplicas(%d)", c.MinReplicas, c.MaxReplicas)
	}
	return nil
}

func (c *NodeConfig) SetDefaults() {
	if c.MinReplicas <= 0 {
		c.MinReplicas = 3
	}
	if c.MaxReplicas <= 0 {
		c.MaxReplicas = 5
	}
}

type NodeCtrl struct {
	mu sync.RWMutex

	config    *NodeConfig
	client    ctrclient.Client
	clientset kubernetes.Interface

	ps      *util.PubSub
	trigger chan struct{}

	labelSelector labels.Selector
}

func NewNodeCtrl(config *NodeConfig, ps *util.PubSub, mgr manager.Manager, clientset kubernetes.Interface) *NodeCtrl {
	n := &NodeCtrl{
		config:    config,
		client:    mgr.GetClient(),
		clientset: clientset,
		ps:        ps,
		trigger:   make(chan struct{}, 10),
	}

	if len(config.ExcludeLabels) > 0 {
		n.labelSelector = labels.SelectorFromSet(config.ExcludeLabels)
	}

	err := ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Node{}).
		WithEventFilter(predicate.NewPredicateFuncs(func(object ctrclient.Object) bool {
			if n.labelSelector == nil {
				return true
			}
			return !n.labelSelector.Matches(labels.Set(object.GetLabels()))
		})).
		Watches(
			&appsv1.StatefulSet{},
			handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj ctrclient.Object) []reconcile.Request {
				if obj.GetName() == config.StatefulSetName && obj.GetNamespace() == config.StatefulSetNamespace {
					return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: "__sts__", Namespace: config.StatefulSetNamespace}}}
				}
				return nil
			}),
		).
		Complete(n)
	if err != nil {
		klog.Fatal(err)
	}
	return n
}

func (n *NodeCtrl) NeedLeaderElection() bool { return true }

func (n *NodeCtrl) Start(ctx context.Context) error {
	go func() {
		for {
			select {
			case <-ctx.Done():
				close(n.trigger)
				klog.Infof("node controller had stop: %v", ctx.Err())
				return
			case _, ok := <-n.trigger:
				if !ok {
					return
				}
				n.syncReplicas(ctx)
			}
		}
	}()
	return nil
}

func (n *NodeCtrl) triggerSync() {
	select {
	case n.trigger <- struct{}{}:
	default:
		klog.V(2).Infof("trigger channel full, skipping")
	}
}

func (n *NodeCtrl) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if req.Name == "__sts__" {
		klog.V(2).Infof("statefulset %s/%s changed, trigger sync", n.config.StatefulSetNamespace, n.config.StatefulSetName)
		n.triggerSync()
		return ctrl.Result{}, nil
	}

	var (
		node corev1.Node
	)

	klog.V(2).Infof("start node reconcile: %v", req.NamespacedName)

	err := n.client.Get(ctx, req.NamespacedName, &node)

	if err != nil {
		if apierrors.IsNotFound(err) {
			klog.Infof("node %s deleted", req.Name)
			n.triggerSync()
			return ctrl.Result{}, nil
		}
		klog.Errorf("get node %s failed: %v", req.Name, err)
		return ctrl.Result{}, nil
	}

	if node.DeletionTimestamp != nil {
		klog.Infof("node %s is deleting", req.Name)
		n.triggerSync()
		return ctrl.Result{}, nil
	}

	n.triggerSync()
	return ctrl.Result{}, nil
}

func (n *NodeCtrl) syncReplicas(ctx context.Context) {
	nodes := corev1.NodeList{}
	err := n.client.List(ctx, &nodes)
	if err != nil {
		klog.Errorf("list nodes failed: %v", err)
		return
	}

	count := n.countNodes(&nodes)
	klog.Infof("node count: %d (total: %d)", count, len(nodes.Items))

	var desiredReplicas int32
	if count < n.config.MaxReplicas {
		desiredReplicas = int32(n.config.MinReplicas)
	} else {
		desiredReplicas = int32(n.config.MaxReplicas)
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	ns := n.config.StatefulSetNamespace
	name := n.config.StatefulSetName

	for i := range 3 {
		sts, err := n.clientset.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				klog.V(2).Infof("statefulset %s/%s not found, skip sync", ns, name)
				return
			}
			klog.Errorf("get statefulset %s/%s failed: %v", ns, name, err)
			return
		}

		if sts.Spec.Replicas == nil {
			klog.Errorf("statefulset %s/%s replicas is nil", ns, name)
			return
		}

		current := *sts.Spec.Replicas
		if current == desiredReplicas {
			klog.V(2).Infof("statefulset %s/%s current=%d, no change needed", ns, name, current)
			return
		}

		sts.Spec.Replicas = &desiredReplicas
		_, err = n.clientset.AppsV1().StatefulSets(ns).Update(ctx, sts, metav1.UpdateOptions{})
		if err == nil {
			klog.Infof("statefulset %s/%s current=%d -> expect=%d", ns, name, current, desiredReplicas)
			return
		}
		if apierrors.IsConflict(err) {
			klog.V(2).Infof("statefulset %s/%s update conflict, retrying (%d/3)", ns, name, i+1)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		klog.Errorf("update statefulset %s/%s current=%d -> expect=%d failed: %v", ns, name, current, desiredReplicas, err)
		return
	}
	klog.Errorf("statefulset %s/%s update failed after 3 retries", ns, name)
}

func (n *NodeCtrl) countNodes(nodes *corev1.NodeList) int {
	if n.labelSelector == nil {
		return len(nodes.Items)
	}

	count := 0
	for _, node := range nodes.Items {
		if !n.labelSelector.Matches(labels.Set(node.Labels)) {
			count++
		}
	}
	return count
}
