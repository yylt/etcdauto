# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Etcd Auto Cluster is a Kubernetes-native solution for automatic etcd cluster setup and management. The project consists of two main components:

1. **etcdcluster** (`cmd/etcdcluster/main.go`): A sidecar container that manages etcd cluster lifecycle - handles cluster initialization, member discovery, joining existing clusters, and automatic recovery
2. **ecsnode** (`cmd/ecsnode/main.go`): A Kubernetes controller that synchronizes node network information and pod states to enable dynamic etcd cluster configuration

## Development Commands

### Build
```bash
make build              # Build all binaries to out/bin/
make test-build         # Test compilation without output
```

### Testing
```bash
make test               # Run all tests
make test ARGS="-v"     # Run tests with verbose output
make test ARGS="-run TestName"  # Run specific test
make coverage           # Display coverage per function
make html-coverage      # Open coverage report in browser
```

### Linting
```bash
make lint               # Run golangci-lint with formatting
make lint-reports       # Generate lint report to out/lint.xml
```

### Security
```bash
make govulncheck        # Run vulnerability detection
```

### Other
```bash
make fmt                # Format code with go fmt
make tidy               # Clean up go.mod and go.sum
make docker             # Build Docker image
make ci                 # Run full CI pipeline (lint, test, govulncheck)
make clean              # Remove build artifacts
```

## Architecture

### Component Communication

The system uses a **PubSub pattern** (`pkg/util/pubsub.go`) for inter-controller communication:

- **EcsNode Controller** publishes node IP mappings to `EcsNodeTopic`
- **Pod Controller** subscribes to `EcsNodeTopic` and publishes pod state changes to `PodTopic`
- **Service/ConfigMap/Secret Controllers** subscribe to `PodTopic` for synchronization

### Key Controllers

#### EcsNode Controller (`pkg/controller/ecnode.go`)
- Watches `ECSNode` custom resources (defined in `pkg/apis/v1/types_ecsnode.go`)
- Maintains a mapping of master IPs to all node IPs (`hostips map[string]sets.Set[string]`)
- Filters network interfaces based on configuration (`interfaces` and `masterif` fields)
- Publishes IP mapping updates via PubSub when nodes change

#### Pod Controller (`pkg/controller/pod.go`)
- Watches Pods filtered by namespace and labels
- Subscribes to EcsNode updates to maintain `hostIpDict` (node IP to all IPs mapping)
- Tracks pod readiness state and host IP assignments
- Provides `ListPodHostIp()` method used by Service controller

#### Service Controller (`pkg/controller/service.go`)
- Synchronizes Kubernetes Service endpoints with pod host IPs
- Updates Service based on pod state changes from PubSub

#### ConfigMap/Secret Controllers
- Synchronize ConfigMap and Secret resources based on pod state changes
- Optional components (only initialized if configured)

### etcdcluster Binary

The `cmd/etcdcluster/main.go` binary is designed to run as a sidecar container in etcd StatefulSet pods:

**Initialization Flow:**
1. Parses pod name to extract prefix and index (e.g., `etcd-0` → prefix: `etcd`, index: `0`)
2. Discovers alive etcd endpoints by checking health of other pods via DNS or Kubernetes API
3. If no alive endpoints and index is 0: initializes new cluster
4. If alive endpoints exist: joins existing cluster as member or learner
5. Handles member promotion, removal of dead members, and data directory management

**Key Features:**
- Automatic cluster bootstrap for pod-0
- Dynamic member addition/removal based on pod lifecycle
- Learner promotion for single-master scenarios
- TLS certificate management via environment variables
- Reads node IP mappings from `NODEIP_DIR` files (written by ecsnode controller)

### Configuration

The ecsnode controller uses YAML configuration (`cmd/ecsnode/config.go`):

```yaml
ecsnode:
  interfaces: ["eth0", "eth1"]  # Network interfaces to track
  masterif: "eth0"               # Primary interface for master IP
  namespace: "default"

pod:
  labels:                        # Pod label selector
    app: etcd
  namespace: "default"

service:                         # Optional
  name: "etcd-service"
  namespace: "default"

configmap:                       # Optional
  name: "etcd-config"
  namespace: "default"

secret:                          # Optional
  name: "etcd-certs"
  namespace: "default"
```

### Custom Resources

**ECSNode** (`pkg/apis/v1/types_ecsnode.go`):
```go
type ECSNode struct {
    Spec EcsNodeSpec
}

type EcsNodeSpec struct {
    Endpoints []*endpoint  // Network endpoints with IP/device info
    Status    string       // Node status
    Hostname  string       // Node hostname
}
```

## Important Patterns

### Controller Initialization
All controllers follow a two-phase initialization:
1. **syncOnce/onceInit**: Performs initial list operation and populates internal state, closes `done` channel when complete
2. **Reconcile**: Handles incremental updates after initialization

Controllers check `<-h.done` to determine if initialization is complete.

### Leader Election
All controllers require leader election (`NeedLeaderElection() bool { return true }`). Only the leader performs reconciliation.

### Environment Variables (etcdcluster)
Required environment variables for the etcdcluster binary:
- `SERVICE_NAME`, `MAX`, `NODEIP_DIR`
- `POD_NAME`, `POD_NAMESPACE`, `POD_IPS`
- `ETCDCTL_CACERT`, `ETCDCTL_CERT`, `ETCDCTL_KEY`
- `ETCD_DATA_DIR`, `CLIENT_PORT`, `PEER_PORT`

## Code Generation

The project uses Kubernetes code generators for deepcopy methods:
- Generated files: `pkg/apis/v1/zz_generated.deepcopy.go`
- Markers: `+k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object`

## Dependencies

Key dependencies:
- `sigs.k8s.io/controller-runtime` - Controller framework
- `go.etcd.io/etcd/client/v3` - etcd client library
- `k8s.io/client-go` - Kubernetes client
- `github.com/cloudflare/cfssl` - TLS certificate management
- `go.uber.org/zap` - Structured logging (used in etcdcluster)
- `k8s.io/klog/v2` - Kubernetes logging (used in ecsnode)
