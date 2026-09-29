# Zone Rollout Control 项目规格

## 1. 项目目标

Zone Rollout Control 是面向 DNS 运维人员的 Go HTTP 控制面。它把区域版本编排、解析节点领取租约、探针结果登记和一致性判定串成一条可追溯的上线链。服务只使用本机 JSON 快照，不连接真实 DNS 网络，也不依赖外部平台。

本仓库是健康项目初始化基线，要求可构建、可运行、无预置缺陷。测试在本阶段有意延后，由后续测试阶段补充；当前通过真实 HTTP 路由上的工作流探针进行生产路径验证。

## 2. 使用者

- 区域滚动负责人：创建滚动计划、添加区域版本、启动上线并完成计划。
- 解析节点值守人员：为目标节点领取有期限的租约，提交探针结果。
- DNS 平台集成者：通过稳定 JSON API 接入内部自动化流程。

## 3. 核心实体

### RolloutPlan

一次明确目标序列号、节点数量、成功比例和延迟上限的滚动计划。

- `id`：服务生成的稳定标识。
- `zone`：区域名称，必须是小写 DNS 名称。
- `state`：`draft`、`active` 或 `completed`。
- `minimum_success_ratio`：成功节点占比下限，范围为 0 到 1。
- `maximum_latency_ms`：允许的最大探针延迟。
- `expected_nodes`：计划预期参与的节点数，范围为 1 到 500。
- `version`：每次计划聚合变化后递增。
- `latest_revision_id`：最近一次添加的区域版本。
- `latest_assessment_id`：最近一次完成的一致性判定。
- `created_at`、`activated_at`、`completed_at`：RFC3339 时间。

### ZoneRevision

计划中不可变的区域内容版本。

- `id`、`plan_id`、`serial`、`authority`。
- `entries`：区域条目数组，每项包含 `owner`、`type` 和 `value`。
- `entry_count`：由服务计算，客户端不能覆盖。
- `created_at`。

同一计划内的序列号必须严格递增。条目类型只接受 `A`、`AAAA` 和 `CNAME`。`A` 与 `AAAA` 的值必须是 IPv4 或 IPv6 地址，`CNAME` 的值必须是规范域名。

### ResolverNode

边缘解析节点档案。

- `id`、`node_name`、`region`。
- `state`：`ready`、`leased` 或 `quarantined`。
- `version`、`created_at`、`updated_at`。

### NodeLease

计划对某个解析节点的有期限占用。

- `id`、`plan_id`、`node_id`、`allocation_id`。
- `state`：`claimed`、`reported`、`released` 或 `expired`。
- `claimed_at`、`expires_at`、`finished_at`。
- `probe_id`：成功登记探针后的结果标识。

### LeaseGroup

一次租约分配请求的幂等记录。

- `id`、`plan_id`、`request_key`、`request_fingerprint`、`region`、`lease_ids`。
- 相同请求键和相同内容重放时返回原分配组，内容不一致时返回冲突。

### Probe

解析节点对指定租约提交的一次上线观察。

- `id`、`lease_id`、`plan_id`、`node_id`。
- `request_key`、`success`、`observed_serial`、`latency_ms`。
- `observed_at`、`stored_at`、`payload_fingerprint`。

### RolloutAssessment

一次计划范围内的确定性一致性判定。

- `id`、`plan_id`、`revision_id`、`request_key`。
- `verdict`：`consistent` 或 `review_required`。
- `reasons`：稳定原因代码列表。
- `probe_count`、`success_count`、`success_ratio`、`maximum_latency_ms`。
- `created_at`。

### MaintenanceWindow

附加在滚动计划上的带时区日历条目，可以是允许上线的维护窗口（`permit`）或暂停时段（`pause`）。

- `id`、`plan_id`、`mode`（`permit`/`pause`）、`schedule`（`once`/`weekly`）。
- `timezone`：IANA 时区名，周期窗口按该时区的本地挂钟时间解释。
- 一次性窗口使用 `starts_at`、`ends_at`（RFC3339 绝对时刻）。
- 周期窗口使用 `weekdays`（0=周日到 6=周六）与 `start_time`、`end_time`（`HH:MM`），`end_time` 早于 `start_time` 表示跨日。
- `regions`：为空表示计划级且适用于所有节点区域；非空时只适用于列出的区域。
- `version`：每次替换递增；删除是软删除（`deleted`），保留身份以还原历史解释。

### WindowDecision

激活或租约分配发生瞬间冻结的、不可静默改写的日历解释。

- `allowed`、`reason`（`unconfigured`、`no_matching_permit_window`、`paused`）、`scope`（`plan`/`region`）、`mode`（`all_hours`/`scheduled`）。
- `evaluated_at`：统一使用服务时钟的 UTC 时刻。
- `basis`：命中的具体窗口与该次发生的 UTC `[starts_at, ends_at)` 区间、时区与本地时间。
- `blocking_pause_id` 与 `next_opening_at`：被暂停或无窗口时返回的确定结果。
- 计划激活后，该解释挂在 `RolloutPlan.window_decision`；租约组挂在 `LeaseGroup.window_decision`。

## 4. 状态和规则

### 滚动计划状态

```text
draft --activate--> active --complete--> completed
```

- 只有 `draft` 可以激活。
- 只有 `active` 可以完成。
- 激活后不能继续添加区域版本。
- 激活和完成需要匹配当前版本。

### 日历与上线窗口规则

- 计划从未配置日历（没有任何未删除条目）时，全天任何时间都可激活并领取租约；一旦存在条目，默认拒绝，必须有命中的 `permit` 窗口才放行。
- 计划激活与节点分配调用同一个纯函数求值器、同一服务时钟和同一 UTC 时刻解释：激活使用计划级作用域（忽略区域窗口），租约分配使用请求区域作用域。
- `pause` 时段总是优先于 `permit`：即使许可窗口覆盖该时刻，暂停期间也拒绝激活和新分配。
- 周期窗口按时区本地挂钟时间逐次展开；区间为闭开区间 `[start, end)`，相邻窗口既不重叠也不留缝。夏令时空档与重复小时由 Go 时区数据确定地归一化，跨日窗口确定性地延续到次日。
- 多个许可窗口同时覆盖时，选择具体发生开始最早者作为 `basis`，平局按窗口 ID 决胜。
- 窗口变化只影响之后的判定：已经冻结在计划和租约组上的 `WindowDecision` 永不被重算或覆盖；同键租约请求在窗口变化后重放仍返回带原始解释的原租约组。
- 删除全部条目后计划恢复全天可上线；窗口定义可整份替换并递增版本，身份不变。
- 日历条目和冻结解释随 JSON 快照持久化，服务重启后按当时窗口还原；内置 `time/tzdata` 保证无系统时区库的主机解释一致。

### 节点与租约生命周期

```text
ready --lease--> leased --report--> ready
                   |
                   +--release/expire--> ready
```

- 隔离节点不能被分配。
- 一个节点同一时刻最多有一个 `claimed` 租约。
- 租约到期后不能提交探针。
- 首次有效探针完成后，租约转为 `reported`，节点恢复 `ready`。

### 区域版本规则

- 计划必须已有一个版本才能激活。
- 同一计划中的序列号必须严格递增。
- 序列号使用无符号 32 位整数。
- 条目数量范围为 1 到 500。
- 区域名称、所有者名称和规范名称必须规范化为小写并移除末尾句点。
- 重复加入完全相同的版本内容返回 `duplicate_revision`。

### 探针幂等

- `observed_at` 不能晚于服务时间五分钟以上。
- `latency_ms` 范围为 0 到 600000。
- 同租约同请求键同内容重放返回原探针，不重复计数。
- 同请求键不同载荷返回 `idempotency_conflict`。
- 探针标识符由服务生成，客户端不能覆盖。

### 一致性判定

- 至少需要一个探针。
- 成功比例按成功探针数除以全部探针数计算，结果保留 6 位小数。
- 每个参与节点都必须提交至少一个探针。
- 所有成功探针的 `observed_serial` 必须等于计划最新版本的序列号。
- 成功比例必须不低于 `minimum_success_ratio`。
- 最大延迟必须不超过 `maximum_latency_ms`。

不满足条件时结论为 `review_required`，并返回以下稳定原因代码：

- `no_probes`
- `node_not_observed`
- `serial_mismatch`
- `success_ratio_below_minimum`
- `latency_limit_exceeded`

## 5. 工作流

### W1 计划生命周期和区域版本

入口：

- `PUT /v1/plans`
- `PATCH /v1/plans/{id}/revisions`
- `PATCH /v1/plans/{id}/activate`
- `PATCH /v1/plans/{id}/complete`

步骤：

1. HTTP 层严格解码 JSON。
2. 计划服务校验资金、区域名称、版本内容和状态迁移。
3. 存储层在锁内创建计划或版本，并原子替换状态快照。
4. 返回带版本的新聚合。

成功行为：计划按 `draft -> active -> completed` 迁移，区域版本在激活前可追加。

失败行为：重复序列号、非法名称、缺少版本、版本冲突或非法迁移不会修改状态。

### W2 节点领取租约

入口：`PATCH /v1/plans/{id}/leases`。

步骤：

1. 入口校验计划、区域、请求键、租约秒数和数量。
2. 分配服务清理已到期租约。
3. 按区域筛选 `ready` 节点并以稳定顺序选择。
4. 在一个事务中把节点改为 `leased`，写入租约组和租约。

成功行为：每个节点获得一个有期限的 `claimed` 租约。

失败行为：计划非活动、无可用节点、请求键同键异载荷或保存失败时不会产生部分租约。

### W3 探针登记

入口：`PATCH /v1/leases/{id}/probes`。

步骤：

1. 入口校验租约、请求键、序列号、延迟和时间。
2. 探针服务计算载荷指纹。
3. 存储层写入探针、完成租约并释放节点。
4. 返回探针及关联租约状态。

成功行为：首条探针落库；同键同内容安全重放。

失败行为：租约过期、计划未激活、同键异载荷或持久化失败时保持原状态。

### W4 一致性判定

入口：

- `PATCH /v1/plans/{id}/assessments`
- `GET /v1/plans/{id}/assessments/latest`

步骤：

1. 入口校验计划、请求键和最新区域版本。
2. 判定服务收集计划探针并按节点分组。
3. 纯领域计算器生成比例、原因和最终结论。
4. 存储层写入不可变判定并更新计划摘要。

成功行为：返回确定性的 `consistent` 或 `review_required`。

失败行为：缺少区域版本、没有探针、请求键冲突或保存失败不会更新计划摘要。

### W5 日历窗口与上线门控

入口：

- `PUT /v1/plans/{id}/calendar/windows`
- `PUT /v1/plans/{id}/calendar/windows/{window_id}`
- `DELETE /v1/plans/{id}/calendar/windows/{window_id}`
- `GET /v1/plans/{id}/calendar`
- `GET /v1/plans/{id}/calendar/windows/{window_id}`
- `GET /v1/plans/{id}/calendar/evaluation`
- 门控点：`PATCH /v1/plans/{id}/activate`、`PATCH /v1/plans/{id}/leases`

步骤：

1. HTTP 层严格解码窗口 JSON，服务层校验模式、调度、IANA 时区、工作日与挂钟时间、适用区域。
2. 窗口在锁内写入快照并递增计划版本。
3. 激活与租约分配用同一求值器在服务时钟的 UTC 时刻解释；拒绝时返回 `window_closed` 和下一次开窗时刻。
4. 放行时把完整解释冻结到计划或租约组上，重放与后续窗口编辑都不改写它。

成功行为：窗口外拒绝、窗口内放行、暂停覆盖许可、跨日和夏令时发生得到确定区间，重启后日历与冻结解释可还原。

失败行为：非法时区或时间、未知计划或窗口不会修改状态；窗口关闭时租约分配不产生部分租约。

## 6. 模块边界

| 模块 | 路径 | 责任 |
| --- | --- | --- |
| entry | `cmd/zrc` | 进程启动、参数解析、信号关闭 |
| httpapi | `internal/httpapi` | 路由、严格解码、中间件和错误映射 |
| plan | `internal/plan` | 计划生命周期和区域版本规则 |
| fleet | `internal/fleet` | 解析节点档案、唯一名称和分页查询 |
| allocation | `internal/allocation` | 节点筛选、幂等分配组和租约生命周期 |
| probe | `internal/probe` | 探针校验、幂等写入和租约完成 |
| assessment | `internal/assessment` | 探针聚合和一致性判定 |
| calendar | `internal/calendar` | 带时区滚动日历、窗口登记与作用域求值 |
| domain | `internal/domain` | 聚合、值校验、状态迁移、纯日历计算和纯判定计算 |
| store | `internal/store` | 锁内副本事务和原子 JSON 持久化 |
| platform | `internal/platform` | 时钟、标识、游标和文件锁 |
| runtime-probe | `internal/workflowprobe`, `cmd/workflowprobe` | 真实 HTTP 路径验证 |

依赖方向：入口和 HTTP 层依赖应用服务；应用服务依赖领域规则和存储；领域包不依赖 HTTP、文件系统或网络。

## 7. 对外接口

成功响应：

```json
{"data": {}, "meta": {"request_id": "..."}}
```

失败响应：

```json
{"error": {"code": "validation_failed", "message": "human readable message", "details": {}}}
```

主要路由：

- `GET /healthz`
- `PUT /v1/nodes`
- `GET /v1/nodes`
- `PUT /v1/plans`
- `GET /v1/plans`
- `GET /v1/plans/{id}`
- `PATCH /v1/plans/{id}/revisions`
- `PATCH /v1/plans/{id}/activate`
- `PATCH /v1/plans/{id}/complete`
- `PATCH /v1/plans/{id}/leases`
- `PATCH /v1/leases/{id}/release`
- `PATCH /v1/leases/{id}/probes`
- `PATCH /v1/plans/{id}/assessments`
- `GET /v1/plans/{id}/assessments/latest`
- `PUT /v1/plans/{id}/calendar/windows`
- `GET /v1/plans/{id}/calendar`
- `GET /v1/plans/{id}/calendar/evaluation`
- `GET /v1/plans/{id}/calendar/windows/{window_id}`
- `PUT /v1/plans/{id}/calendar/windows/{window_id}`
- `DELETE /v1/plans/{id}/calendar/windows/{window_id}`

## 8. 工程机制

- 状态迁移：计划、节点和租约使用显式迁移表。
- 乐观并发：计划版本不匹配时拒绝变更。
- 幂等写入：租约分配和探针登记都保存请求键和载荷指纹。
- 日历门控：激活与分配共用纯函数日历求值器，放行解释冻结后不再随窗口编辑改写。
- 资源生命周期：租约带截止时间，支持释放、完成和过期清理。
- 原子持久化：事务先复制状态，成功后再通过临时文件替换快照。
- 错误传播：领域错误稳定映射到 HTTP 状态和错误代码。
- 边界校验：DNS 名称、IP、序列号、延迟、比例、分页和能力均显式验证。
- 并发入口：HTTP 处理器可并发运行，状态修改通过互斥锁串行化。

这些机制为后续工程任务提供了多个独立行为面，但本阶段不创建缺陷、不写测试，也不生成缺陷票据。

## 9. 验证计划

`.project-runtime.json` 的 `testing` 固定为 `deferred`，不生成单元测试、夹具或浏览器测试。交付前使用 `cmd/workflowprobe` 启动真实本机服务并在随机端口完成：

1. 计划创建、版本追加、激活、完成和非法重复迁移。
2. 节点分配、租约唯一性、幂等重放和无可用节点冲突。
3. 探针首写、同键重放、同键异载荷冲突和释放冲突。
4. 一致性通过、审核触发、最新结果查询和幂等重放。
5. 未配置日历全天放行、一次性与周期窗口、暂停覆盖、区域作用域、跨日与夏令时确定区间、重叠选基、解释冻结、重启还原和非法窗口拒绝。

每条探针使用独立临时目录，不访问外部网络，结束后关闭服务。
