# Team-Worker 所有权模型问题梳理

> **✅ RESOLVED**: 本文档列出的所有问题已在 [`team-refactor-plan.md`](./team-refactor-plan.md) 中通过关系模型重新设计解决。核心修复：Team 不再生成/覆盖 Worker CR，Worker 通过 `spec.teamRef` 单向引用 Team，所有权冲突在建模层消失。本文档保留为问题档案。

## 1. 背景

HiClaw 的 Team CRD 实现了一个典型的"父资源管理子资源"模式：用户创建 Team CR，Team reconciler 根据 spec 中的 `leader` 和 `workers[]` 自动创建对应的 Worker CR，Worker reconciler 再为每个 Worker CR 创建 Pod 和相关基础设施（Matrix 账号、MinIO 用户、Gateway consumer 等）。

资源关系链路：

```
Team CR (用户声明)
  │
  │  Team Reconciler 自动创建/更新/删除
  ▼
Worker CR (leader)   ──→ Worker Reconciler ──→ Pod + Matrix + MinIO + Gateway
Worker CR (member-1) ──→ Worker Reconciler ──→ Pod + Matrix + MinIO + Gateway
Worker CR (member-2) ──→ Worker Reconciler ──→ Pod + Matrix + MinIO + Gateway
```

这个模式在 K8s 生态中非常常见，例如：
- Deployment → ReplicaSet → Pod
- Crossplane CompositeResource → ManagedResource
- Strimzi Kafka → StatefulSet
- KubeVirt VirtualMachine → VirtualMachineInstance

## 2. 当前实现方式

### 2.1 Worker 与 Team 的关联机制

当前通过 annotation 和 label 建立 Team 与 Worker 的关联，没有使用 K8s 原生的 ownerReferences：

```go
// team_controller.go - buildLeaderCR / buildWorkerCR
annotations := map[string]string{
    "hiclaw.io/role":        "team_leader" | "worker",
    "hiclaw.io/team":        t.Name,
    "hiclaw.io/team-leader": leaderName,      // 仅 member
    "hiclaw.io/team-admin-id": adminMatrixID, // 可选
}
labels := map[string]string{
    "hiclaw.io/team": t.Name,
    "hiclaw.io/role": "team_leader" | "worker",
}
```

### 2.2 Team Reconciler 的生命周期管理

- **创建**（`handleCreate`）：为 leader 和每个 worker 构建 Worker CR 并调用 `createOrUpdateWorkerCR`
- **更新**（`handleUpdate`）：重新构建所有 Worker CR，通过 label `hiclaw.io/team` 查询现有 Worker，删除不在期望列表中的 stale Worker
- **删除**（`handleDelete`）：遍历 `t.Spec.Workers` 逐个删除 Worker CR，再删除 leader CR

### 2.3 Worker Reconciler 如何识别 Team 上下文

Worker reconciler 在 reconcile 过程中读取 annotation 判断自身角色：

```go
// worker_reconcile_infra.go
role := w.Annotations["hiclaw.io/role"]
teamName := w.Annotations["hiclaw.io/team"]
teamLeaderName := w.Annotations["hiclaw.io/team-leader"]

// worker_reconcile_delete.go
isTeamWorker := w.Annotations["hiclaw.io/team-leader"] != ""
```

## 3. 存在的问题

### 3.1 缺少 ownerReferences — 无法级联删除和垃圾回收

当前 `buildLeaderCR` 和 `buildWorkerCR` 构建的 Worker CR 没有设置 `ownerReferences`，导致：

1. **无法级联删除**：删除 Team 时必须依赖 `handleDelete` 手动逐个删除 Worker CR。如果 controller 在删除过程中崩溃、重启，或 finalizer 逻辑有 bug，会产生孤儿 Worker CR 持续运行并消耗资源
2. **无法通过 K8s GC 自动回收**：K8s 内置的 garbage collector 基于 ownerReferences 工作，当前的 annotation 方式完全绕过了这个机制
3. **所有权不可追溯**：`kubectl get worker xxx -o yaml` 无法直接看到这个 Worker 属于哪个 Team（需要查看 annotation 而非标准的 ownerReferences 字段）
4. **无法利用 controller-runtime 的 Owns() 监听**：`SetupWithManager` 中无法用 `.Owns(&v1beta1.Worker{})` 自动监听子资源变化，当前只监听了 Team 本身的变化

相关代码位置：
- `team_controller.go:333-376`（buildLeaderCR，无 ownerReferences）
- `team_controller.go:378-431`（buildWorkerCR，无 ownerReferences）
- `team_controller.go:504-508`（SetupWithManager，未 Owns Worker）

### 3.2 用户直接修改 Worker CR 会被静默覆盖

`handleUpdate` 在每次 reconcile 时会用 Team spec 重新构建所有 Worker CR 并调用 `createOrUpdateWorkerCR`：

```go
// team_controller.go:433-449
func (r *TeamReconciler) createOrUpdateWorkerCR(ctx context.Context, desired *v1beta1.Worker) error {
    existing.Spec = desired.Spec  // 直接覆盖整个 Spec
    for k, v := range desired.Annotations {
        existing.Annotations[k] = v
    }
    return r.Update(ctx, existing)
}
```

这意味着：
1. 用户通过 `kubectl edit worker xxx` 直接修改被 Team 管理的 Worker CR，修改会在下次 Team reconcile 时被静默覆盖，没有任何警告
2. 没有 ValidatingWebhook 或其他机制阻止或提示用户"这个 Worker 由 Team 管理，请修改 Team CR"
3. 没有 drift 检测 — Team status 中不会反映 Worker CR 的实际状态是否与 Team spec 的期望一致

### 3.3 handleDelete 依赖 spec 而非实际查询

```go
// team_controller.go:307-314
for _, w := range t.Spec.Workers {
    workerCR := &v1beta1.Worker{}
    workerCR.Name = w.Name
    workerCR.Namespace = ns
    if err := r.Delete(ctx, workerCR); err != nil { ... }
}
```

删除逻辑遍历的是 `t.Spec.Workers`，而不是通过 label 查询实际存在的 Worker CR。如果 Team spec 在某次更新中移除了一个 worker，但 `handleUpdate` 的 stale worker 清理失败了，那么 `handleDelete` 不会清理到这个已经不在 spec 中但实际仍存在的 Worker CR。

对比 `handleUpdate` 中的做法（通过 label 查询）是更健壮的：

```go
// team_controller.go:224-225 (handleUpdate 中的做法)
r.List(ctx, &existingWorkers, client.InNamespace(t.Namespace),
    client.MatchingLabels{"hiclaw.io/team": t.Name})
```

### 3.4 SetupWithManager 未监听子资源变化

```go
// team_controller.go:504-508
func (r *TeamReconciler) SetupWithManager(mgr ctrl.Manager) error {
    return ctrl.NewControllerManagedBy(mgr).
        For(&v1beta1.Team{}).
        Complete(r)
}
```

当前只监听 Team CR 的变化。如果一个被管理的 Worker CR 被外部删除或修改，Team reconciler 不会感知到，也不会自动修复。加上 ownerReferences 后可以使用 `.Owns(&v1beta1.Worker{})` 自动触发 Team reconcile。

## 4. K8s 生态中的最佳实践参考

| 项目 | 父→子关系 | ownerReferences | 防误改机制 | 子资源定制方式 |
|------|-----------|-----------------|-----------|--------------|
| K8s Deployment | Deployment→RS→Pod | 有 | 不建议直接改 RS/Pod | PodTemplate 内嵌在 Deployment spec |
| Crossplane | XR→ManagedResource | 有 | `crossplane.io/composite` annotation | Composition 模板 |
| Strimzi Kafka | Kafka→StatefulSet | 有 | 文档说明 + 自动修复 | `template` 字段透传 Pod/Container 定制 |
| KubeVirt | VM→VMI | 有 | 直接改 VMI 会被覆盖 | VM spec 中定义 |
| ArgoCD | Application→K8s资源 | 有 | 检测 drift，UI 标记 "OutOfSync" | Application spec 中声明 |

共同点：
1. **全部使用 ownerReferences** 实现级联删除和 GC
2. **父资源是唯一的声明式入口**，子资源的修改应通过父资源进行
3. **提供某种机制**让用户知道子资源是被管理的（annotation、webhook、或 status 中的 drift 检测）

## 5. 建议改进方向

### 5.1 添加 ownerReferences（优先级：高）

在 `buildLeaderCR` 和 `buildWorkerCR` 中设置 ownerReferences，启用级联删除：

```go
OwnerReferences: []metav1.OwnerReference{
    *metav1.NewControllerRef(team, v1beta1.GroupVersion.WithKind("Team")),
},
```

同时在 `SetupWithManager` 中添加 `.Owns(&v1beta1.Worker{})`，让 Worker 变化自动触发 Team reconcile。

### 5.2 标记被管理的 Worker CR（优先级：高）

添加 `app.kubernetes.io/managed-by: team-controller` label，明确标识这个 Worker 是由 Team controller 管理的。配合文档说明或 ValidatingWebhook 提示用户不要直接修改。

### 5.3 handleDelete 改用 label 查询（优先级：中）

将 `handleDelete` 中的删除逻辑从遍历 `t.Spec.Workers` 改为通过 `hiclaw.io/team` label 查询实际存在的 Worker CR，与 `handleUpdate` 保持一致，避免遗漏孤儿资源。

### 5.4 drift 检测（优先级：低）

在 Team status 中增加 sync 状态字段，当检测到 Worker CR 的实际 spec 与 Team 期望不一致时标记为 `Drifted`，方便运维排查。
