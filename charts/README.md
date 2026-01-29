# etcdauto Helm Chart

## Overview

This Helm chart provides a complete deployment solution for etcdauto, an automatic etcd cluster management system for Kubernetes.

## Chart Structure

```
charts/etcdauto/
├── Chart.yaml                      # Chart metadata
├── values.yaml                     # Default configuration values
├── values-production.yaml          # Production example values
├── README.md                       # Detailed documentation
├── QUICKSTART.md                   # Quick start guide
├── .helmignore                     # Files to ignore when packaging
└── templates/
    ├── NOTES.txt                   # Post-installation notes
    ├── _helpers.tpl                # Template helpers
    ├── ecsnode-configmap.yaml      # ECSNode configuration
    ├── ecsnode-deployment.yaml     # ECSNode controller deployment
    ├── ecsnode-rbac.yaml           # ECSNode RBAC resources
    ├── etcd-rbac.yaml              # etcd RBAC resources
    ├── etcd-service.yaml           # etcd services
    ├── etcd-statefulset.yaml       # etcd StatefulSet
    ├── nodeips-configmap.yaml      # Node IPs ConfigMap
    └── tls-secret.yaml             # TLS certificates secret
```

## Key Features

### Configurable Components

1. **Container Images**
   - Repository: Configurable via `image.repository`
   - Tag: Configurable via `image.tag`
   - Pull policy: Configurable via `image.pullPolicy`

2. **Ports**
   - Client port: Configurable via `etcd.ports.client` (default: 2479)
   - Peer port: Configurable via `etcd.ports.peer` (default: 2480)

3. **Network**
   - Host network: Configurable via `etcd.network.hostNetwork`
   - Network interfaces: Configurable via `etcd.network.interfaces`

4. **Storage**
   - Memory-backed: `etcd.storage.useMemory: true`
   - Persistent: `etcd.storage.useMemory: false` with PVC
   - Size: Configurable via `etcd.storage.size`
   - Storage class: Configurable via `etcd.storage.storageClassName`

5. **Resources**
   - CPU/Memory requests and limits for both etcd and init containers
   - Fully configurable via `etcd.resources` and `etcd.initResources`

6. **Replicas**
   - etcd replicas: Configurable via `etcd.replicaCount`
   - ECSNode replicas: Configurable via `ecsnode.replicaCount`

7. **Health Probes**
   - Liveness probe: Fully configurable
   - Readiness probe: Fully configurable
   - Can be disabled if needed

8. **Affinity and Scheduling**
   - Node affinity: Configurable via `etcd.affinity.nodeAffinity`
   - Pod anti-affinity: Configurable via `etcd.affinity.podAntiAffinity`
   - Node selector: Configurable via `etcd.nodeSelector`
   - Tolerations: Configurable via `etcd.tolerations`

9. **TLS/SSL**
   - Secret name: Configurable via `tls.secretName`
   - CA certificate: Can be provided in values or created separately
   - Auto-generation: Planned feature

10. **RBAC**
    - Fully configurable cluster roles and bindings
    - Can be disabled via `rbac.create: false`

## Installation Examples

### Basic Installation

```bash
helm install my-etcd ./charts/etcdauto -n openstack --create-namespace
```

### With Custom Image

```yaml
etcd:
  image:
    repository: my-registry.com/etcdcluster
    tag: v1.0.0
    pullPolicy: Always
```

### With Custom Ports

```yaml
etcd:
  ports:
    client: 2379
    peer: 2380
```

### With Persistent Storage

```yaml
etcd:
  storage:
    useMemory: false
    size: 50Gi
    storageClassName: fast-ssd
```

### With Custom Resources

```yaml
etcd:
  resources:
    requests:
      cpu: 1000m
      memory: 2Gi
    limits:
      cpu: 4000m
      memory: 8Gi
```

### Production Configuration

```yaml
etcd:
  replicaCount: 5

  image:
    repository: production-registry/etcdcluster
    tag: v1.0.0
    pullPolicy: IfNotPresent

  ports:
    client: 2379
    peer: 2380

  network:
    hostNetwork: false
    interfaces: "eth0,eth1"

  storage:
    useMemory: false
    size: 100Gi
    storageClassName: premium-ssd

  resources:
    requests:
      cpu: 2000m
      memory: 4Gi
    limits:
      cpu: 8000m
      memory: 16Gi

  affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
        - matchExpressions:
          - key: node-role.kubernetes.io/etcd
            operator: Exists
    podAntiAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        - labelSelector:
            matchExpressions:
              - key: component
                operator: In
                values:
                  - etcd
          topologyKey: kubernetes.io/hostname
```

## Validation

The chart has been validated with:

```bash
# Lint check
helm lint charts/etcdauto
# Result: 1 chart(s) linted, 0 chart(s) failed

# Template rendering
helm template test-release charts/etcdauto --namespace openstack
# Result: Successfully rendered all templates
```

## Upgrading from etcd-all-in-one.yaml

If you're migrating from the original `etcd-all-in-one.yaml`:

1. **Namespace**: Change from `openstack` to your desired namespace via `global.namespace`
2. **Image**: Update `image.repository` and `image.tag` to match your registry
3. **Ports**: Update `etcd.ports.client` and `etcd.ports.peer` if different
4. **Network**: Update `etcd.network.interfaces` to match your network setup
5. **Storage**: Choose between memory-backed or persistent storage
6. **Replicas**: Set `etcd.replicaCount` to your desired cluster size

## Maintenance

### Backup

```bash
# Backup etcd data
kubectl exec -n openstack etcd-server-0 -- etcdctl snapshot save /tmp/snapshot.db
kubectl cp openstack/etcd-server-0:/tmp/snapshot.db ./snapshot.db
```

### Restore

```bash
# Restore from snapshot
kubectl cp ./snapshot.db openstack/etcd-server-0:/tmp/snapshot.db
kubectl exec -n openstack etcd-server-0 -- etcdctl snapshot restore /tmp/snapshot.db
```

### Scaling

```bash
# Scale up
helm upgrade my-etcd ./charts/etcdauto --set etcd.replicaCount=5 -n openstack

# Scale down (be careful!)
helm upgrade my-etcd ./charts/etcdauto --set etcd.replicaCount=3 -n openstack
```

## Contributing

To contribute to this chart:

1. Make changes to templates or values
2. Run `helm lint charts/etcdauto`
3. Test with `helm template` and `helm install --dry-run`
4. Update documentation
5. Submit pull request

## License

This chart is part of the etcdauto project.
