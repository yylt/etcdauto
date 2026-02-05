# etcdauto Helm Chart

This Helm chart deploys etcdauto, an automatic etcd cluster management solution for Kubernetes.

## Prerequisites

- Kubernetes 1.19+
- Helm 3.0+
- PV provisioner support in the underlying infrastructure (if using persistent storage)

## Installing the Chart

To install the chart with the release name `my-etcd`:

```bash
helm install my-etcd ./charts/etcdauto -n openstack --create-namespace
```

## Uninstalling the Chart

To uninstall/delete the `my-etcd` deployment:

```bash
helm delete my-etcd -n openstack
```

## Configuration

The following table lists the configurable parameters of the etcdauto chart and their default values.

### Global Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `global.namespace` | Namespace to deploy resources | `openstack` |

### etcdnode Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `etcdnode.enabled` | Enable etcdnode controller | `true` |
| `etcdnode.replicaCount` | Number of etcdnode replicas | `1` |
| `etcdnode.image.repository` | etcdnode image repository | `hub.easystack.cn/multiarch/etcdcluster` |
| `etcdnode.image.tag` | etcdnode image tag | `v0.0.1` |
| `etcdnode.image.pullPolicy` | Image pull policy | `IfNotPresent` |
| `etcdnode.resources.requests.cpu` | CPU request | `100m` |
| `etcdnode.resources.requests.memory` | Memory request | `128Mi` |

### etcd Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `etcd.enabled` | Enable etcd StatefulSet | `true` |
| `etcd.replicaCount` | Number of etcd replicas | `3` |
| `etcd.image.repository` | etcd image repository | `hub.easystack.cn/multiarch/etcdcluster` |
| `etcd.image.tag` | etcd image tag | `v0.0.1` |
| `etcd.ports.client` | Client port | `2479` |
| `etcd.ports.peer` | Peer port | `2480` |
| `etcd.network.hostNetwork` | Use host network | `true` |
| `etcd.network.interfaces` | Network interfaces | `br-roller,br-mgmt,br-storagepub` |
| `etcd.storage.useMemory` | Use memory-backed storage | `true` |
| `etcd.storage.size` | PVC size (if not using memory) | `10Gi` |
| `etcd.resources.requests.cpu` | CPU request | `200m` |
| `etcd.resources.requests.memory` | Memory request | `256Mi` |

### TLS Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `tls.enabled` | Enable TLS | `true` |
| `tls.secretName` | TLS secret name | `etcd-ssl` |
| `tls.caCert` | CA certificate (base64) | `""` |
| `tls.caKey` | CA key (base64) | `""` |

### RBAC Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `rbac.create` | Create RBAC resources | `true` |

## Examples

### Basic Installation

```bash
helm install my-etcd ./charts/etcdauto
```

### Custom Values

Create a `custom-values.yaml`:

```yaml
etcd:
  replicaCount: 5
  ports:
    client: 2379
    peer: 2380
  resources:
    requests:
      cpu: 500m
      memory: 512Mi
    limits:
      cpu: 2000m
      memory: 4Gi
```

Install with custom values:

```bash
helm install my-etcd ./charts/etcdauto -f custom-values.yaml
```

### Using Persistent Storage

```yaml
etcd:
  storage:
    useMemory: false
    size: 20Gi
    storageClassName: fast-ssd
```

### Custom Network Interfaces

```yaml
etcd:
  network:
    interfaces: "eth0,eth1,eth2"

ecsnode:
  config:
    interfaces:
      - eth0
      - eth1
      - eth2
    masterInterface: eth0
```

### Disable Host Network

```yaml
etcd:
  network:
    hostNetwork: false
```

## Upgrading

To upgrade the chart:

```bash
helm upgrade my-etcd ./charts/etcdauto -f custom-values.yaml
```

## Troubleshooting

### Check Pod Status

```bash
kubectl get pods -n openstack -l component=etcd
```

### View Logs

```bash
kubectl logs -n openstack etcd-server-0
kubectl logs -n openstack -l app=ecsnode
```

### Check etcd Health

```bash
kubectl exec -n openstack etcd-server-0 -- /run/etcd/readyz.sh
```

## License

This chart is provided as-is.
