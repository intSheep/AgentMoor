# 物理对象回收的持久审计

[INT-87](https://linear.app/intsheep/issue/INT-87) 为完整对象、workspace 文件块、沙箱输入/输出增加运维审计。沿用现有 24 小时公开保留窗口、冻结候选、全部历史版本与共享/活跃引用保护；不新增资产删除接口，也不改变哪些字节应该保留。[原回收规则](asset-lifecycle.md) · [仅运维读取](audit.md)

对象存储与 SQL 不是同一个事务。流程先记下“准备删除”，再调用对象服务，最后登记实际收到的结果。没有确认的计划表示结果仍不确定，不能当作删除成功，更不能据此推断物理操作恰好执行一次。

## 计划、确认和新写入

所有步骤保持 `Catalog.mu` 与现有独占服务租约。三个原 collector 的候选查询不变，持久 pending 意图不能绕过每次扫描的引用/保留筛选。

| 事件 | 记录的事实 | 这一步的 actor |
| --- | --- | --- |
| `object.collection_planned` | 在 leased SQL 事务内写入 pending 意图与事件，提交成功后才允许调用 `Objects.Remove`。 | 当前 Collect 的授权凭据摘要/角色，或明确的内部系统身份。 |
| `object.collection_confirmed`，`remove_ack` | `Objects.Remove` 返回成功，随后 SQL 确认与事件在另一个事务内提交。 | 实际执行本次 Collect 的身份。 |
| `object.collection_confirmed`，`absence` | 重试原 pending 意图时，原 selector 仍允许回收，实际 `Exists` 返回不存在；没有伪造一次 Remove ACK。 | 此次恢复/重试的身份，可以不同于原计划的身份。 |
| `object.collection_superseded` | 新 Put 可能重建同 key 的字节，先将旧 pending 意图退休；旧删除结果仍未知，不能再把它写成 confirmed。 | 实际新写入者；沙箱输出保留经认证的 `guest-capability/output` 与 Run/attempt 域摘要绑定。 |

每个意图有持久随机 `operation_id`。ACK 丢失、确认审计失败或进程重启后，仍使用原意图；全部状态与其事件同事务提交。新的 Put 前须先审计退休旧 pending，失败时不能开始 Put；即使 Put 自己失败，这次保守退休也不回滚成“旧删除已完成”。之后的新对象回收使用新 operation ID；已 confirmed 对象重现也不会复用旧 ID。

对象回收专属字段为 operation ID、对象类别、域分离的 `object_id_sha256` 和受限确认 proof；另保留公共 schema/cursor/time、四维 Scope、task_id 及固定 actor/component/action，guest 输出退休事件可关联 Run ID 和 attempt 域摘要。身份识别授权凭据或委托，不推断真实人员帐号。类别仅为 `asset_blob`、`workspace_part`、`sandbox_input`、`sandbox_output`；不保存对象 key、路径、正文、token、能力值/能力摘要或任意错误串。新字段不能混入旧动作，guest 不能提供审计身份。

`Collect` 的计数不是重试次数或跨进程累计物理删除次数：只有本次 Remove 成功且确认事务成功的对象才计入；已经不存在的恢复不增加数量。外部删除后若确认写入失败，该对象不计成功并返回错误，之前已完成的对象不能回滚。没有 pending 且对象原本就不存在时，不补造删除事件。

## SQL 和对象服务之间的窗口

计划与确认是两个事务，没有 SQL 事务横跨对象 I/O。真实 Remove 成功后，确认插入失败仍可能留下已消失的字节和 pending 计划；恢复时以 `absence` 记录实际观察。沙箱 run 的逻辑 `Collected` 另在后续事务提交；该事务失败不能撤回已经确认的对象删除，重试也不会重复产生对象确认事件。

对已有 pending 的恢复，`Exists` 可能阻塞。代码在它返回之后、开始 Remove 之前再次检查原 leased writer；已知死亡的 PostgreSQL 写者不能借池中其他连接继续删除。四个 Put 入口也在实际调用前检查 leased writer，并先完成必要的 supersede。

这些检查不是对象服务的分布式 fencing token：检查通过后再失去租约，已经开始的外部调用及 SDK 重试仍可能完成，无法撤回；SQL 最终确认必须失败关闭并保留可恢复状态。此改动不承诺跨 SQL/对象服务的原子性、恰好一次、生产 HA 或介质安全擦除，也不改变外部存储本来的删除语义。

## 实际验证

干净集成源码 `b7b93573649b9cad2a2a899a69e7c0215135fa89` 已合入审计 core 的 main `43e454cb58d3d92f7f2c39df80db029000505949`。`make fmt/check/demo`、`GOFLAGS=-v make test-services` 和完整四 CLI `make test-k8s` 均退出 0，前后源码未变。测试数据、凭据和工作区均为合成内容。[源码、逐项退出码、原日志 SHA、实际审计样本与清理证据](evidence/object-collection-audit-2026-11-27.json)

| 实际边界 | 结果 |
| --- | --- |
| SQLite / DiskObjects | 四类同 SHA 对象分别回收；真实 Remove 前/后失败、计划/确认/退休审计拒绝、重启、新 incarnation 与失败 Put 均有正反例。四维 scope、类别、历史/共享/pending/active 引用保护，以及公开 24h 窗口和精确 cutoff 等值保留通过。 |
| 真实 S3 应答故障 | 保持默认 SDK 策略：前故障共 10 次 DELETE 尝试、0 次转发，恢复收到 `remove_ack`；后故障共 10 次真正 HTTP 204 被测试 transport 丢弃，调用方报错，重启后以原意图的 `absence` 确认。不是“只有一个网络 DELETE”的证明。 |
| 真实 PostgreSQL 审计拒绝 | 拒绝计划插入时对象仍在、无计划事件；拒绝确认插入时对象已不存在、只有 pending 计划。重启后按实际观察恢复，不把 SQL 回滚写成恢复了对象。 |
| 已知写者死亡 | 用真实正向 S3 Exists 同步屏障，再 `pg_terminate_backend` 并确认旧 backend 消失。修复后原进程向对象服务发出的 Remove 为 0，对象保留、计划仍 pending；健康新 owner 才发出一次观察到的 Remove，以同一 ID、实际新 actor 确认。 |
| 实际 guest 输出归属 | 原 run 的输出已删但 ACK 丢失后，新同 Session turn 使用真正的新 output capability 写入同 SHA，退休旧意图；事件保留新 Run/attempt 绑定，错误/缺失绑定拒绝且审计不变。 |
| 四 CLI 回归 | Codex、Gemini、OpenCode、Claude 各完成首轮、两次自动新 Pod 追问和两种取消；原生取消约 4.6/3.0/3.0/3.8 秒。专属 kind 节点、本次四份镜像标签及三个独立存储验证项目的容器/网络/卷均独立确认已清理。 |

初始 `089b834` 的旧测试通过，不能当作后来发现的租约时间窗已通过。新增真实回归的 `ffcf97262a81a89d9e760355363f1cc7bbddb1ee` 保持旧 Go，实际复现 backend 已死亡后仍转发 1 次 Remove、对象消失；测试失败，`make` 退出 2（Go test 退出 1）。[原始失败日志](evidence/object-collection-lease_negative-test-services-2026-11-27.log)保留。随后独立修复提交 `2d4a798` 在 Remove 前检查 leased writer，再合主分支并运行上述完整验证；没有将旧通过或失败改写为新来源。

证据包含从真实运维 `AuditReader` 读取的八条有界事件。原计划已提交的恢复会保留原 initiator，并将后续步骤记为实际恢复者；如果计划插入本身被拒绝，恢复者会新建计划和确认，两条都属于它。事件与原日志已扫描，无能力值、私钥 PEM 或原始凭据赋值。

本片的四 CLI 只是默认受信文本回归；不新增真实模型、生产 HA、沙箱逃逸或所有原生格式的验收承诺。
