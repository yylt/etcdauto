# Cluster Discovery & Brain Split Detection Fix Proposal

## Problem Statement

当 `etcd-server-0` 重启后（pod 被删除后由 StatefulSet 重建），启动时存在以下问题：

1. **探测其他节点失败**：`DiscoverEndpoints` 的 health check 无法正确感知已运行的 `etcd-server-1`、`etcd-server-2`，导致 `ReadyNodes == 0`。
2. **脑裂检测失效**：etcd-0 以单节点初始化新集群后，`StartBrainSplitChecker` 同样探测不到其他节点，无法触发脑裂检测，etcd-0 以独立集群持续运行。

最终结果：两个独立的 etcd 集群同时存在，数据分裂。

## Deployment Model

理解部署模型对分析至关重要：

```
StatefulSet: etcd-server (replicas=3, hostNetwork=true, OrderedReady)
  ├── etcd-server-0  → master node A, interfaces: br-roller/br-mgmt/br-storagepub
  ├── etcd-server-1  → master node B
  └── etcd-server-2  → master node C

ConfigMap: nodeips (由 NodeSync Controller 独立维护，与 etcd 进程无关)
  ├── 10.0.0.1  →  "10.0.0.1,192.168.1.1,172.16.0.1"   (etcd-0)
  ├── 10.0.0.2  →  "10.0.0.2,192.168.1.2,172.16.0.2"   (etcd-1)
  └── 10.0.0.3  →  "10.0.0.3,192.168.1.3,172.16.0.3"   (etcd-2)

Volume: /run/nodeip → ConfigMap nodeips (所有节点文件始终存在)
```

**关键事实**：`nodeIPDir` 是静态 ConfigMap，**永远存在所有节点的 IP 文件**，无论 etcd 进程是否在运行。健康检查是判断节点是否存活的**唯一手段**。

## Root Cause Analysis

代码路径：`cmd/etcdcluster/main.go:runClusterLoop` → `pkg/cluster/discovery.go:DiscoverEndpoints` → `pkg/cluster/manager.go`

### etd-server-0 重启的完整时序

```
1. etcd-server-0 pod 被删除
2. StatefulSet 重建 etcd-server-0，新 pod 启动
3. etcdcluster 进程启动:
   a. loadConfig → 解析配置，提取本机 IP
   b. initializeEnvironment → 建目录、生成证书、写环境变量、写健康检查脚本
   c. runClusterLoop 进入主循环:
      i.   DiscoverEndpoints():
           - 读取 /run/nodeip/ 下的所有文件
           - 文件中始终有 etcd-1, etcd-2 的 IP 文件
           - 逐个调用 checkSingleEndpoint() 探测
           - 如果任一健康检查失败 → 标记为 DeadNames
           - AliveEndpoints 为空
      ii.  AliveEndpoints == 0, MyIndex == 0:
           - InitializeNewCluster() → 启动只有自己的集群 → 脑裂!
           - StartBrainSplitChecker():
             - checkBrainSplit() 同样读 nodeIPDir, checkSingleEndpoint()
             - 同样探测不到其他节点 → Reason: "no other nodes"
             - "no other nodes" 不增加 successCount, 也不触发脑裂
             - brainSplitResultCh 永远收不到数据
             - etcd-0 永久运行在单节点集群状态
```

### 问题 1（🔴 核心）：健康检查失败原因被统一吞没，无法区分"节点不健康"与"节点不可达"

```go
// discovery.go:165-190
func checkSingleEndpoint(ctx context.Context, endpoint string, tlscfg *tls.Config) bool {
    client, err := etcdcli.New(cliconfig)
    if err != nil {
        return false  // ← 原因 A：TLS 握手失败、DNS/网络不通
    }
    defer client.Close()

    _, err = client.Get(ctx, "health")
    if err == nil || errors.Is(err, rpctypes.ErrPermissionDenied) {
        resp, err := client.AlarmList(ctx)
        if err == nil && len(resp.Alarms) == 0 {
            return true
        }
    }
    return false  // ← 原因 B：超时、NoLeader、NOSPACE、网络断开
}
```

所有非成功路径统一返回 `false`。对于 `nodeIPDir` 始终有文件条目的部署模型，这意味着一旦其他节点的 etcd 端口不可达（正常情况：pod 正在重启、网络抖动、leader 选举中），就会立即被视为 dead。

**但问题在于**：由于 nodeIPDir 是 ConfigMap，它始终有所有节点的条目，**不存在"目录为空即没有其他节点"的情况**。etcd-0 无法知道自己是否是第一个启动的节点——这只能通过实际探测其他节点是否在运行来判断。

然而当前逻辑的问题是：**探测失败一次就立刻初始化新集群**，没有任何重试或确认机制。`runClusterLoop` 中一旦 `AliveEndpoints == 0`，etcd-0 立即执行 `InitializeNewCluster`。

### 问题 2（🔴 核心）：BrainSplitChecker 对"其他节点不可达"无感知

```go
// manager.go:495-498
if len(otherEndpoints) == 0 {
    return BrainSplitCheckResult{
        BrainSplitDetected: false,
        Reason:             "no other nodes",
    }
}
```

这里 `"no other nodes"` 在 `StartBrainSplitChecker` 中：

```go
// manager.go:425-437
if result.Reason == "verified" {
    successCount++
    if successCount >= cfg.MaxChecks {
        resultCh <- BrainSplitCheckResult{...Reason: "all checks passed"...}
        return
    }
}
// "no other nodes" 既不被计入，也不触发脑裂
```

配置 `MaxChecks=3, CheckPeriod=5s`：
- ticker 每 5 秒触发一次 `checkBrainSplit`
- 如果始终探测不到其他节点，每次都返回 `"no other nodes"`
- `successCount` 始终为 0，checker 永远不结束也不发结果
- `brainSplitResultCh` 永远收不到数据
- `waitExitWithBrainSplitCheck` 的第三个 case 永远不触发
- **etcd-0 以单节点集群永久运行**

**更深层的问题**：`BrainSplitChecker` 的逻辑假设"没有其他节点 = 集群确实只有我自己"，这在当前部署模型中永远不成立——因为 nodeIPDir 始终有所有节点的文件。`"no other nodes"` 恰恰意味着**有其他节点但全部不可达**，这正是脑裂的迹象。

### 问题 3（🟡）：runClusterLoop 无退避延迟

```go
// main.go:345-413
for {
    ...
    switch len(endpointInfo.AliveEndpoints) {
    case 0:
        if cfg.MyIndex != 0 {
            klog.Warningf(...)
            break  // ← 跳出 switch，for 立即再次迭代，无延迟
        }
        // InitializeNewCluster → 阻塞在 waitExitWithBrainSplitCheck
    default:
        // join 失败 → break → for 立即再次迭代，无延迟
    }
}
```

非 pod-0 等待期间、join 失败后，形成 tight loop，造成日志风暴和 CPU 浪费。

### 问题 4（🟡）：processMembers 中 Learner 计入 alive 计数

```go
// manager.go:164-168
if shouldRemove {
    m.removeDeadMember(ctx, client, member)
} else {
    aliveNumExceptMe++  // ← Learner 也被计入 voting 成员数
}
```

`addMemberAndStart` 用 `alive` 决定加入策略：
- `alive == 1` → 以 Learner 加入（保守策略，防止 quorum 丢失）
- `alive > 1` → 以正式成员加入
- `alive == 0` → 报错

但 `alive` 变量的语义注释是 "non-learner members"，实际却包含了 Learner。如果集群中只有 1 个 voting 成员 + 1 个 Learner，`alive=2`，新节点会以正式成员加入，这是正确的。但如果集群中只有 1 个 Learner（无 voting），`alive=1`，新节点尝试以 Learner 加入 → `MemberAddAsLearner` 会因无 quorum 而失败。

**结论**：应分别追踪 `votingNumExceptMe` 和 `learnerNumExceptMe`，或在 `addMemberAndStart` 中使用正确的计数。

### 问题 5（🟢）：checkSingleEndpoint 对 NOSPACE 告警的处理

```go
if err == nil && len(resp.Alarms) == 0 {
    return true
}
```

`NOSPACE` 告警的 etcd 节点仍然可以服务读请求和新成员加入操作，但当前逻辑直接标记为 dead。本项目不关注 NOSPACE 场景，将 NOSPACE 归入 `HealthNotReady` 即可。

### 问题 6（🟡）：handleExistingMemberWithData 中 Learner 提升失败死循环

```go
// manager.go:206-217
func (m *Manager) handleExistingMemberWithData(...) (*exec.Cmd, error) {
    if myIsLearner {
        err := m.promoteLearner(client, myMemberID)
        if err != nil {
            return nil, fmt.Errorf("failed to promote learner: %w", err)
        }
    }
    return m.startEtcd(etcdBin)
}
```

提升失败 → 返回 error → `runClusterLoop` 重试 → `DiscoverEndpoints` 发现存活节点 → `JoinExistingCluster` → `processMembers` 找到自己（`myMemberID != 0`）→ `dataDir` 存在 → 再次进入 `handleExistingMemberWithData` → 再次提升失败 → 死循环。

对比 `addMemberAndStart` 中对 learner 提升失败的处理（先 SIGTERM 进程再返回 error），这里缺少清理逻辑。

### 关于 `buildClusterConfig` 修改 protobuf 对象

用户澄清：新加的 member 返回时 `Name` 为空，需要手动设置。这是有意为之，不是 bug。

但可以通过局部变量替代直接修改传入结构体，避免潜在副作用：

```go
// 当前
if member.Name == "" {
    member.Name = m.cfg.PodName  // 直接修改
}

// 建议
name := member.Name
if name == "" {
    name = m.cfg.PodName
}
```

## Proposed Fix

### Fix 1：checkSingleEndpoint 返回结构化健康状态（🔴 核心）

将返回值从 `bool` 改为细化枚举：

```go
type EndpointHealth int

const (
    HealthUnreachable EndpointHealth = iota // 无法连接：网络不通（TLS 握手失败、连接拒绝、超时）
    HealthHealthy                           // 完全健康：Get 成功 + Alarm 为空
    HealthNotReady                          // 连接成功但 etcd 未就绪（NoLeader、正在启动、NOSPACE、其他告警等）
)
```

| 状态 | 触发条件 | 含义 |
|------|---------|------|
| `HealthUnreachable` | `etcdcli.New` 失败、连接被拒绝、TCP 超时 | 网络不通，节点不可达 |
| `HealthHealthy` | `Get` 成功 且 `AlarmList` 为空 | 节点正常运行 |
| `HealthNotReady` | `Get` 返回 `ErrGRPCNoLeader` 等、`AlarmList` 有告警（含 NOSPACE）、其他非网络错误 | etcd 进程在运行但未就绪或不健康 |

**注意**：本项目不关注 NOSPACE 问题，不做 `HealthDegraded` 区分。`HealthUnreachable` 仅用于网络不通的场景，其他所有非健康情况统一归入 `HealthNotReady`。

### Fix 2：DiscoverEndpoints 细化分类（🔴 核心）

```go
type EndpointInfo struct {
    ReadyNodes    map[string][]string // masterIP → []IP（HealthHealthy）
    NotReadyNodes map[string]struct{} // masterIP 存在文件但 HealthNotReady
    DeadNames     map[string]struct{} // 文件读取失败等
}
```

- `HealthHealthy` → `ReadyNodes`
- `HealthNotReady` → `NotReadyNodes`
- `HealthUnreachable` → 既不在 ReadyNodes 也不在 NotReadyNodes

**关键认识**：nodeIPDir 是静态 ConfigMap，所有节点的 IP 文件永远存在。`HealthUnreachable`（网络不通）意味着其他节点确实没有运行 etcd 进程——否则网络通畅时至少应该返回 `HealthNotReady` 或 `HealthHealthy`。因此 `len(ReadyNodes) == 0 && len(NotReadyNodes) == 0` 即可判断"无其他集群存在"，可以初始化新集群。

### Fix 3：runClusterLoop 决策逻辑修正（🔴 核心）

```go
switch {
case len(info.ReadyNodes) > 0:
    // 有存活节点 → join
    joinCluster(client, ...)

case len(info.ReadyNodes) == 0 && len(info.NotReadyNodes) > 0:
    // 有其他节点但 etcd 未就绪（正在启动中）
    // 不需要退避，直接重试加入集群
    klog.Infof("Other nodes are not ready yet, retrying immediately")
    // 不 sleep，直接 continue

case len(info.ReadyNodes) == 0 && len(info.NotReadyNodes) == 0:
    // 没有存活节点，也没有 NotReady 节点（即全部 HealthUnreachable 或真的没有其他节点文件）
    // 由于 nodeIPDir 是静态 ConfigMap，所有节点的 IP 文件永远存在：
    //   全部 HealthUnreachable → 其他节点确实没有运行 etcd → 无其他集群 → 可初始化
    if cfg.MyIndex == 0 {
        InitializeNewCluster() + BrainSplitCheck
    } else {
        // 等待 pod-0 先启动
        sleep(backoff)
    }
}
```

**关键改变**：
- NotReady 分支**优先判断**：有节点在启动中 → 直接重试不退避
- `ReadyNodes == 0 && NotReadyNodes == 0` 即为"无其他集群"，可初始化新集群
- 由于 nodeIPDir 始终有所有节点文件，`HealthUnreachable`（网络不通）意味着其他节点上没有 etcd 进程运行，与"没有其他节点"等价

### Fix 4：BrainSplitChecker 改用 cluster ID + member ID 比较，循环直到一致（🔴 核心）

**核心改变**：
1. 使用 cluster ID + member ID 比较作为脑裂判断手段
2. 不设置对比次数上限，持续对比直到一致才退出
3. 周期可配，默认 5s

```go
// manager.go
func (m *Manager) checkBrainSplit(...) BrainSplitCheckResult {
    // 先从本地 etcd 获取自己的 cluster ID 和 member ID
    myClusterID, myMemberID := m.getLocalClusterInfo(tlscfg)

    // 本地信息不可用 → 无法进行有意义的对比，必须重试
    if myClusterID == 0 || myMemberID == 0 {
        return BrainSplitCheckResult{BrainSplitDetected: false, Reason: "local not ready"}
    }

    var otherEndpoints []string
    var totalOtherNodes int

    for _, entry := range entries {
        // 跳过自己的 masterIP
        if _, isMyIP := myIPSet[masterIP]; isMyIP {
            continue
        }
        totalOtherNodes++

        // 健康检查
        health := checkSingleEndpoint(...)
        if health == HealthHealthy {
            otherEndpoints = append(otherEndpoints, endpoint)
        }
        // HealthNotReady / HealthUnreachable 不参与对比，等下次检查
    }

    // 没有其他健康节点可达 → 无法对比
    if len(otherEndpoints) == 0 {
        if totalOtherNodes == 0 {
            return BrainSplitCheckResult{BrainSplitDetected: false, Reason: "no other nodes"}
        }
        // 有其他节点但都不可达/未就绪 → 无法验证，继续循环等待
        return BrainSplitCheckResult{BrainSplitDetected: false, Reason: "no healthy nodes to compare"}
    }

    // 有其他节点可达 → 比较 cluster ID 和 member ID
    // 必须至少有 1 个远程节点通过完整比对才返回 "verified"
    verifiedCount := 0
    for _, endpoint := range otherEndpoints {
        resp, err := getMemberList(endpoint, tlscfg)
        if err != nil {
            continue // 偶发失败跳过，等下次检查
        }

        // 检查 1：cluster ID 必须一致（myClusterID 已保证非零）
        if resp.Header.ClusterId != 0 && myClusterID != resp.Header.ClusterId {
            return BrainSplitCheckResult{
                BrainSplitDetected: true,
                Reason: fmt.Sprintf("cluster ID mismatch: ours=%016x, theirs=%016x", myClusterID, resp.Header.ClusterId),
            }
        }

        // 检查 2：我们的 member ID 必须在对方 member list 中（myMemberID 已保证非零）
        memberIDFound := false
        for _, member := range resp.Members {
            if member.ID == myMemberID {
                memberIDFound = true
                break
            }
        }
        if !memberIDFound {
            return BrainSplitCheckResult{
                BrainSplitDetected: true,
                Reason: fmt.Sprintf("endpoint %s does not contain our member ID %016x", endpoint, myMemberID),
            }
        }

        verifiedCount++
    }

    // 没有任何远程节点通过完整比对 → 不能返回 verified，必须重试
    if verifiedCount == 0 {
        return BrainSplitCheckResult{BrainSplitDetected: false, Reason: "no healthy nodes to compare"}
    }

    return BrainSplitCheckResult{BrainSplitDetected: false, Reason: "verified"}
}
```

**StartBrainSplitChecker 改为循环直到一致**：

```go
func (m *Manager) StartBrainSplitChecker(ctx context.Context, resultCh chan<- BrainSplitCheckResult, cfg BrainSplitConfig) {
    period := cfg.CheckPeriod
    if period <= 0 {
        period = 5 * time.Second // 默认 5s
    }

    ticker := time.NewTicker(period)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            result := m.checkBrainSplit(cfg.TLSConfig)
            if result.BrainSplitDetected {
                klog.Errorf("Brain split detected: %s", result.Reason)
                resultCh <- result
                return
            }
            if result.Reason == "verified" {
                // cluster ID 和 member ID 都一致，检查通过，退出
                klog.Infof("Brain split check passed: cluster ID and member ID verified")
                resultCh <- BrainSplitCheckResult{BrainSplitDetected: false, Reason: "all checks passed"}
                return
            }
            // "local not ready" / "no other nodes" / "no healthy nodes to compare" / "read error"
            // → 继续循环，不设上限
            klog.V(2).Infof("Brain split check inconclusive: %s, will retry after %v", result.Reason, period)
        }
    }
}
```

**改用 cluster ID + member ID 比较而非 member 名字的原因**：
- Cluster ID：etcd 集群在初始化时随机生成，不同集群的 cluster ID 绝对不同。这是区分脑裂的最可靠手段。
- Member ID：etcd 为新成员分配的唯一 ID，不会因为 pod 重建而改变（如果保留了数据目录）。比名字比较更精确。
- 当 cluster ID 不同 → 肯定脑裂。当 cluster ID 相同但 member ID 不在对方列表中 → 可能本方是 stale 成员。

**不设对比次数上限的原因**：
- `verified` 是唯一安全退出的条件（cluster ID 一致 + member ID 在对方列表中，且至少有 1 个远程节点通过比对）
- 脑裂检测到则立即退出并上报
- `"local not ready"` / `"no other nodes"` / `"no healthy nodes to compare"` / `"read error"` 等所有中间状态持续循环，直到达到可验证状态
- 循环周期可配，默认 5s，避免 CPU 空转

**关键安全保证**：
1. 本地 cluster ID / member ID 必须从本地 etcd 成功获取（非零），否则不进行任何比较，直接 retry。确保不会因为获取不到本地信息而跳过所有检查、误返回 `verified`
2. 必须至少有 1 个其他节点通过完整比对（cluster ID 一致 + member ID 在对方列表中），才返回 `verified`。防止所有远程节点 `MemberList` 调用都失败后仍然误返回 `verified`
3. 对端 `resp.Header.ClusterId` 为 0 时不触发脑裂判定（对端可能正在启动），但不计入 `verifiedCount`，等下次检查

### Fix 5：processMembers 区分 Voting 和 Learner（🟡）

```go
func (m *Manager) processMembers(...) (myMemberID uint64, myIsLearner bool, votingNumExceptMe int) {
    for _, member := range members {
        if member.Name == m.cfg.PodName {
            // ... 记录自己
            continue
        }
        // ... dead member removal ...
        if !shouldRemove && !member.IsLearner {
            votingNumExceptMe++
        }
    }
}
```

`addMemberAndStart` 使用 `votingNumExceptMe` 决定加入策略。

### Fix 6：Learner 提升失败时清理（🟡）

```go
func (m *Manager) handleExistingMemberWithData(ctx context.Context, client etcdcli.Cluster, ...) (*exec.Cmd, error) {
    if myIsLearner {
        err := m.promoteLearner(client, myMemberID)
        if err != nil {
            // 提升失败：移除这个 learner 及本地数据
            klog.Errorf("Failed to promote learner: %v, removing member and data", err)
            m.removeDeadMember(ctx, client, &pb.Member{ID: myMemberID})
            os.RemoveAll(dataDir)
            return nil, fmt.Errorf("failed to promote learner, cleaned up: %w", err)
        }
    }
    return m.startEtcd(etcdBin)
}
```

### Fix 7：修复 buildClusterConfig 的代码风格（🟢，可选）

保持功能不变，但避免直接修改传入结构体：

```go
func (m *Manager) buildClusterConfig(members []*pb.Member) []string {
    var cluster []string
    for _, member := range members {
        name := member.Name
        if name == "" {
            name = m.cfg.PodName
        }
        for _, url := range member.PeerURLs {
            cluster = append(cluster, fmt.Sprintf("%s=%s", name, url))
        }
    }
    return cluster
}
```

## Summary of Changes

| # | File | Change | Severity | Justification |
|---|------|--------|----------|---------------|
| 1 | `discovery.go` | `checkSingleEndpoint` 返回三态健康枚举（Unreachable/Healthy/NotReady） | 🔴 Critical | 区分网络不通/健康/未就绪，不关注 NOSPACE |
| 2 | `discovery.go` | `EndpointInfo` 字段改为 `ReadyNodes` + `NotReadyNodes` | 🔴 Critical | NotReady 节点应直接重试不退避，不影响 init 新集群的判断 |
| 3 | `main.go` | `runClusterLoop` 决策逻辑：NotReady 时直接重试不退避；ReadyNodes==0 && NotReadyNodes==0 才允许初始化新集群 | 🔴 Critical | 直接解决 etcd-0 误初始化的问题 |
| 4 | `manager.go` | `checkBrainSplit` 使用 cluster ID + member ID 比较，循环直到一致才退出，不设次数上限，周期可配默认 5s | 🔴 Critical | 让 BrainSplitChecker 真正能检测到脑裂 |
| 5 | `manager.go` | `processMembers` 区分 voting/learner 计数 | 🟡 High | 防止加入策略因 Learner 计数错误而失败 |
| 6 | `manager.go` | `handleExistingMemberWithData` 提升失败清理 | 🟡 High | 防止死循环 |
| 7 | `manager.go` | `buildClusterConfig` 用局部变量替代修改入参 | 🟢 Low | 代码卫生 |

## Testing Plan

| 场景 | 操作 | 预期行为 |
|------|------|---------|
| 全新集群首次启动 | 部署 StatefulSet，etcd-0/1/2 依次启动 | etcd-0 init 新集群，etcd-1/2 join |
| etcd-0 重启，其他正常 | `kubectl delete pod etcd-server-0` | etcd-0 发现 etcd-1/2 alive → join existing |
| etcd-0 重启，网络短暂不可达 | iptables 阻断 etcd-0 到其他节点的 2479/2480 端口 | etcd-0 发现 unreachable → 等待重试 → 网络恢复后 join |
| 脑裂场景 | 手动启动一个独立 etcd 进程（不同 cluster token） | etcd-0 的 BrainSplitChecker 通过 cluster ID 不一致检测到脑裂 → 终止 etcd → 重新 join |
| HealthNotReady 快速重试 | 其他节点 etcd 正在启动（leader 选举中） | etcd-0 探测到 NotReady → 不退避直接重试 → 节点就绪后立即 join |
| Learner 提升失败 | 手动制造提升失败场景 | 移除 learner 成员，清理数据，下次循环重新加入 |
