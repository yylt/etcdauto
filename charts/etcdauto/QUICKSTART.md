# etcdauto Helm Chart Quick Start Guide

This guide will help you quickly deploy etcdauto using Helm.

## Prerequisites

1. Kubernetes cluster (1.19+)
2. Helm 3.0+
3. kubectl configured to access your cluster

## Quick Installation

### Step 1: Create TLS Certificates

First, you need to create CA certificates for etcd. You can use the provided script or create them manually.

#### Option A: Use existing certificates

If you already have CA certificates:

```bash
kubectl create secret generic etcd-ssl \
  --from-file=ca.pem=path/to/ca.pem \
  --from-file=ca-key.pem=path/to/ca-key.pem \
  -n openstack
```

#### Option B: Generate new certificates

```bash
# Install cfssl
go install github.com/cloudflare/cfssl/cmd/cfssl@latest
go install github.com/cloudflare/cfssl/cmd/cfssljson@latest

# Generate CA
cat > ca-csr.json <<EOF
{
  "CN": "etcd-ca",
  "key": {
    "algo": "ecdsa",
    "size": 256
  }
}
EOF

cfssl gencert -initca ca-csr.json | cfssljson -bare ca

# Create secret
kubectl create secret generic etcd-ssl \
  --from-file=ca.pem=ca.pem \
  --from-file=ca-key.pem=ca-key.pem \
  -n openstack --dry-run=client -o yaml | kubectl apply -f -
```

### Step 2: Install the Chart

```bash
# Create namespace
kubectl create namespace openstack

# Install with default values
helm install my-etcd ./charts/etcdauto -n openstack
```

### Step 3: Verify Installation

```bash
# Check pods
kubectl get pods -n openstack

# Check etcd cluster health
kubectl exec -n openstack etcd-server-0 -- /run/etcd/readyz.sh
```

## Customization

### Custom Values File

Create a `my-values.yaml`:

```yaml
etcd:
  replicaCount: 3
  ports:
    client: 2379
    peer: 2380
  storage:
    useMemory: false
    size: 10Gi
```

Install with custom values:

```bash
helm install my-etcd ./charts/etcdauto -f my-values.yaml -n openstack
```

### Common Configurations

#### 1. Change Replica Count

```yaml
etcd:
  replicaCount: 5
```

#### 2. Use Persistent Storage

```yaml
etcd:
  storage:
    useMemory: false
    size: 20Gi
    storageClassName: fast-ssd
```

#### 3. Custom Ports

```yaml
etcd:
  ports:
    client: 2379
    peer: 2380
```

#### 4. Custom Network Interfaces

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

#### 5. Disable Host Network

```yaml
etcd:
  network:
    hostNetwork: false
```

#### 6. Custom Resource Limits

```yaml
etcd:
  resources:
    requests:
      cpu: 500m
      memory: 1Gi
    limits:
      cpu: 4000m
      memory: 8Gi
```

## Accessing etcd

### From within the cluster

```bash
# Using service DNS
etcd-server-0.etcd-server.openstack.svc.cluster.local:2479
etcd-server-1.etcd-server.openstack.svc.cluster.local:2479
etcd-server-2.etcd-server.openstack.svc.cluster.local:2479
```

### Using etcdctl

```bash
# Exec into pod
kubectl exec -it -n openstack etcd-server-0 -- bash

# Inside the pod
source /run/etcd/env
etcdctl member list
etcdctl endpoint health
etcdctl endpoint status
```

## Upgrading

```bash
# Upgrade with new values
helm upgrade my-etcd ./charts/etcdauto -f my-values.yaml -n openstack

# Check upgrade status
helm status my-etcd -n openstack
```

## Uninstalling

```bash
# Uninstall the release
helm uninstall my-etcd -n openstack

# Clean up PVCs (if using persistent storage)
kubectl delete pvc -n openstack -l component=etcd
```

## Troubleshooting

### Check Pod Status

```bash
kubectl get pods -n openstack -l component=etcd
kubectl describe pod -n openstack etcd-server-0
```

### View Logs

```bash
# etcd logs
kubectl logs -n openstack etcd-server-0

# ECSNode logs
kubectl logs -n openstack -l app=ecsnode
```

### Check etcd Health

```bash
kubectl exec -n openstack etcd-server-0 -- /run/etcd/readyz.sh
echo $?  # Should be 0 if healthy
```

### Check etcd Cluster Status

```bash
kubectl exec -n openstack etcd-server-0 -- bash -c '
  source /run/etcd/env
  etcdctl member list
  etcdctl endpoint health
  etcdctl endpoint status --write-out=table
'
```

### Common Issues

#### 1. Pods not starting

Check events:
```bash
kubectl describe pod -n openstack etcd-server-0
```

#### 2. TLS certificate errors

Verify secret exists:
```bash
kubectl get secret -n openstack etcd-ssl
kubectl describe secret -n openstack etcd-ssl
```

#### 3. Network interface not found

Check available interfaces:
```bash
kubectl exec -n openstack etcd-server-0 -- ip addr show
```

Update values.yaml with correct interface names.

#### 4. Storage issues

Check PVC status:
```bash
kubectl get pvc -n openstack
kubectl describe pvc -n openstack data-etcd-server-0
```

## Advanced Configuration

### Using with Existing etcd Data

If you have existing etcd data, you can mount it:

```yaml
etcd:
  storage:
    useMemory: false
    existingClaim: my-existing-pvc
```

### Custom Affinity Rules

```yaml
etcd:
  affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
        - matchExpressions:
          - key: node-role.kubernetes.io/etcd
            operator: Exists
```

### Enable Client Service

```yaml
etcd:
  clientService:
    enabled: true
    name: etcd-client
    type: LoadBalancer
    port: 2379
```

## Next Steps

- Read the full [README.md](README.md) for detailed configuration options
- Check [values.yaml](values.yaml) for all available parameters
- Review [values-production.yaml](values-production.yaml) for production setup example

## Support

For issues and questions:
- GitHub: https://github.com/yylt/etcdauto
- Documentation: https://github.com/yylt/etcdauto/tree/main/docs
