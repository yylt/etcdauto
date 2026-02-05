package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"

	ctrclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type ListIPListFn func(ctx context.Context, publishNotready bool) ([]string, error)

type portInfo struct {
	Protocol   string `json:"protocol,omitempty" yaml:"protocol,omitempty"`
	Port       int32  `json:"port" yaml:"port"`
	TargetPort int    `json:"targetPort" yaml:"targetPort"`
}

type ServiceConfig struct {
	PublishNotReady bool              `json:"publish_notready,omitempty" yaml:"publish_notready,omitempty"`
	Namespace       string            `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	Name            string            `json:"name" yaml:"name"`
	Ports           []portInfo        `json:"ports" yaml:"ports"`
	PodLabels       map[string]string `json:"pod_labels,omitempty" yaml:"pod_labels,omitempty"`
}

func (c *ServiceConfig) Valid() error {
	if len(c.Namespace) == 0 || len(c.Name) == 0 {
		return fmt.Errorf("namespace and name must not be empty")
	}
	if len(c.Ports) == 0 {
		return fmt.Errorf("ports must not be empty")
	}
	if len(c.PodLabels) == 0 {
		return fmt.Errorf("pod_labels must not be empty")
	}
	return nil
}

type svcInfo struct {
	service  *corev1.Service
	endpoint *corev1.Endpoints
}

func (si *svcInfo) equalService(other *svcInfo) bool {
	if si.service == nil || other.service == nil {
		return false
	}
	if !deepEqual(si.service.ObjectMeta, other.service.ObjectMeta) {
		return false
	}
	if !si.ports().Equal(other.ports()) {
		return false
	}
	return true
}
func (si *svcInfo) equalEndpoint(other *svcInfo) bool {
	if si.endpoint == nil || other.endpoint == nil {
		return false
	}
	if !deepEqual(si.endpoint.ObjectMeta, other.endpoint.ObjectMeta) {
		return false
	}
	if !si.ips().Equal(other.ips()) {
		return false
	}
	return true
}

func (si *svcInfo) equal(other *svcInfo) bool {
	if si.equalService(other) && si.equalEndpoint(other) {
		return true
	}
	return false
}
func (si *svcInfo) deepCopy() *svcInfo {
	return &svcInfo{
		service:  si.service.DeepCopy(),
		endpoint: si.endpoint.DeepCopy(),
	}
}
func (si *svcInfo) ports() sets.Set[string] {
	var (
		ports = sets.New[string]()
	)
	for _, p := range si.service.Spec.Ports {
		ports.Insert(fmt.Sprintf("%s/%d", p.Protocol, p.Port))
	}
	return ports
}

func (si *svcInfo) ips() sets.Set[string] {
	var (
		addrs = sets.New[string]()
	)
	for _, a := range si.endpoint.Subsets {
		for _, addr := range a.Addresses {
			addrs.Insert(addr.IP)
		}
	}
	return addrs
}

func (si *svcInfo) String() string {
	ps := strings.Join(si.ports().UnsortedList(), ",")
	as := strings.Join(si.ips().UnsortedList(), ",")
	return fmt.Sprintf("service: %s(ports: %s, address: %s)", ctrclient.ObjectKeyFromObject(si.service), ps, as)
}

type podAddress struct {
	PodName  string
	NodeName string
	IPs      []string
}

type ServiceSync struct {
	config *ServiceConfig

	svcinfo *svcInfo

	client ctrclient.Client

	triggerch chan struct{}
}

func NewServiceSync(config *ServiceConfig, mgr manager.Manager) *ServiceSync {
	ss := &ServiceSync{
		config:    config,
		client:    mgr.GetClient(),
		triggerch: make(chan struct{}, 10),
	}
	ss.svcinfo = ss.serviceEndpointWithAddresses(nil)

	builder := ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Service{}).
		WithEventFilter(predicate.NewPredicateFuncs(func(object ctrclient.Object) bool {
			if _, ok := object.(*corev1.Pod); ok {
				return true
			}
			if object.GetName() == config.Name && object.GetNamespace() == config.Namespace {
				return true
			}
			return false
		}))

	// Watch Endpoints
	builder = builder.Watches(&corev1.Endpoints{},
		handler.EnqueueRequestsFromMapFunc(func(_ context.Context, object ctrclient.Object) []reconcile.Request {
			if object.GetName() == config.Name && object.GetNamespace() == config.Namespace {
				return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: config.Name, Namespace: config.Namespace}}}
			}
			return nil
		}))

	// Watch Pods with matching labels
	builder = builder.Watches(&corev1.Pod{},
		handler.EnqueueRequestsFromMapFunc(func(_ context.Context, object ctrclient.Object) []reconcile.Request {
			pod, ok := object.(*corev1.Pod)
			if !ok {
				return nil
			}
			// Check if pod is in the same namespace
			if pod.Namespace != config.Namespace {
				return nil
			}
			// Check if pod labels match
			podLabels := pod.GetLabels()
			if podLabels == nil {
				return nil
			}
			for k, v := range config.PodLabels {
				if podLabels[k] != v {
					return nil
				}
			}
			// Trigger service reconciliation
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: config.Name, Namespace: config.Namespace}}}
		}),
	)
	// Not require to watch node, becaut node ip will set in node annotations,
	// And Pod create depends on cert, which must slower than node ip set.
	err := builder.Complete(ss)
	if err != nil {
		klog.Fatalf("create service controller failed: %v", err)
	}
	return ss
}

func (s *ServiceSync) NeedLeaderElection() bool { return true }

func (s *ServiceSync) Start(ctx context.Context) error {
	go func(ctx context.Context) {
		for {
			select {
			case <-ctx.Done():
				close(s.triggerch)
				klog.Infof("controller had stop: %v", ctx.Err())
				return
			case _, ok := <-s.triggerch:
				if !ok {
					klog.Errorf("service trigger had close")
					return
				}
				klog.Info("service sync triggered")
				err := s.syncService(ctx)
				if err != nil {
					klog.Errorf("service sync failed: %v, will retry after %v", err, RequeuDelay)
					go func() {
						<-time.After(RequeuDelay)
						s.triggerch <- struct{}{}
					}()
				} else {
					klog.Infof("service sync success: %v", s.svcinfo)
				}
			}
		}
	}(ctx)
	return nil
}

func (s *ServiceSync) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var (
		err     error
		newinfo = svcInfo{
			service:  &corev1.Service{},
			endpoint: &corev1.Endpoints{},
		}
		trigger = false
		nsname  = types.NamespacedName{Name: s.config.Name, Namespace: s.config.Namespace}
	)

	defer func() {
		if trigger {
			s.triggerch <- struct{}{}
		}
	}()
	klog.V(2).Infof("start service reconcile: %v", req.NamespacedName)
	if err = s.client.Get(ctx, nsname, newinfo.service); err != nil {
		klog.Errorf("get service %s failed: %v", nsname, err)
		trigger = true
		return ctrl.Result{}, nil
	}
	if err = s.client.Get(ctx, nsname, newinfo.endpoint); err != nil {
		klog.Errorf("get endpoints %s failed: %v", nsname, err)
		trigger = true
		return ctrl.Result{}, nil
	}

	if !s.svcinfo.equal(&newinfo) {
		trigger = true
	}
	return ctrl.Result{}, nil
}

// syncService creates or updates a headless Service
func (s *ServiceSync) syncService(ctx context.Context) error {
	addresses, err := s.listPodAddresses(ctx)
	if err != nil {
		klog.Errorf("list pod addresses failed: %v", err)
		return err
	}

	var (
		desiredInfo = s.serviceEndpointWithAddresses(addresses)
		desiredCopy = desiredInfo.deepCopy()
	)

	result, err := controllerutil.CreateOrUpdate(ctx, s.client, desiredInfo.service, func() error {
		if desiredInfo.equalService(desiredCopy) {
			return nil
		}
		desiredInfo.service.Spec = desiredCopy.service.Spec
		for k, v := range desiredCopy.service.Annotations {
			if desiredInfo.service.Annotations == nil {
				desiredInfo.service.Annotations = make(map[string]string)
			}
			desiredInfo.service.Annotations[k] = v
		}
		return nil
	})
	if err != nil {
		klog.Errorf("create or patch service failed: %v", err)
		return err
	}
	klog.Infof("create or patch service: %s, result: %v", ctrclient.ObjectKeyFromObject(desiredInfo.service), result)

	result, err = controllerutil.CreateOrUpdate(ctx, s.client, desiredInfo.endpoint, func() error {
		if desiredInfo.equalEndpoint(desiredCopy) {
			return nil
		}
		desiredInfo.endpoint.Subsets = desiredCopy.endpoint.Subsets
		for k, v := range desiredCopy.endpoint.Annotations {
			if desiredInfo.endpoint.Annotations == nil {
				desiredInfo.endpoint.Annotations = make(map[string]string)
			}
			desiredInfo.endpoint.Annotations[k] = v
		}
		return nil
	})
	if err != nil {
		klog.Errorf("create or patch endpoint failed: %v", err)
		return err
	}
	klog.Infof("create or patch endpoint: %s, result: %v", ctrclient.ObjectKeyFromObject(desiredInfo.service), result)
	s.svcinfo = desiredCopy
	return nil
}

// listPodAddresses lists all pods matching pod_labels and gets their addresses from node annotations
func (s *ServiceSync) listPodAddresses(ctx context.Context) ([]podAddress, error) {
	var podList corev1.PodList

	// List pods in the namespace matching pod_labels
	listOpts := []ctrclient.ListOption{
		ctrclient.InNamespace(s.config.Namespace),
		ctrclient.MatchingLabels(s.config.PodLabels),
	}

	if err := s.client.List(ctx, &podList, listOpts...); err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	var addresses []podAddress
	for _, pod := range podList.Items {
		// Skip pods that are not ready unless PublishNotReady is true
		if !s.config.PublishNotReady && !isPodReady(&pod) {
			continue
		}

		// Get the node this pod is running on
		if pod.Spec.NodeName == "" {
			continue
		}

		// Get node to read annotations
		var node corev1.Node
		if err := s.client.Get(ctx, types.NamespacedName{Name: pod.Spec.NodeName}, &node); err != nil {
			klog.Warningf("failed to get node %s for pod %s: %v", pod.Spec.NodeName, pod.Name, err)
			continue
		}

		// Extract IPs from node annotations
		var ips []string
		for key, value := range node.Annotations {
			if strings.HasPrefix(key, NodeAnnotationPrefix) {
				ips = append(ips, value)
			}
		}

		if len(ips) > 0 {
			addresses = append(addresses, podAddress{
				PodName:  pod.Name,
				NodeName: pod.Spec.NodeName,
				IPs:      ips,
			})
		}
	}

	return addresses, nil
}

// isPodReady checks if a pod is ready
func isPodReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func (s *ServiceSync) serviceEndpointWithAddresses(addresses []podAddress) *svcInfo {
	meta := v1.ObjectMeta{
		Annotations: map[string]string{ServiceAnnoNotReadyKey: fmt.Sprintf("%t", s.config.PublishNotReady)},
		Name:        s.config.Name,
		Namespace:   s.config.Namespace,
	}

	service := &corev1.Service{
		ObjectMeta: meta,
		Spec: corev1.ServiceSpec{
			// Setting ClusterIP to "None" creates a headless Service.
			ClusterIP: "None",
			Ports:     transPorts(s.config.Ports),
		},
	}

	// Build endpoint addresses from pod addresses
	var addrs []corev1.EndpointAddress
	for _, podAddr := range addresses {
		// Create an endpoint address for each IP from node annotations
		for _, ip := range podAddr.IPs {
			addr := corev1.EndpointAddress{
				IP:       ip,
				NodeName: &podAddr.NodeName,
				TargetRef: &corev1.ObjectReference{
					Kind:      "Pod",
					Namespace: s.config.Namespace,
					Name:      podAddr.PodName,
				},
			}
			addrs = append(addrs, addr)
		}
	}

	endpoints := &corev1.Endpoints{
		ObjectMeta: meta,
	}
	if len(addrs) > 0 {
		endpoints.Subsets = []corev1.EndpointSubset{{Addresses: addrs}}
	}
	return &svcInfo{service: service, endpoint: endpoints}
}

func transPorts(pi []portInfo) []corev1.ServicePort {
	var ports []corev1.ServicePort = make([]corev1.ServicePort, len(pi))
	for i := range pi {
		ports[i].Protocol = corev1.ProtocolTCP
		if strings.ToLower(pi[i].Protocol) == "udp" {
			ports[i].Protocol = corev1.ProtocolUDP
		}
		ports[i].Port = pi[i].Port
		ports[i].TargetPort = intstr.FromInt(pi[i].TargetPort)
	}
	return ports
}

func deepEqual(svc1, svc2 v1.ObjectMeta) bool {
	if svc1.Name != svc2.Name {
		return false
	}
	if svc1.Namespace != svc2.Namespace {
		return false
	}
	if svc1.DeletionTimestamp != svc2.DeletionTimestamp {
		return false
	}
	if svc1.Annotations == nil || svc2.Annotations == nil {
		return false
	}
	// TODO
	for k, v := range svc1.Annotations {
		if svc2.Annotations[k] != v {
			return false
		}
	}
	return true
}
