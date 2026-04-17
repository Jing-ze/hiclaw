# Team / Worker / Manager / Human CR 关系模型重构方案

## 0. 元信息

- 创建日期：2026-04-17
- 前置讨论：`team-worker-proposal.md`（原始 Team 设计）、`team-worker-ownership-issues.md`（问题梳理）
- 定位：**终局重构方案**。不考虑兼容性、不分 MVP 阶段，一次到位。
- 关联 PRs：Manager Reconciler 模块化（#635）、Worker Reconciler declarative convergence 重写（#632）—— 本重构承接它们的 phase-based declarative 风格，将 Team 与 Human 两个 reconciler 对齐。

---

## 1. 问题背景

### 1.1 现状不一致

HiClaw 的四个 CR（Worker / Team / Manager / Human）在关系建模上存在哲学冲突：

- **Human**：通过 `accessibleTeams`、`accessibleWorkers` 单向引用 Team 和 Worker，是典型的 Reference 模式。
- **Team**：通过 `spec.leader` 和 `spec.workers[]` 完整内联 Worker 的 spec，reconciler 生成和更新对应的 Worker CR，是典型的 Composition / Deployment 模式。
- **Manager**：通过 Worker reconciler "反向 push" 自己到 Manager 的 `groupAllowFrom` 列表，再辅以 Team Worker 的特判跳过，是命令式 side-effect 链路，既不是 Reference 也不是 Composition。

这种"同系统内三种关系哲学"的结果是：每当添加新字段、新 role、新 API endpoint 时，设计者都必须重新决定"这个字段归谁"、"这条关系走哪种模型"，长期累积出不可维护的复杂度。

### 1.2 Team-Worker 所有权问题（详见 `team-worker-ownership-issues.md`）

当前 `team_controller.go` 的 `createOrUpdateWorkerCR` 方法会整块覆盖 Worker CR 的 spec：

```go
existing.Spec = desired.Spec   // 所有字段归 Team
```

这导致用户 `kubectl edit worker alpha-dev` 的任何改动都会在下一次 Team reconcile 时被静默覆盖，没有 event、没有 warning、没有 drift 检测。同时当前实现还缺少：

- `ownerReferences`，K8s GC 级联删除失效；
- `Owns(&Worker{})` 监听，Worker 变化不触发 Team reconcile；
- `handleDelete` 依赖 `t.Spec.Workers` 遍历而非 label 查询，漏删 stale worker；
- `TeamStatus` 中没有 `ObservedGeneration`，reconcile 无法判断"本次是否有效"。

### 1.3 Worker 不是 Pod 的身份成本

HiClaw 的每个 Worker 拥有：Matrix 账号（不可重建）、MinIO 用户（带历史数据）、Gateway key、独立的 SOUL/skills/state 文件。"改一个 spec 字段就重建 Worker" 的 Deployment 流派模型对 Worker 而言成本不可接受。任何设计方案都必须保证 **改字段不触发身份重建**。

### 1.4 Team Reconciler 代码复杂度

`team_controller.go` 单文件 582 行，承担 reconcile 入口 + 三阶段生命周期 + Worker CR 构造 + ChannelPolicy 合并 + Room 创建 + Coordination 注入 + Expose 端口 + Shared Storage + Legacy 兼容。这与刚完成重构的 Worker reconciler（256 行）和 Manager reconciler（166 行）形成鲜明对比。Team 作为"meta-reconciler"（同时管理 K8s CR 和外部基础设施）的混合角色是复杂度根源。

---

## 2. 设计原则

本次重构基于四条不可妥协的原则：

### 2.1 关系模型统一为 Reference（哲学 C）

所有跨 CR 引用都改为单向软引用（soft reference），与 Human 已有模式保持一致。Worker 始终是独立一等公民，Team 通过 list + label selector 观察成员，Manager 通过 list 观察 Worker 与 Human。

### 2.2 父资源不写子资源的 spec

Team reconciler 不再生成、修改或删除 Worker CR。所有 Worker spec 的所有权归用户。Team 只管 team-level 基础设施（Team Room、Leader DM Room、Shared Storage）和观察结果投影（Team.status）。

### 2.3 Apply 顺序无关

所有跨资源引用都是 soft reference，ValidatingAdmissionWebhook **禁止**做跨 kind 存在性校验。引用未解析状态通过 Status Conditions 暴露，reconcile-time 级 triggered 自动收敛。

### 2.4 所有跨 CR 引用必须单向

引用图中任何两个 kind 之间最多只有一个方向的 spec 级字段。反方向通过 list + selector 观察，投射到 status。不允许任何"双向权威 + 冲突解决规则"的设计。

---

## 3. 终局 CR 形态

### 3.1 Worker CR

`WorkerSpec` 新增字段：

| 字段 | 类型 | 默认 | 约束 |
|---|---|---|---|
| `role` | enum `standalone \| team_leader \| team_worker` | `standalone` | 必填 |
| `teamRef` | string | "" | `role != standalone` 时必填；`role == standalone` 时必须为空 |

`WorkerStatus` 新增字段：

| 字段 | 类型 | 语义 |
|---|---|---|
| `teamRef` | string | 观察到的 teamRef（用于检测迁移） |
| `conditions` | `[]metav1.Condition` | `Ready`、`Provisioned`、`TeamRefResolved` |

Controller 自动维护两个派生 label：`hiclaw.io/team=<spec.teamRef>`、`hiclaw.io/role=<spec.role>`，供 TeamReconciler / ManagerReconciler 做 `MatchingLabels` 查询。

### 3.2 Team CR

`TeamSpec` 完全重写为瘦协调描述：

| 字段 | 类型 | 语义 |
|---|---|---|
| `description` | string | 团队描述 |
| `peerMentions` | `*bool` | 是否允许 member 互 mention，默认 true |
| `channelPolicy` | `*ChannelPolicySpec` | 团队级 channel policy 默认 |
| `heartbeat` | `*TeamHeartbeatSpec` | Leader 心跳配置 |
| `workerIdleTimeout` | string | 成员空闲超时 |

**完全删除**：`leader`（inline Worker spec）、`workers[]`（inline Worker spec 数组）、`admin`（inline TeamAdminSpec）。Leader 身份通过 Worker.spec.role 表达；成员身份通过 Worker.spec.teamRef 表达；Admin 通过 Human.spec.teamAccess[].role=admin 表达。

`TeamStatus` 重写为观察结果投影：

| 字段 | 类型 | 语义 |
|---|---|---|
| `observedGeneration` | int64 | 用于 defer-patch 模式 |
| `phase` | enum `Pending \| Active \| Degraded \| Failed` | 综合状态 |
| `teamRoomID`、`leaderDMRoomID` | string | Matrix Room ID |
| `leader` | `*TeamLeaderObservation` | `{Name, MatrixUserID, Ready}` |
| `members` | `[]TeamMemberObservation` | `{Name, Role, MatrixUserID, Ready}` |
| `admins` | `[]TeamAdminObservation` | `{HumanName, MatrixUserID}`，由 Human.teamAccess 推导 |
| `totalMembers`、`readyMembers` | int | 观察统计 |
| `conditions` | `[]metav1.Condition` | `LeaderResolved`、`TeamRoomReady`、`MembersHealthy`、`NoLeader`、`MultipleLeaders` |
| `message` | string | 最近错误或提示 |

### 3.3 Human CR

`HumanSpec` 重写：

| 字段 | 类型 | 语义 |
|---|---|---|
| `displayName` | string | 显示名 |
| `email` | string | 注册邮箱 |
| `note` | string | 备注 |
| `superAdmin` | bool | 替代原 `permissionLevel=1`，true 时全访问 |
| `teamAccess` | `[]TeamAccessEntry` | 替代原 `accessibleTeams` + Team.spec.admin；每条 `{team, role: admin \| member}` |
| `workerAccess` | `[]string` | 替代原 `accessibleWorkers`（L3 风格直连）|

**完全删除**：`permissionLevel`、`accessibleTeams`、`accessibleWorkers`。

`HumanStatus` 沿用现有字段，新增 `observedGeneration`、`conditions`。

### 3.4 Manager CR

**不变**。Manager 的 spec 设计本来就正确，无需调整。Manager Reconciler 增加 `reconcileManagerAllowFrom` phase（见第 4 节）。

### 3.5 跨 CR 引用拓扑

```
Worker.spec.teamRef              ──────► Team   (list by label)
Human.spec.teamAccess[].team     ──────► Team   (list observation)
Human.spec.workerAccess[]        ──────► Worker (list observation)
```

**零双向引用**。反方向全部通过 list + mapper 实现。Team / Manager / Worker 之间没有任何 spec-level 引用。

---

## 4. Reconciler 终局架构

### 4.1 TeamReconciler（完全重写）

按 `worker_controller.go` 的 phase-based declarative 风格拆分为 8 个文件：

| 文件 | 职责 |
|---|---|
| `team_controller.go` | Reconcile 主循环、finalizer、defer-patch status、SetupWithManager |
| `team_scope.go` | `teamScope` 结构：team 对象 + patchBase + members + leader + admins |
| `team_phase.go` | `computeTeamPhase` 综合 conditions 计算 phase |
| `team_reconcile_members.go` | list Workers by `hiclaw.io/team` label，分类 leader/member，检测 0/1/2+ leader |
| `team_reconcile_admins.go` | list Humans with `teamAccess[].team=self AND role=admin` |
| `team_reconcile_rooms.go` | 调 `Provisioner.EnsureTeamRooms` + `ReconcileTeamRoomMembership` |
| `team_reconcile_storage.go` | 调 `Provisioner.EnsureTeamStorage` |
| `team_reconcile_legacy.go` | 更新 teams-registry（non-critical） |
| `team_reconcile_delete.go` | finalizer：清理 Rooms + Storage + registry；**不删 Worker** |

`SetupWithManager` 监听图：

```go
For(&Team{}).
    Watches(&Worker{}, workerToTeamsMapper).
    Watches(&Human{},  humanToTeamsMapper)
```

### 4.2 WorkerReconciler（增量扩展）

保持现有 4 个 phase（infra / config / container / expose），新增 2 个 phase：

- `reconcileTeamMembership`（插入在 infra 与 config 之间）：
  - 读 `spec.teamRef`，若为空则跳过；
  - Get Team CR，不存在则设置 `TeamRefResolved=False` 并 fallback 成 standalone-style policy；
  - 找到则计算 effective channelPolicy（Team 默认 + Worker override）、读 `Team.status.members` 为 peer list；
  - 检测 `spec.teamRef != status.teamRef` 的迁移并处理。

- `reconcileLeaderBroadcast`（插入在 config 与 container 之间）：
  - 仅 `role=team_leader` 触发；
  - 调 `Deployer.WriteLeaderCoordinationContext` 把 coordination context（Team Room ID、Leader DM Room ID、heartbeat、workers 列表、team admin matrix IDs）写入 leader 自己的 MinIO agent 空间。

Reconcile 入口处新增 label 同步：确保 `Worker.Labels["hiclaw.io/team"]` 和 `hiclaw.io/role` 永远等于 spec 值。

监听图扩展：

```go
For(&Worker{}).
    Watches(&Pod{},    podToWorkerMapper).  // 既有
    Watches(&Team{},   teamToWorkersMapper).
    Watches(&Human{},  humanToWorkersMapper)
```

### 4.3 ManagerReconciler（增量扩展）

保持现有 infra / config / container phase，新增：

- `reconcileManagerAllowFrom`（插入在 config 与 container 之间）：
  - list Workers filter `role ∈ {team_leader, standalone}`；
  - list Humans filter `superAdmin=true`；
  - 合并成 effective allowFrom，后续 config phase 写入 agent config。

监听图扩展：

```go
For(&Manager{}).
    Watches(&Pod{},    podToManagerMapper).  // 既有
    Watches(&Worker{}, workerToManagersMapper).
    Watches(&Human{},  humanToManagersMapper)
```

### 4.4 HumanReconciler（完全重写）

从老式 `switch Phase` 重写为 phase-based declarative：

| 文件 | 职责 |
|---|---|
| `human_controller.go` | Reconcile 主循环、finalizer、defer-patch |
| `human_scope.go` | `humanScope` |
| `human_phase.go` | `computeHumanPhase` |
| `human_reconcile_infra.go` | Matrix 账号创建 + credentials refresh |
| `human_reconcile_rooms.go` | 根据 superAdmin + teamAccess + workerAccess 计算 desired rooms，diff 当前 rooms，invite/leave |
| `human_reconcile_legacy.go` | humans-registry 更新 |
| `human_reconcile_delete.go` | finalizer：从所有 Room 踢出、废账号、registry 清理 |

监听图：

```go
For(&Human{}).
    Watches(&Team{},   teamToHumansMapper).   // Team.status 里 admin/member 变化需要重算 Human rooms
    Watches(&Worker{}, workerToHumansMapper). // Worker 新建 Room 后需要邀请被授权的 Human
```

---

## 5. Service 层接口变更

### 5.1 新增接口

**`TeamProvisioner`** `service/interfaces.go`：

```
EnsureTeamRooms(ctx, TeamRoomsRequest) (*TeamRoomsResult, error)
ReconcileTeamRoomMembership(ctx, TeamRoomMembershipRequest) error
EnsureTeamStorage(ctx, teamName) error
CleanupTeamInfra(ctx, TeamCleanupRequest) error
```

**`TeamObserver`** `service/interfaces.go`：

```
ListTeamMembers(ctx, teamName) ([]WorkerObservation, error)
ListTeamAdmins(ctx, teamName) ([]HumanObservation, error)
```

### 5.2 改签名

- `service.Provisioner.ProvisionTeamRooms` → 重命名为 `EnsureTeamRooms`，入参 `TeamRoomsRequest`（新类型，不再持有 `TeamAdminSpec`，改为 `AdminMatrixIDs []string`）。
- `service.Deployer.InjectCoordinationContext` → 删除；替换为 `WriteLeaderCoordinationContext(ctx, LeaderCoordinationRequest)`，在 Worker Reconciler 的 `reconcileLeaderBroadcast` phase 里被调用。
- `service.Provisioner.EnsureTeamStorage` → 保留，签名不变。

### 5.3 删除类型与方法

- `service.TeamRoomRequest`（替换为 `TeamRoomsRequest`）
- `service.TeamRoomResult`（替换为 `TeamRoomsResult`）
- `service.CoordinationDeployRequest`（替换为 `LeaderCoordinationRequest`）
- `service.Deployer.InjectCoordinationContext`

---

## 6. ValidatingAdmissionWebhook

### 6.1 Package 布局

新建 `hiclaw-controller/internal/webhook/`：

| 文件 | 职责 |
|---|---|
| `webhook.go` | dispatcher + `RegisterWithManager` |
| `worker_validator.go` | `ValidateWorkerCreate`、`ValidateWorkerUpdate` |
| `team_validator.go` | `ValidateTeamCreate`、`ValidateTeamUpdate` |
| `human_validator.go` | `ValidateHumanCreate`、`ValidateHumanUpdate` |
| `validators.go` | 共享工具（list 同 team leader、合法 duration 等）|
| `*_test.go` | table-driven 单测 |

### 6.2 校验规则（详尽清单）

**Worker**（单对象 + 同 kind peer）：
- `spec.role ∈ {standalone, team_leader, team_worker}`
- `role != standalone` ⟹ `teamRef != ""`
- `role == standalone` ⟹ `teamRef == ""`
- `role == team_leader`：list 同 namespace 内 `hiclaw.io/team=this.teamRef AND hiclaw.io/role=team_leader`，期望 ≤ 1 且 name 就是自己（允许更新）
- `spec.runtime ∈ {openclaw, copaw, ""}`
- name DNS-1123 合法

**Team**（单对象）：
- `spec.heartbeat.every` 解析为合法 `time.Duration`
- `spec.workerIdleTimeout` 解析为合法 `time.Duration`
- `spec.peerMentions` 类型校验

**Human**（单对象）：
- `spec.superAdmin == true` ⟹ `teamAccess` 和 `workerAccess` 必须为空
- `spec.teamAccess[].role ∈ {admin, member}`
- `spec.teamAccess[].team` 数组内唯一

**禁止规则**：绝不做"teamRef 指向的 Team 必须存在"、"teamAccess.team 必须存在"、"workerAccess 必须存在" 这类跨 kind 存在性校验。

### 6.3 部署策略

- Validator 函数均为纯函数，不依赖 admission 原语。
- **incluster 模式**：`cmd/controller/main.go` 通过 controller-runtime `webhook.Server` 注册 `ValidatingWebhookConfiguration`；helm chart 生成对应 TLS secret（或用 cert-manager 注入）。
- **embedded 模式**：不注册 webhook server，REST API handler（`resource_handler.go`、`bundle_handler.go`）在写 CR 前直接调用 validator 函数。
- 环境变量 `HICLAW_WEBHOOK_ENABLED`（默认 true）控制 incluster 下是否启用。

---

## 7. REST API 与 CLI

### 7.1 REST API Endpoints

**保留**（Raw CR CRUD）：
- `POST/GET/PUT/DELETE /api/v1/workers[/{name}]`
- `POST/GET/PUT/DELETE /api/v1/teams[/{name}]`（注意：这里的 `POST/PUT` 只处理 Team CR 自身，不 expand）
- `POST/GET/PUT/DELETE /api/v1/humans[/{name}]`
- `POST/GET/PUT/DELETE /api/v1/managers[/{name}]`

**新增**（Bundle）：
- `POST /api/v1/bundles/team` —— 接收 `TeamBundleRequest`，server 侧展开创建 Team CR + Leader Worker CR + N Member Worker CR + 可选地 patch Human.teamAccess 添加 admin 关系；返回 `207 Multi-Status`。
- `DELETE /api/v1/bundles/team/{name}` —— list 所有 teamRef 指向该 team 的 Worker，逐个删除；patch Human.teamAccess 移除 admin 关系；最后删 Team CR；返回 `207 Multi-Status`。

### 7.2 Request/Response 类型（`internal/server/types.go`）

**删除**：`TeamLeaderRequest`、`TeamLeaderHeartbeatRequest`、`TeamWorkerRequest`。

**重写**：`CreateTeamRequest` / `UpdateTeamRequest` 瘦身，只含 Team spec 字段；`CreateHumanRequest` / `HumanResponse` 适配新字段。

**新增**：`TeamBundleRequest { Name, Description, Admins []string, PeerMentions, ChannelPolicy, Heartbeat, WorkerIdleTimeout, Leader TeamBundleLeader, Workers []TeamBundleWorker }`；`TeamBundleLeader`、`TeamBundleWorker` 为内联 Worker spec 视图。

### 7.3 CLI 命令行为

| 命令 | 调用路径 |
|---|---|
| `hiclaw create team` | POST `/api/v1/bundles/team` |
| `hiclaw delete team`（默认级联） | DELETE `/api/v1/bundles/team/{name}` |
| `hiclaw delete team --orphan-workers` | DELETE `/api/v1/teams/{name}` |
| `hiclaw update team` | PUT `/api/v1/teams/{name}`（只含 team-level 字段）|
| `hiclaw create worker --role X --team Y` | POST `/api/v1/workers` |
| `hiclaw update worker --role X --team Y` | PUT `/api/v1/workers/{name}` |
| `hiclaw promote worker X --as-leader-of Y` | 两次 PUT：先 demote 旧 leader，再 promote 目标 |
| `hiclaw create human --super-admin / --team X:role / --worker Y` | POST `/api/v1/humans` |
| `hiclaw apply -f file.yaml`（multi-doc）| POST `/api/v1/apply` 逐个 CR |

---

## 8. Team.spec.admin 到 Human.teamAccess 的迁移语义

### 8.1 语义等价关系

| 旧 | 新 |
|---|---|
| `Team.spec.admin = { name: "zhangsan", matrixUserId: "@zhangsan:domain" }` | `Human(zhangsan).spec.teamAccess[] 包含 { team: <team>, role: admin }` |
| Human `permissionLevel: 2, accessibleTeams: [alpha]` | `Human.spec.teamAccess[] 包含 { team: alpha, role: member }` |
| Human `permissionLevel: 1` | `Human.spec.superAdmin: true` |
| Human `permissionLevel: 3, accessibleWorkers: [w1]` | `Human.spec.workerAccess: [w1]` |

### 8.2 Bundle 创建时的 Admin 关系处理

`POST /api/v1/bundles/team` 的 `TeamBundleRequest.Admins` 字段接收一组 Human name。Server 侧：

1. 对每个 Human name，Get Human CR；
2. 如果存在：merge-patch `teamAccess[]` 追加 `{ team: <bundleName>, role: admin }`（若已存在则 no-op）；
3. 如果不存在：响应中标记为 Warning item（`not_found`），**不阻塞**其他创建。用户后续创建 Human 时 reconcile 会自动挂钩。

### 8.3 Bundle 删除时的 Admin 关系清理

`DELETE /api/v1/bundles/team/{name}`：

1. list 所有 Humans；
2. 对每个 `teamAccess[]` 中有 `team=<name>` 的 Human，merge-patch 移除那一条；
3. 保留其他 teamAccess 条目。

---

## 9. 测试策略

### 9.1 单元测试

- `internal/webhook/*_test.go`：table-driven，每条规则至少一个 pass / fail case。
- `internal/controller/team_phase_test.go`、`human_phase_test.go`：phase 计算纯函数。
- `internal/controller/*_reconcile_*_test.go`：各 phase 单元级逻辑。

### 9.2 集成测试（envtest）

新增 / 扩展：

- `test/integration/controller/team_test.go`（新）：10 个场景覆盖空团队、leader-first、team-first、member 增删、teamRef 迁移、multi-leader 冲突、human admin 挂接、kubectl 非级联删除、bundle 级联删除、Team 重建恢复。
- `test/integration/controller/human_test.go`（新）：覆盖 superAdmin、teamAccess admin/member、workerAccess、cross-team 迁移、Human 删除后 team status 更新。
- `test/integration/controller/bundle_test.go`（新）：POST bundle 成功、207 部分失败、DELETE bundle 级联。
- `test/integration/controller/worker_test.go`（扩展）：`TestWorkerWithInvalidTeamRef`、`TestWorkerRoleTransition`。
- `test/integration/controller/manager_test.go`（扩展）：`TestManagerAllowFromReactsToWorkerChanges`、`TestManagerAllowFromReactsToHumanChanges`。

### 9.3 手动验证

在 kind 集群跑：`hiclaw create team` → `hiclaw get team` → `kubectl edit worker` 观察无覆盖 → `hiclaw delete team`（级联） → 再次 apply 观察恢复。

---

## 10. 文档更新清单

| 文件 | 动作 |
|---|---|
| `docs/design/team-refactor-plan.md` | **新建**（本文件）|
| `docs/design/team-refactor-progress.md` | **新建**（进度跟踪）|
| `docs/design/team-worker-proposal.md` | 顶部加 "superseded by team-refactor-plan.md" 横幅 |
| `docs/design/team-worker-ownership-issues.md` | 顶部加 "RESOLVED by team-refactor-plan.md" 横幅 |
| `AGENTS.md` | Key Design Patterns 节、文件导航节 |
| `manager/agent/skills/team-management/SKILL.md` | 重写 create-team 脚本调用方式、admin 挂接方式 |
| `manager/agent/skills/human-management/SKILL.md` | 新字段 superAdmin / teamAccess / workerAccess |
| `manager/agent/worker-skills/**` | 审查 Worker CR 字段依赖，更新 |
| `changelog/current.md` | 添加 refactor feat 条目 |

---

## 11. 新语义下的工作流走查

### 11.1 用户创建整个 Team（Manager agent 也走这条路）

```
User / Manager: hiclaw create team alpha \
    --leader alpha-lead --leader-model claude-sonnet-4-6 \
    --workers alpha-dev:claude,alpha-qa:gpt-5-mini \
    --admins zhangsan
↓
CLI → POST /api/v1/bundles/team {TeamBundleRequest}
↓
BundleHandler:
  1. ValidateTeamCreate, ValidateWorkerCreate (for leader + each worker), ValidateHumanUpdate (for zhangsan patch)
  2. Create Team{alpha}
  3. Create Worker{alpha-lead, role=team_leader, teamRef=alpha, ...}
  4. Create Worker{alpha-dev, role=team_worker, teamRef=alpha, ...}
  5. Create Worker{alpha-qa,  role=team_worker, teamRef=alpha, ...}
  6. Patch Human{zhangsan}.spec.teamAccess += {team: alpha, role: admin}
  7. Return 207 {per-resource results}
↓
各 Reconciler 自主 converge:
  - WorkerReconciler(alpha-lead):  provision Matrix/MinIO/Gateway/Pod + resolve teamRef + leader broadcast
  - WorkerReconciler(alpha-dev):   同上 + join Team Room
  - TeamReconciler(alpha):          list members → status.leader/members → EnsureTeamRooms → EnsureTeamStorage
  - HumanReconciler(zhangsan):      list teamAccess → invite to Team Room / Leader DM Room / Worker Rooms
```

### 11.2 用户调整单个 Worker 运行时参数

```
kubectl edit worker alpha-dev  # 改 spec.model
↓
WorkerReconciler(alpha-dev):
  - reconcileInfrastructure (no-op, 身份不变)
  - reconcileTeamMembership (teamRef 不变, policy 重算)
  - reconcileConfig (重新渲染 agent config, 含新 model)
  - reconcileContainer (重启 Pod 或触发 runtime model switch)
TeamReconciler(alpha):            不触发（Worker spec 变化不改 labels → mapper 不 enqueue）
```

**零覆盖风险**，因为 Team reconciler 本来就不写 Worker spec。

### 11.3 Worker 跨队迁移

```
kubectl edit worker alpha-dev  # spec.teamRef: alpha → beta
↓
WorkerReconciler(alpha-dev):
  - label sync: hiclaw.io/team 从 alpha 改成 beta
  - reconcileTeamMembership: 检测 spec.teamRef (beta) != status.teamRef (alpha)
    → 离开 alpha 的 Team Room, 更新 channelPolicy, 加入 beta 的 Team Room
    → 更新 status.teamRef = beta
TeamReconciler(alpha): mapper 触发 → list members 发现 alpha-dev 不再在列表 → 从 alpha 的 Team Room 踢出（已被 Worker 自己做了, 幂等）
TeamReconciler(beta):  mapper 触发 → list members 发现 alpha-dev 新加入 → status.members 更新
```

**Matrix 账号、MinIO 用户、Gateway key 全部保留**，只是 Room 成员关系切换。

### 11.4 typo 场景

```
kubectl apply -f <<EOF
apiVersion: hiclaw.io/v1beta1
kind: Worker
metadata:
  name: alpha-dev
spec:
  role: team_worker
  teamRef: alphx       # typo!
  ...
EOF
↓
Webhook: pass (不校验 Team 存在)
WorkerReconciler(alpha-dev):
  - reconcileInfrastructure: 正常 provision
  - reconcileTeamMembership: Get Team alphx → NotFound
    → set Condition TeamRefResolved=False, Reason=TeamNotFound
    → fallback effective policy = standalone-style
  - reconcileConfig: 写入 fallback config, worker 可运行
  - reconcileContainer: Pod 正常运行
kubectl describe worker alpha-dev:
  Conditions:
    Type: TeamRefResolved   Status: False   Reason: TeamNotFound
    Message: referenced team 'alphx' does not exist
```

用户看到条件 → `kubectl edit worker alpha-dev` → 改回 `alpha` → Worker reconciler 再次触发 → 收敛。

### 11.5 同 team 两个 leader 冲突

```
Worker alpha-lead:  role=team_leader, teamRef=alpha (already exists)
kubectl edit worker alpha-dev  # role: team_worker → team_leader
↓
Webhook ValidateWorkerUpdate:
  - List Workers MatchingLabels{hiclaw.io/team=alpha, hiclaw.io/role=team_leader}
  - 找到已有的 alpha-lead
  - 拒绝: "team 'alpha' already has leader 'alpha-lead'"
↓
用户看到 apiserver 报错, 明确的 webhook message
```

如果用户希望切换 leader，使用 `hiclaw promote worker alpha-dev --as-leader-of alpha`（原子两步）。

---

## 12. 与刚完成的 Manager/Worker Reconciler 重构的对齐

本重构完全承接 `09231a9`（Manager）和 `7d0c117`（Worker）的设计语言：

- `Reconcile(ctx, req) (retres reconcile.Result, reterr error)` 模板：patchBase 抽取 → defer-patch status（ObservedGeneration 仅成功时写）→ finalizer → switch normal/delete
- `teamScope` / `humanScope` 结构贯穿所有 phase
- phase 函数签名 `reconcileXxx(ctx, s *teamScope) (reconcile.Result, error)`
- `computeTeamPhase` / `computeHumanPhase` 纯函数
- modular 文件拆分（8 个 team_reconcile_*.go / 7 个 human_reconcile_*.go）
- 集成测试通过 envtest + mocks 覆盖

没有任何模式不一致。Team 和 Human 重构完之后，四个 CR 的 reconciler 呈现统一风格。

---

## 13. 实施边界

**本次重构包含**：
- 4 个 CR 的 types.go 重写 + CRD YAML 重写
- Team/Human reconciler 完全重写，Worker/Manager reconciler 增量扩展
- Webhook package 全新建立
- Service 层接口重签名
- REST API bundle 端点新建、旧 Team handler 瘦身
- CLI 命令适配
- 完整集成测试覆盖
- 所有涉及的文档更新

**不包含**：
- Nacos 配置中心的 package URI 解析调整（若有字段冲突）—— 若发现需改动，单独做小 PR
- Higress gateway 相关改动（expose 端口只读取 Worker spec，不受本次影响）
- Manager HA / leader election 机制（已由 `5cf7277` 提供）

**回滚策略**：本重构无需 legacy migration（用户明确说"还在重构阶段"）。Git 层面单个大 PR 失败就整体回滚到前一次 commit。集成测试充分覆盖，若 CI 绿则合入。
