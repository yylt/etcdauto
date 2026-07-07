# ecsnode 架构文档

## 概述

`ecsnode` 是一个 Kubernetes 控制器程序，负责自动管理 etcd 集群的网络配置和服务发现。它通过监控 ECSNode 自定义资源、Pod 状态和 Node 信息，动态同步 Service、Endpoints、ConfigMap、Secret 以及 StatefulSet 副本数。

## 入口 (`cmd/ecsnode/main.go`)

启动流程：

1. 通过 `-config` 参数加载 YAML 配置文件
2. 从 `NAMESPACE` 环境变量获取当前命名空间
3. 应用默认配置并校验
4. 创建 controller-runtime Manager（开启 Leader Election）
5. 调用 `setupControllers` 初始化各控制器
6. 启动 Manager

控制器初始化顺序：

1. 创建 PubSub 消息总线
2. 创建 **EcsNode 控制器**（发布者）
3. 创建 **Pod 控制器**（订阅 EcsNodeTopic，发布 PodTopic）
4. 若配置了 Service：创建 **Service 控制器**（订阅 PodTopic）
5. 若配置了 ConfigMap：创建 **ConfigMap 控制器**（订阅 EcsNodeTopic）
6. 若启用证书或节点管理：创建 Kubernetes clientset
7. 若启用证书：创建 **Secret 控制器**
8. 若配置了 StatefulSet：创建 **Node 控制器**

所有控制器均要求 Leader Election，仅当选的 Leader 执行实际调谐逻辑。

---

## 配置 (`cmd/ecsnode/config.go`)

### 顶层结构

```yaml
ecsnode:   # ECSNode 自定义资源监控配置
pod:       # Pod 监控配置
service:   # Service/Endpoints 同步配置
configmap: # ConfigMap 同步配置（可选）
cert:      # TLS 证书管理配置（可选）
node:      # Kubernetes Node 监控及 StatefulSet 扩缩容配置（可选）
```

### EcsNodeConfig (`ecsnode`)

| 字段 | 类型 | YAML Key | 必填 | 默认值 | 说明 |
|---|---|---|---|---|---|
| `Interfaces` | `[]string` | `interfaces` | 是 | - | 要追踪的网卡名列表，如 `["eth0", "eth1"]` |
| `MasterIf` | `string` | `masterif` | 是 | - | 主网卡名称，用于标识 master IP |
| `Namespace` | `string` | `namespace` | 是 | `$NAMESPACE` | ECSNode 资源所在的命名空间 |

### PodConfig (`pod`)

| 字段 | 类型 | YAML Key | 必填 | 默认值 | 说明 |
|---|---|---|---|---|---|
| `Lables` | `map[string]string` | `labels` | 否 | - | Pod 标签选择器，为空时匹配所有 Pod |
| `Namespace` | `string` | `namespace` | 是 | `$NAMESPACE` | 监控 Pod 的命名空间 |

### ServiceConfig (`service`)

| 字段 | 类型 | YAML Key | 必填 | 默认值 | 说明 |
|---|---|---|---|---|---|
| `Namespace` | `string` | `namespace` | 是 | `$NAMESPACE` | Service 所在的命名空间 |
| `Name` | `string` | `name` | 是 | - | Service 名称 |
| `PublishNotReady` | `bool` | `publish_notready` | 否 | `false` | 是否将未就绪 Pod 的宿主机 IP 也加入 Endpoints |
| `Ports` | `[]portInfo` | `ports` | 是 | - | 端口映射列表 |

**portInfo**：

| 字段 | 类型 | YAML Key | 默认值 | 说明 |
|---|---|---|---|---|
| `Protocol` | `string` | `protocol` | `"TCP"` | 协议：TCP 或 UDP |
| `Port` | `int32` | `port` | 必填 | Service 端口 |
| `TargetPort` | `int` | `targetPort` | 必填 | 容器目标端口 |

### ConfigMapConfig (`configmap`)

| 字段 | 类型 | YAML Key | 必填 | 默认值 | 说明 |
|---|---|---|---|---|---|
| `Namespace` | `string` | `namespace` | 是 | `$NAMESPACE` | ConfigMap 所在命名空间 |
| `Name` | `string` | `name` | 是 | - | ConfigMap 名称 |

仅当 `Name` 非空时才初始化 ConfigMap 控制器。

### CertConfig (`cert`)

| 字段 | 类型 | YAML Key | 必填 | 默认值 | 说明 |
|---|---|---|---|---|---|
| `Enabled` | `bool` | `enabled` | 否 | `false` | 总开关，为 false 时跳过整个 Secret 控制器 |
| `CASecretName` | `string` | `caSecretName` | 启用时必填 | - | 存放 CA 证书的 Secret 名称 |
| `CASecretNamespace` | `string` | `caSecretNamespace` | 否 | `$NAMESPACE` | CA Secret 所在命名空间 |
| `ClientSecretName` | `string` | `clientSecretName` | 启用时必填 | - | 客户端证书 Secret 名称 |
| `ClientSecretNamespaces` | `[]string` | `clientSecretNamespaces` | 启用时必填 | - | 需创建客户端证书 Secret 的命名空间列表 |
| `ValidityYears` | `int` | `validityYears` | 否 | `100` | 证书有效期（年） |
| `Organization` | `string` | `organization` | 否 | `"etcdauto"` | 证书 Organization 字段 |
| `CommonName` | `string` | `commonName` | 否 | `"etcdauto-ca"` | CA 证书 Common Name |

当前命名空间会自动追加到 `ClientSecretNamespaces` 中。

### NodeConfig (`node`)

| 字段 | 类型 | YAML Key | 必填 | 默认值 | 说明 |
|---|---|---|---|---|---|
| `ExcludeLabels` | `map[string]string` | `excludeLabels` | 否 | - | 排除的 Node 标签，匹配的节点不参与计数 |
| `StatefulSetName` | `string` | `statefulsetName` | 启用时必填 | - | 要管理的 StatefulSet 名称 |
| `StatefulSetNamespace` | `string` | `statefulsetNamespace` | 否 | `$NAMESPACE` | StatefulSet 所在命名空间 |
| `MinReplicas` | `int` | `minReplicas` | 否 | `3` | 节点数 < 5 时的最小副本数 |
| `MaxReplicas` | `int` | `maxReplicas` | 否 | `5` | 节点数 >= 5 时的最大副本数 |

仅当 `StatefulSetName` 非空时才初始化 Node 控制器。

---

## PubSub 消息总线 (`pkg/util/pubsub.go`)

基于内存的发布-订阅系统，用于控制器间通信。

- **Message**：`{Topic string, Data interface{}}`
- **Subscriber**：带缓冲通道（容量 10），通过 `GetMessage()` 获取接收端
- **PubSub**：两级映射 `topic -> subscriberID -> Subscriber`，线程安全（`sync.RWMutex`）
- **Publish** 为非阻塞发送，通道满时消息静默丢弃

### 主题定义 (`pkg/controller/const.go`)

| 常量 | 值 | 发布数据类型 |
|---|---|---|
| `EcsNodeTopic` | `"/ecsnode"` | `map[string]sets.Set[string]`（master IP → 所有节点 IP 集合） |
| `PodTopic` | `"/pod"` | `nil`（仅作信号通知） |
| `NodeTopic` | `"/node"` | 已定义但当前未被任何控制器使用 |

---

## 控制器

### 控制器依赖关系图

```
ECSNode CRs
    │
    ▼
[EcsNode 控制器]
    │
    ├── 发布 EcsNodeTopic ──┬──▶ [Pod 控制器] 订阅
    │                       │       │
    │                       │       │ 结合 Pod 状态，发布 PodTopic
    │                       │       │
    │                       │       └──▶ [Service 控制器] 订阅
    │                       │              │
    │                       │              └── 调用 PodCtrl.ListPodHostIP()
    │                       │                  管理 Headless Service + Endpoints
    │                       │
    │                       └──▶ [ConfigMap 控制器] 订阅
    │                              │
    │                              └── 将节点 IP 映射写入 ConfigMap
    │
[Secret 控制器] — 独立，无 PubSub，管理跨命名空间 TLS 证书
[Node 控制器]  — 独立，无 PubSub，根据 Node 数量扩缩 StatefulSet
```

### 1. EcsNode 控制器 (`pkg/controller/ecnode.go`)

**监控资源**：`ECSNode` 自定义资源

**作用**：提取 ECSNode 的网络端点信息，按配置的网卡名过滤，构建 `master IP → 所有节点 IP 集合` 的映射，并在映射变更时发布到 `EcsNodeTopic`。

**核心逻辑**：
- 首次 Reconcile 执行 `onceInit`：全量列出 ECSNode，填充 `hostips`
- 后续 Reconcile：对比新旧状态，变更时触发发布
- `getIpmap` 方法：遍历 `Spec.Endpoints`，按 `Interfaces` 过滤，以 `MasterIf` 对应的 IP 为 key

**PubSub**：发布到 `EcsNodeTopic`，数据为 `map[string]sets.Set[string]`

---

### 2. Pod 控制器 (`pkg/controller/pod.go`)

**监控资源**：`corev1.Pod`（按 namespace + labels 过滤）

**作用**：结合 Pod 状态与 EcsNode 提供的宿主机 IP 信息，维护每个 Pod 的就绪状态和宿主机 IP 集合映射，Pod 状态变更时发布信号到 `PodTopic`。

**核心逻辑**：
- `syncOnce`：先等待 `hostIPDict` 非空（来自 EcsNode 订阅），再全量列出 Pod 填充 `podInfo`
- 后续 Reconcile：对比 Pod 就绪状态和 HostIP，变更时发布 PodTopic 信号
- `stat(pod)`：检查 Pod 就绪状态（Phase=Running 且所有容器就绪），从 `hostIPDict` 查找宿主机 IP 集合
- `ListPodHostIP(ctx, publishNotReady)`：返回当前所有就绪 Pod 的宿主机 IP 列表，供 Service 控制器调用

**PubSub**：订阅 `EcsNodeTopic` → 发布 `PodTopic`

---

### 3. Service 控制器 (`pkg/controller/service.go`)

**监控资源**：`corev1.Service` 和 `corev1.Endpoints`（按指定 name/namespace 过滤）

**作用**：创建并维护 Headless Service（`ClusterIP: "None"`）及其 Endpoints，将 Pod 宿主机 IP 作为 Endpoints 地址，实现基于节点 IP 的 DNS 服务发现。

**核心逻辑**：
- 收到 PodTopic 信号或 Reconcile 漂移检测后触发 `syncService`
- 调用 `PodCtrl.ListPodHostIP()` 获取当前宿主机 IP 列表
- 通过 `controllerutil.CreateOrUpdate` 创建/更新 Service 和 Endpoints
- 失败时 5 秒后重试

**PubSub**：订阅 `PodTopic`

---

### 4. ConfigMap 控制器 (`pkg/controller/configmap.go`)

**监控资源**：`corev1.ConfigMap`（按指定 name/namespace 过滤）

**作用**：将 EcsNode 的 IP 映射同步到 ConfigMap 的 Data 字段，key 为 master IP，value 为该节点所有 IP 的逗号分隔列表。

**PubSub**：订阅 `EcsNodeTopic`

---

### 5. Secret 控制器 (`pkg/controller/secret.go`)

**监控资源**：`corev1.Secret`（按 `ClientSecretName` 在多个命名空间中过滤）、`corev1.Namespace`

**作用**：管理 TLS 证书生命周期。启动时加载或生成 CA 证书，并为所有配置的命名空间创建客户端证书 Secret。Reconcile 时验证现有证书，无效则重建。

**核心逻辑**：
- `loadOrCreateCA`：从配置的 Secret 加载 CA，不存在则生成新的
- `ensureClientCertificates`：为所有命名空间生成客户端证书并创建 Secret
- `validateClientCert`：校验客户端证书是否由当前 CA 签发、CA 是否变更
- `recreateClientSecret`：删除旧 Secret 并重建

**PubSub**：无，独立运行

---

### 6. Node 控制器 (`pkg/controller/node.go`)

**监控资源**：`corev1.Node`（排除匹配 `ExcludeLabels` 的节点）

**作用**：根据集群可用 Node 数量自动扩缩 StatefulSet 副本数。节点数 < 5 时缩至 `MinReplicas`（默认 3），>= 5 时扩至 `MaxReplicas`（默认 5）。

**核心逻辑**：
- 任何 Node 变更触发 `syncReplicas`
- 列出所有节点，排除标记节点后计数
- 对比当前 StatefulSet 副本数与期望值，不一致时更新 `Spec.Replicas`

**PubSub**：无，独立运行（`ps` 字段已存储但未使用）

---

## 完整配置示例

```yaml
ecsnode:
  interfaces: ["eth0", "eth1"]
  masterif: "eth0"
  namespace: "default"

pod:
  labels:
    app: etcd
  namespace: "default"

service:
  name: "etcd-service"
  namespace: "default"
  publish_notready: false
  ports:
    - protocol: "TCP"
      port: 2379
      targetPort: 2379
    - protocol: "TCP"
      port: 2380
      targetPort: 2380

configmap:
  name: "etcd-config"
  namespace: "default"

cert:
  enabled: true
  caSecretName: "etcd-ca"
  clientSecretName: "etcd-client"
  clientSecretNamespaces: ["ns1", "ns2"]
  validityYears: 100
  organization: "etcdauto"
  commonName: "etcdauto-ca"

node:
  statefulsetName: "etcd"
  minReplicas: 3
  maxReplicas: 5
  excludeLabels:
    node-role.kubernetes.io/master: ""
```
