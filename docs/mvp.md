# AgentMoor v0.1 验收记录

2026-11-22 在 macOS 上完成首批运行时验证。按项目约定，首批为 Claude Code、Codex CLI、Gemini CLI 和 OpenCode。本次交付对应[原始 PRD](PRD-v0.1.md)第 10 节 MVP 和第 12 节验收。第 14 节的统一原生协议、跨 Runtime 会话迁移等属于后续阶段。

## 四种运行时的会话续聊

| 适配器 | 实测 CLI 输出版本 | 同步 / 恢复依据 |
| --- | --- | --- |
| `claude-code` | `2.1.330 (Claude Code)` | 专属项目目录的 UUID Transcript 和同 ID 的关联目录，恢复原生文件后按 ID 续聊。 |
| `codex` | `codex-cli 0.171.0-alpha.13` | 从 rollout 的 `session_meta` 读取 ID，保留日期目录，恢复后用 `codex exec … resume`。 |
| `gemini-cli` | `0.70.0` | 从项目 chats 读取会话 ID、项目标识和原始记录，恢复对应目录后用 `--resume`。 |
| `opencode` | `1.19.6` | 用原生 CLI 列出属于指定工作目录的会话、逐个导出，再原生导入到新数据目录，最后用 `--session`。 |

四项均用真实 CLI 创建第一轮会话，经平台同步后移除源工作区和运行时数据，用干净目录恢复，再发送第二轮请求。本机模拟模型检查新请求同时带有两轮不同的随机标记。实测覆盖主会话续聊。关联目录和批量恢复另有合成数据测试，分支、所有工具状态和全部历史 CLI 版本的行为仍取决于各运行时。[真实 CLI 测试](../tests/native_roundtrip.py) · [模拟模型](../tests/fixture_provider.py)

## PRD 验收对应表

| 条目 | 已实现行为 | 复验入口 |
| --- | --- | --- |
| 注册、探测、扩展 | 四个独立适配器，通过注册表选择。支持按 ID 查询能力、区分共享路径和运行时专属标记、比对已测 CLI 版本。可选接口表达原生会话能力。 | [入口测试](../cmd/agentmoor/main_test.go)、[注册表测试](../internal/adapter/registry_test.go) |
| Session 导出与恢复 | 保留可观察的原生格式，以外部 ID 和平台版本检索。恢复单条或整批历史。 | 上表四项真实 CLI 测试、`TestFourRuntimeRecovery` |
| Skill 与 Artifact | Skill 稳定打包附属资源，保存可识别的元数据。交付目录的文件逐个发布，可取回历史版本及关联会话。 | [四种适配器资产往返测试](../internal/worker/sync_test.go)、`make demo` |
| Workspace 与销毁恢复 | 比对完整快照摘要，原路径销毁后可在新位置恢复。同名目标、损坏内容、不合法路径会报错。 | [快照测试](../internal/snapshot/snapshot_test.go)、四项真实 CLI 测试 |
| 身份与租户隔离 | 服务端从签名凭据取得作用域。每次列举、上传、提交、读取及回收都校验权限。对象前缀按作用域隔离。 | [HTTP 权限测试](../internal/platform/http_test.go)、[平台持久化测试](../internal/platform/catalog_test.go) |
| 幂等与断点续传 | 按资产摘要判断变化，冻结待发字节。对象上传后数据库写入失败、提交成功后应答丢失、旧操作重放时都可重试。中断上传从该资产重新传输。 | `TestPublicationAndRecovery`、`TestFourRuntimeRecovery` |
| 事件、监听、定期扫描 | Hook 可调用单次同步。Watcher 监视子目录并合并短时事件，周期扫描补齐漏报，也可只用定时器。 | `TestWatchAndConflict/watch`、`TestWatchAndConflict/periodic` |
| 状态、冲突与回收 | 本地保存待办、最后成功时间和错误。崩溃后区分活跃与中断。版本冲突须按已审阅的版本处理。回收过期无引用对象。 | [游标状态测试](../internal/worker/journal_test.go)、`TestWatchAndConflict`、`TestPublicationAndRecovery` |
| 替换存储后端 | SQLite / 文件存储适用于单机。PostgreSQL 与 S3 兼容服务已实际联合验证发布、读回和回收。 | [真实服务测试](../internal/platform/services_test.go)、`make test-services` |

## 实现边界

- 元数据以租户、项目、逻辑运行时和适配器四项为作用域。恢复实例沿用同一逻辑运行时身份。跨租户共享和全局 Skill 分发留给控制面。
- 工作区以整个快照为版本。每次扫描读取内容，只向平台发送有变化的资产。工作区中删除文件会体现在下一份快照，历史交付文件及会话版本继续可读。
- 当前支持 macOS 和 Linux。一个目录服务由一个平台进程负责，可接入多个独立 Worker。快照捕获磁盘上可观察的状态。需要精确交接时，先让 Agent 落盘再同步。单资产上限为 8 GiB。

## Kubernetes 任务验收

2026-11-25 单独复验了 Codex 的文本交付、换 Pod 续聊和取消，以及同版本 Linux CLI 的 `tests/native_roundtrip.py`。另一次文件交付尝试停在首轮工具审批。本地两个真实 Worker 进程已完成文件发布、恢复和 SQLite 去重，所用会话由测试适配器提供。[命令、固定版本引用和哈希](verification-matrix.md#2026-11-25worker-文件交付)

同一个 SaaS 客户端提交随机 CSV、验算金额并追问。初次交付后，测试删除原 Pod。SaaS 每发一个新问题，平台自动拉起一个新 Job；SaaS 按回合下载答案。测试在第二次追问前也会删除第一次追问的 Pod。[SaaS 客户端](../tests/saas_contract.py) · [集群验收](../tests/kubernetes/contract.py)

| 适配器 | 首轮报告交付并验算 | 连续两次由 SaaS 追问，每次换新 Pod 并取回答案 | 在运行中取消首轮和追问 |
| --- | --- | --- | --- |
| Codex CLI | 通过 | 通过 | 通过 |
| Gemini CLI | 通过 | 通过 | 通过 |
| OpenCode | 通过 | 通过 | 通过 |
| Claude Code | 通过 | 通过 | 通过 |

第二次追问只带本轮的新校验码。正确答案必须同时包含原始 CSV 的合计、上一次追问的随机码以及本轮校验码。每次提交都重发一次，并检查只有一个对应 Job。取消测试分别等待首轮和后续追问的模型请求到达，再检查 Worker 确认、Job 退出，且已取消的回合没有成功交付物。追问取消后，最近成功的工作区和会话引用保持一致。本轮使用本机 kind 集群、Linux ARM64 原生 CLI 和确定性模型端点。版本、各 CLI 的接手方式及复验命令见[任务契约](task-contract.md)。

## 2026-11-26：Codex 文件工作流补充验收

Codex CLI `0.171.0-alpha.13` 在 kind 0.33.0 / Kubernetes v1.35.0 中，经脚本模型请求受信 MCP 工具，完成真实 CSV 文件读写、三次不同 Pod 的同会话接续、两份 Artifact 的固定版本交付、旧版下载、业务数据库写入失败后重试及活跃工具取消。`make test-k8s-file-report` 通过；四 CLI 的 `make test-k8s` 文本矩阵、`make check`、`make demo`、`make test-services` 和本机 Codex `.13.2` 的 native roundtrip 也通过。[完整结果与边界](verification-matrix.md#2026-11-26codex-文件工具集群验收通过) · [结构化证据](evidence/codex-file-report-2026-11-26.json)。真实模型、Skill 固定版本注入及活跃工具中 Pod 突然丢失仍未验收。
