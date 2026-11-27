# 资产怎样保存、更新和恢复

AgentMoor 保存的是带版本的字节。工作区里的一个文件改了或删了，不会自动改写、删除平台上的旧版本。业务应用应保存成功任务返回的固定引用，需要时按这个引用下载，而不是再次猜测“最新文件”。

## 四类资产

| 资产 | 怎样创建和更新 | 本地删除后会怎样 | 怎样恢复 |
| --- | --- | --- | --- |
| Workspace | Worker 对分配的工作区做完整快照。树、普通文件、权限或支持的相对链接变化后发布新快照；字节和可识别元数据都未变化时不产生重复版本。 | 文件移除会体现在下一份完整快照里。旧快照继续包含原文件。整个工作区根目录不存在时扫描会失败，不会生成远端删除操作。 | 下载指定快照，校验摘要并在同文件系统的临时目录完成恢复，最后发布到一个尚不存在的新路径。 |
| Session | 对应适配器读取该运行时自己的会话 ID、原始记录和支持的关联文件。原生会话内容变化后发布新版本；任务追问另有回合键。OpenCode 的原生导出/导入由其适配器调用 CLI。 | 删除本地 transcript 或其目录不会删除平台会话。若会话文件本就在工作区内，移除会体现在下一份 Workspace 快照；工作区外的独立历史目录不属于该快照。 | 按固定会话引用恢复文件，或调用已验证的原生导入；从成功 checkpoint 接续时沿用同一原生会话 ID。批量恢复会选择已提交的接续版本，避开未完成回合。 |
| Skill | 发现 manifest 后保留原始内容，连同脚本、参考文件打包。包内容或可识别元数据变化后产生新版本。元数据区分 `runtime-native` 与 `platform-managed`。 | 本地 Skill 不再被发现时，平台旧包和旧引用仍存在。工作区中的删除体现在下个 Workspace 快照；任务已固定的包不会因此升级、替换或消失。 | 普通恢复使用新路径。当前 Codex 任务固定包由部署端选择版本和 SHA-256，在每轮新的私有运行时 Home 安装完整包；不覆盖来源包或工作区原生 Skill。 |
| Artifact | 普通同步扫描声明的交付目录；任务使用 `out/`，冻结文件字节后发布。内容变更产生新版本；业务应用另外校验 schema 和金额。 | 普通资产列表保留历史记录。任务成功清单只列本轮 `out/` 的完整文件集：删除不进入新回合集，改名是新路径；先前固定引用仍可下载。 | 下载指定版本的原始文件。恢复命令要求新的文件路径，不覆盖已有文件。 |

这些规则只覆盖适配器声明、已验证的格式。恢复整个执行中的进程、迁移到另一个 CLI，以及恢复尚未落盘或发布的工具动作，不属于当前保证。

## 来源和固定引用

资产由 Tenant、Project、Runtime、Adapter 四项作用域，加上 `kind`、`id` 和版本定位。元数据保存 SHA-256、大小、格式、MediaType、Adapter/CLI 版本及可用的原生 SessionID；调用者的凭据决定可访问的作用域。

首轮文件的键可以是 `out/report.json`，追问则是 `turns/<uuid>/out/report.json`。它们可以各自都是版本 1，互不覆盖。原生会话 ID 保持不变，不等于所有回合共用同一个平台会话包键。

成功任务或回合返回的 `assets` 包含确切 `kind`、`id`、`version`、`sha256` 和 `content_path`。业务消费者按主文件路径选择 Artifact，并保留固定引用。任务 `checkpoint` 把最近成功的 Workspace、Session 和既定 Skill pins 绑定在一起；失败、取消或中断不越过这份成功状态。部分上传或资产列表中单独出现的记录，不代表任务已经成功交付。[任务契约](task-contract.md)

## 删除和回收的边界

当前没有远端资产 `DELETE` API，也没有 tombstone 协议。本地删除、任务取消和普通 `gc` 都不是删除历史资产版本的接口。

`gc` 只在授权作用域里回收超过保留期限、没有任何已提交版本引用且没有受保护在途写入的对象。公开入口的过期窗口是 24 小时。它会看全部版本的引用，不只看 head：即使 Artifact 或 Skill 的 v1 已被不同字节的 v2 替代，v1 对象仍应保留。数据库里的版本记录不会因为本地文件删除而自动清空。

物理对象删除另有[持久回收审计](object-collection-audit.md)：先提交计划，再调用对象服务，最后区分成功 ACK 与恢复时观察到的不存在。新 Put 会先退休同 key 的旧 pending 意图；不会因此清除历史引用或缩短 24 小时窗口。对象服务不在 SQL 事务内，失败不能解释为已撤回外部删除。

未来若增加远端删除、保留期策略或 tombstone，需要另行定义权限、历史引用和恢复语义；本文不把这些当作已实现功能。

## 冲突和失败

Worker 先冻结待发字节，再上传、提交元数据和本地游标。目标版本已变化、且字节或可识别元数据不一致时返回冲突，保留本地冻结内容和平台已有版本；部署方先分别恢复到新路径比较，再按已审阅的平台版本明确选择。不自动覆盖另一方内容。[操作步骤](operations.md#5-处理失败或冲突)

任务发布的瞬态重试复用同一份冻结内容，不重新运行 Agent。永久错误或重试耗尽保留 pending；尚未提交的目标游标不前进。新追问使用新的 UUID，从最近成功 checkpoint 继续。进程丢失时，未发布的本地修改可能丢失；取消不会撤销已经发生的外部副作用。

## 已有验证

| 范围 | 真正运行的入口或测试 |
| --- | --- |
| 四运行时的资产往返、旧 Artifact 版本、Skill 附属文件、普通 Sync 的 journal 恢复 | [TestFourRuntimeRecovery](../internal/worker/sync_test.go)、`make demo` |
| 快照完整性、新路径、越界与损坏拒绝 | [快照测试](../internal/snapshot/snapshot_test.go) |
| 多文件清单、同名修改、删除/改名、旧引用与 Skill 扫描 | [TestTaskDeliveryCollectsAllArtifactsWithoutSkippingSkills](../internal/worker/task_artifacts_test.go) |
| 固定 Skill v1、新 Home、来源 v2 不漂移、原生加载负对照 | [Worker 测试](../internal/worker/task_skills_test.go)、`make test-k8s-skills` |
| 发布边界失败、冻结字节、单次原生执行、旧 checkpoint 与 pending 游标 | [任务发布故障矩阵](../internal/worker/task_publication_retry_test.go) |
| head 变成 v2 后，GC 真删孤儿而保留 Artifact/完整 Skill 的 v1/v2，随后通过 HTTP 核字节和摘要 | [历史引用测试](../internal/platform/historical_gc_test.go)、`make test-services` 的 PostgreSQL/S3 场景 |

各次运行的提交、后端和限制以[验收矩阵](verification-matrix.md)为准。单项测试通过不等于所有后端、所有 CLI 和所有故障组合同时通过。
