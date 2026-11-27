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

Codex CLI `0.171.0-alpha.13` 在 kind 0.33.0 / Kubernetes v1.35.0 中，经脚本模型请求受信 MCP 工具，完成真实 CSV 文件读写、三次不同 Pod 的同会话接续、两份 Artifact 的固定版本交付、旧版下载、业务数据库写入失败后重试及活跃工具取消。`make test-k8s-file-report` 通过；四 CLI 的 `make test-k8s` 文本矩阵、`make check`、`make demo`、`make test-services` 和本机 Codex `.13.2` 的 native roundtrip 也通过。[完整结果与边界](verification-matrix.md#2026-11-26codex-文件工具集群验收通过) · [结构化证据](evidence/codex-file-report-2026-11-26.json)。本节短验收没有覆盖真实模型、固定 Skill 注入或活跃工具中 Pod 突然丢失；后两项的独立补充结果见下文，真实模型仍未实跑。

## 2026-11-26：活跃 Worker Pod 丢失补充验收

测试提交 `c91198683ef002f211e0bc1a032a308b44432579` 在真实 MCP 工具进入阻塞后，对专属 Worker 容器发 SIGKILL（退出码 137）并删除 Job/Pod，保留生产 15 分钟期限。公开 API 在期限前保持 running、期限后约 0.09 秒显示 interrupted；原 UUID 重投两次没有新 Job，新 UUID 在新 Pod 按同一原生会话恢复审计。checkpoint、旧版摘要与两条业务记录保持不变，未归档草稿没有恢复。`make test-k8s-pod-loss`、`make fmt/check/demo` 和 Python/Shell 离线检查通过。本次使用固定 Codex 0.171.0-alpha.13、脚本模型、SQLite/本地对象存储，没有指定 Skill。[复验与边界](pod-loss-acceptance.md) · [结构化证据](evidence/codex-pod-loss-2026-11-26.json)。

## 2026-11-26：固定 Skill 包补充验收

Codex Adapter `0.5.0` 的 `make test-k8s-skills` 通过。真实 Linux Codex `0.171.0-alpha.13` 在三个新 Pod 中加载相同 v1 原生 Skill，工具实际执行恢复包内脚本并读取参考文件；来源 head 升到 v2 后不漂移，相同输入没有 Skill 的负例明确失败。来源包、工作区原生 Skill 和旧报告未被覆盖。[完整证据与边界](verification-matrix.md#2026-11-26固定-skill-版本与原生加载)。

## 2026-11-26：PostgreSQL / S3 下的 Codex 任务与队列恢复

`make test-k8s-persistent` 在同一专属 kind 集群和 Codex 镜像中，依次跑通 PostgreSQL 18.7 + MinIO RELEASE.2026-04-17T00-00-00Z 的文件与固定 Skill SaaS 契约，包括 queued 追问接受后真实更换控制面 Pod、原 UUID 重投与单 Job 调度。文件、取消、业务数据库失败重试、旧版下载、Skill v1 不随 v2 head 漂移等断言保持；直接核对真实后端，确认没有 SQLite / 本地对象回退。[结果与剩余边界](verification-matrix.md#2026-11-26postgresql--s3-联合集群与队列恢复) · [结构化证据](evidence/postgres-s3-contract-2026-11-26.json)。未组合活跃 Worker Pod 丢失及发布边界故障，不视为 V2 整体完成。

## 2026-11-26：任务发布边界补充验收

`84461dd` 为任务发布增加最多三次的局部瞬态重试，复用冻结字节和原操作键，不重新执行 Agent、不重扫目录；普通 Sync 仍在首次错误后依靠下一轮 journal 恢复。SQLite + 真实 HTTP/Worker 的 Begin/Upload/Commit 前后、实际元数据失败、取消/期限、永久错误和耗尽场景均通过，单次执行、单版本、旧 checkpoint 与 pending 游标均有断言。Artifact/完整 Skill 的非 head 历史版本在 GC 后仍能通过 HTTP 读回，SQLite 和 PostgreSQL/S3 服务测试都通过。[详细结果和范围](verification-matrix.md#2026-11-26任务冻结发布重试与历史引用回收) · [结构化证据](evidence/task-publication-retries-2026-11-26.json)。PG/S3 的同一发布故障注入组合仍未验收。

PG/S3 集群复验的前两次 HTTP 500 保留为失败记录。补充诊断及 PostgreSQL 任务读连接隔离后，真实并发/取消读取和丢失写租约测试通过，`62106e5` 的文件与固定 Skill 两场景全部通过；未回退本地存储。四 CLI 完整矩阵来自较早的 `3c497396`，本次未把它写成最终提交重跑。[资产生命周期](asset-lifecycle.md) 另说明本地删除、历史版本与 GC 的现有边界。

## 2026-11-26：工作区文件内容复用

显式开启 `workspace_transfer: tar-parts.v1` 后，Worker 从冻结 tar 上传缺失的文件 body 和压缩清单；公开下载、摘要和恢复继续使用完整 `workspace-tar.v1`。5,000/10,000 文件同尺寸改单文件的总变更请求体约减少 99%，包括清单和元数据；首次上传与完整后端校验存在额外成本，默认保持关闭。历史版本、改名/权限/删除/相对链接、冻结重试、恢复和 GC 已有测试及真实测量。[使用、限制与原始结果](workspace-incremental.md)。

`e8deb796` 的 PG/S3 分片专项通过真实 Codex 文件和固定 Skill 的新 Pod 接续、取消后恢复、旧下载与 queued API 重建；S3 中确认实际分片引用且没有本地存储回退。[集群证据](evidence/workspace-parts-cluster-2026-11-26.json)。真实模型及 PG/S3 发布故障与无应答 Worker 丢失组合尚未执行，不据此关闭整个 P1/V2。

同一 `e8deb796` 的四 CLI 完整 `make test-k8s` 文本矩阵通过，含两次自动新 Pod 追问与两种取消；`make fmt/check/demo` 与集成 PostgreSQL/S3 服务回归也通过。固定版本、镜像及各自范围均在上述结构化证据中，原始大工作区测量仍保留此前的 `48e9290` 来源。

## 2026-11-26 UTC：持久化故障套件与转发回归

新增 `make test-k8s-v2`，同一 kind 集群和固定 Codex 镜像依次运行 PG/S3 文件、固定 Skill、发布故障及真实 Worker 失联。每场景重启 queued 状态的 API，再检查合法外租户凭据对历史固定版本、任务和 checkpoint 的拒读。执行中原 UUID 重投另检查完整记录和唯一 Job/Pod/容器不变。

`109b534` 的独立故障/失联入口已通过；完整套件 `54cb556` 最后一次外租户读取遇到 TLS 握手超时，整体失败，原始结果保留为[失败证据](evidence/postgres-s3-v2-suite-failed-2026-11-26.json)。离线子进程复现了旧转发夹具只消费启动首行、后续 stdout 管道填满时卡住的问题；`89c3c155` 改用临时文件和 `pread` 检查完整启动行。五项回归加入 `make check`，没有增加 HTTP 重试、放宽 404 或缩短生产期限。该复现与原超时相符，原异常链本身不直接证明同一原因。

`89c3c155` 的 `make fmt/check/demo`、五项转发回归及完整四 CLI `make test-k8s` 已通过；四 CLI 原生取消约为 2.3、3.0、3.8、3.8 秒，自有矩阵集群已清理。

同一干净源码的 `make test-k8s-v2` 最终退出 0：文件、固定 Skill、故障/失联三个 PG/S3 场景全部通过，各自完成 queued API 重建和合法外租户拒读，分别重读 16、17、48 份固定版本并核摘要。两个真实 running 回合原 UUID 重投各两次，Job/Pod/容器身份不变且零重启。八个发布边界恢复、三次 manifest 失败耗尽、14 个独立 Pod 原生计数各为 1，以及真实 SIGKILL 137 后的生产 900 秒期限均通过；观察 interrupted 比公开期限晚约 1.66 秒，新 UUID/Pod 沿原会话恢复，旧 UUID 不创建替代 Job。最终核验 PostgreSQL 18.7、MinIO 固定版本及实际 S3 parts，无本地后端回退，专属集群已清理。[完整套件证据](evidence/postgres-s3-v2-suite-2026-11-26.json) · [复验与限制](persistent-faults-acceptance.md)。

本次达到 P1/V2 无模型密钥工程门槛。真实模型按用户选择后补；多控制面副本、生产 HA、不可信代码沙箱和凭据治理仍属后续阶段。分钟级公开 GC 只验证 24 小时窗口内的历史/pending 保护，过期孤儿实际删除由服务测试另证，不扩大本次结果范围。

## 2026-11-27：P2 执行门禁与 gVisor 运行时探针

用户已确定允许客户代码与第三方 Skill，并先面向可自行配置的 Linux Kubernetes/gVisor 集群。密钥轮换和单凭据撤销已通过独立真实 HTTP/CLI 验证并合入。[凭据验收](authentication-policy.md#2026-11-27-实际验证)

实现 `cf143f8` 增加任务 `execution_policy`：空值/`trusted-local.v1` 保留受信开发路径；`untrusted-isolated.v1` 在没有独立提供方时，于凭据读取、授权/claim、版本探测和执行前返回 `ErrIsolationUnavailable`。真实 HTTP 首轮/追问与 CLI 负例确认 0 请求、0 原生命令、无执行目录或成功资产，queued 记录、已有 checkpoint、游标和固定旧字节不变。显式 trusted-local 的合成原生子进程正例成功；任务文本不能覆盖部署策略。普通原生导入/导出仍是受信维护范围。

整合已完成凭据治理后的干净源码 `6db69fc2c591fa2c0128f002927c8825146f948e` 通过 `make fmt/check/demo` 和完整四 CLI `make test-k8s`；四份镜像独立实读版本、两次新 Pod 追问和两种取消均通过，原生取消约 4.5/3.1/3.8/3.8 秒，专属集群及镜像标签已清理。该矩阵仍为默认受信文本工作流。[门禁与回归证据](evidence/untrusted-execution-gate-2026-11-27.json)

独立的 `make test-k8s-gvisor-feasibility` 真实通过 runsc/systrap BusyBox 与缺 handler 拒绝，保留官方包校验、CRI、同 sandbox Sentry、实际用户及清理证据；八项离线反例已加入 check。它的实际脚本来源按基础提交和未提交文件 SHA 记录，不替换成后续文档头。[运行时验收](gvisor-feasibility.md)

应用隔离提供方、无平台凭据的交接、模型网关、网络/资源与真实原生 CLI/Skill 的沙箱工作流尚未完成，因此不可信任务门禁继续关闭。运行时探针不改变该状态。

## 2026-11-27：原生 Guest 核心的真实 gVisor 文件接续

`b45250b` 的有界 `native-guest.v1` 核心，通过固定 Linux Codex 与脚本模型，在三个新 runsc/systrap Pod 中完成文件生成、恢复后修改和核验；原始 Session 与同一原生 ID 保持，实际工具报告和工作区变化独立验算，每 Pod 原生执行一次。完整输出封存后交接，工作区和会话捕获共用最多 64 MiB 输出预算。

默认 Gofer 工作挂载的原子目录发布先失败，原始来源与诊断保留；仅换宿主 Memory 卷仍不够。受信运行时设置固定大小 Sentry 内存覆盖层后通过，并实测无覆盖碰撞、镜像根和 DownwardAPI 只读拒写。没有放宽公共快照的原子发布规则。[命令、实际来源、失败记录与边界](native-guest.md)

同一核心源码的四 CLI 默认文本 `make test-k8s` 全部通过；合入存储组件后的 `4aef8c8` 的 fmt/check/demo 通过。生产 Pod 控制器、能力与模型网关、SaaS 发布、真实 Skill 的沙箱验证、取消及网络/资源对抗仍继续推进，`untrusted-isolated.v1` 保持关闭，真实模型后补。

## 2026-11-27：任务与沙箱的类型化交接

`628d849` 提供首轮/追问共八条 execute 权限的 POST 接口、固定 claim/policy/profile 的输入准备和独立完整外包 SHA-256/大小下载校验。平台按固定版本封装 Workspace、原生 Session 与 Skill 原字节，输入总包最多 64 MiB，不在可信宿主导入会话或运行 CLI。缺 profile/controller 明确拒绝，同一 attempt 不能切换本地与隔离策略，旧记录和围栏指纹保持兼容。

该源码的 fmt/check/demo、真实 PostgreSQL/S3 输入准备及完整四 CLI 默认文本矩阵通过；四 CLI 原生取消约 2.3/3.1/3.1/3.1 秒，集群和本次镜像标签已清理。合入能力网关后的 `bb6cd7c` 再次通过 fmt/check/demo，没有把前述存储/集群结果改写为合并后重跑。[实际来源与证据](sandbox-task-handoff.md#2026-11-27-实际验证)

新增 HTTP/controller 测试使用明确合成 fixture；四 CLI 回归仍是受信开发路径。Worker 与 `serve` 尚未连接本组件，不可信执行门禁保持关闭。真实隔离 SaaS/native Skill、私有 CA 模型链路、CNI、资源耗尽、真实模型和生产 HA 均未由本次验证。

## 2026-11-27：隔离结果的可信发布组件

`e79066b` 增加 Worker 私有数据发布入口：先核对当前分配、固定版本及 Ready/正常退出/Pod 删除证明，再完整验证独立整包摘要、原始 Workspace/Session 和全部交付文件，最后复用 journal 的冻结发布及重试。真实 HTTP 合成适配器的所有宿主原生方法均为零调用；初次/追问的新 Workspace 沿用同一 Session，原始 tar 与会话字节、多 Artifact 和完整 Skill 均校验通过。503/TCP 应答丢失、耗尽、取消、期限及 pending 恢复保持旧成功 checkpoint。

同一实现补齐 JSON 深度/节点、tar 隐式父目录/路径限制，以及 `000` 目录权限导致的失败暂存清理；三个旧实现负例保留实际失败日志。`make fmt/check/demo` 与完整四 CLI 默认文本 `make test-k8s` 通过，16 组断言、8 次追问全部通过，原生取消约 3.0/3.1/3.7/3.0 秒，专属集群与任务镜像标签已清理。合入 main `c18e3bd` 后的干净 `1698547` 再次通过 fmt/check/demo，四 CLI 来源仍如实记为 `e79066b`。[复验与边界](isolated-result-publication.md) · [结构化证据](evidence/isolated-result-publication-2026-11-27.json)

本次只交付数据发布组件，`serve`/`run-task` 尚未接线，不可信执行门禁继续关闭。合成发布测试和受信四 CLI 回归不代表真实隔离 SaaS、native Skill、私有 CA 模型全链路、CNI、资源耗尽或真实模型已通过。


## 2026-11-27：gVisor 网络、资源与运行时回收

干净来源 `38a7a39` 的 R20/R21 在官方 containerd `2.4.2`、固定 gVisor systrap/512 MiB overlay、Calico `3.33.0` 和禁 swap 的 ARM64 参考节点中，通过真实网络正反例及 CPU、内存、ENOSPC、进程数、原始截止和整 Pod 取消。生产网络 helper 在策略移除和默认 CNI 的 Kubernetes API canary 可达时拒绝；合成启动/原生执行计数有独立记录。

资源实测区分 guest 的不可提升 hard NPROC 64 与宿主 Pod 的 pids.max 256：62 个新子进程后 EAGAIN、升到 65 拒绝、无 OOM/宿主 PID 事件。旧 128 宿主预算确实先触顶，保留为失败。三个额外新 Pod 的文件内存 OOM 均先确认 Sentry/gofer/shim 消失，再清理已停止 CRI 历史，没有用信号回收促成验收。

旧 containerd 2.2.0 的 task v3 删除顺序曾留下 idle shim。R17 的真实时序与非致命栈匹配官方 `402eb316` 修复；参考安装按 2.2.0→2.3.7→2.4.2 逐 minor 验证，旧 32 预算的 R18 仅作为机制对照。完整来源、固定摘要、失败索引及整合后四 CLI 回归见[复验与边界](gvisor-network-resources.md)和[精选证据](evidence/gvisor-network-resources-2026-11-27.json)。本组件仅运行合成程序；实际生产提供方、真实 CLI/Skill/SaaS、并发配额及真实模型另有验收，`untrusted-isolated.v1` 门禁仍待 INT-81。

合并 main `d307e7e` 后的整合源码 `2b2538d` 已通过 fmt/check/demo、15 项离线证明负例和完整四 CLI 默认文本契约，16 组检查与 8 次追问通过；取消耗时为 3.8/3.0/3.7/2.3 秒，专属节点与四个镜像标签已清理。此回归仍是受信开发路径，R20/R21 的来源仍分别保留为 `38a7a39`。



## 2026-11-27：生产隔离提供方的 Codex 文件与 Skill 验收

`19d0d5b40a48226402768f49d49228f9845901a3` 将生产 `serve --sandbox-config`、八条交接 API、固定 controller、私有 CA TLS 模型/能力网关、data-only Worker 与真实 Linux Codex 接成完整路径。文件、固定 Skill 各三轮新 gVisor Pod，在来源 head 升为 v2 后仍执行 v1；同一原生 Session、旧版本字节和独立业务验算保持。另通过活跃工具取消（1.247 秒）、精确 UID guest 删除、失败原 UUID 重投两次不新建 guest，以及新 UUID 从旧 checkpoint 恢复。共十个 Pod 原生计数各 1，guest RLIMIT 64；实际 containerd 2.4.2、Calico 3.33、runsc/systrap、宿主 PodPidsLimit 256。三次原生 Skill 各证明九个私有路径不可读、凭据环境缺失、两个禁止端口超时与只读根 EROFS；每次删除后独立观察无同归属 Sentry/gofer/shim 和 Ready CRI，最终专属节点及标签已清理。[完整实际结果与限制](sandbox-provider.md#2026-11-27-实际验收) · [原始证据](evidence/sandbox-provider-2026-11-27.json)

同一干净源码的独立 `pods=1` 配额探针通过首 Pod 201、第二 Pod 403/无 UID/无 CRI、自然清理和额度恢复；仅为合成 admission 证明。`make fmt/check/demo` 及完整四 CLI 默认受信文本矩阵也通过，原生取消约 3.7/3.0/3.0/3.8 秒。[证据索引与源码/日志摘要](evidence/sandbox-provider-index-2026-11-27.json)

本节更新此前组件阶段“Worker/serve 未接线、全局门禁关闭”的状态：明确配置且检查合格的 Codex 文件分配已有可用提供方；缺配置继续拒绝，不退回可信本地执行。`4ce08cf` 的隔离文本补充结果如下；其他 CLI 隔离路径、真实模型、生产 HA、PG/S3 与整条隔离路径组合均未由此验证。上述实际模型是脚本 Responses 服务，业务与不可信 Skill 全为合成数据。


`4ce08cf815581dfc5d31820aed3756e8ee5975ed` 的 `--scenario text` 独立通过生产隔离路径：没有 FileTool/pins，三个不同 gVisor Pod 沿同一原生 Session 返回随机 CSV 合计、前次确认码和新校验码。三次原生计数各 1、RLIMIT 64，11 份固定资产旧字节复读不变，每轮无同归属 Sentry/gofer/shim 和 Ready CRI，专属节点及本轮镜像标签已清理。实际三次上游请求均未携带 `text.format`，合法 named Responses SSE 通过原生产校验；没有开放任意 `json_schema`。[文本原始证据](evidence/sandbox-provider-text-2026-11-27.json) · [有界请求摘要](evidence/sandbox-provider-text-wire-2026-11-27.json)

前次 `24cf983` 因测试误选旧报告而失败，生产首轮和第一次追问已成功；原始失败保留，修复选择后才在新节点重跑。依赖缓存保留的是加入本轮 CA 前的 base，当前 Go 仍按 Dockerfile/COPY 编译，证据按各自源码记录；没有将此前 `19d0d5b` 的 fmt/check/demo/四 CLI 结果改写为后续源码重跑。[来源、摘要与限制](evidence/sandbox-provider-index-2026-11-27.json)


## 2026-11-27：P2.5 物理对象回收审计

INT-87 的 `b7b93573649b9cad2a2a899a69e7c0215135fa89` 保留原 24h/引用筛选，为完整对象、文件块及沙箱输入输出增加持久计划、实际 ACK/absence 确认和 Put 前旧意图退休；不声称 SQL 与对象删除原子或物理操作恰好一次。SQLite/DiskObjects 故障、四维作用域/类别、共享/保留引用、实际 guest 输出归属，以及真实 PostgreSQL/S3 的 ACK 前后故障、审计拒绝、重启均通过。旧 Go 的真实 lease-loss 负例单列保留；修复后已知死写者不发 Remove，健康新 owner 才沿原意图恢复。[实现、边界和原始失败](object-collection-audit.md)

同一干净集成源码的 fmt/check/demo、真实 services 和完整四 CLI 默认受信文本矩阵退出 0；原生取消约 4.6/3.0/3.0/3.8 秒，专属节点/项目/本次标签已清理。没有把这个回归当作新的隔离、真实模型或 HA 证明。[实际事件样本、源码、原日志与 SHA](evidence/object-collection-audit-2026-11-27.json)

## 2026-11-27：节点失联的 guest 停止证明

INT-90 保留旧实现 R2/R3 的真实失败：只停止自有节点 kubelet，API 404 后旧 Catalog 错记 PodDeleted=true，同一个 CRI/Sentry/单 child 仍发出三条新心跳。新控制器为 Pod 加 finalizer；只凭当前唯一 guest 的真实 terminated/restart0/节点与容器绑定，或从未调度且删除中的精确 UID/RV/spec CAS，才保存停止证明并释放对象。GET 404、PhaseFailed、NodeReady 不造证明；新的结果下载、服务端成功 Finish 和 sandbox GC 同样拒绝无证明记录，旧历史读取与已成功 ACK 保持。

`bc9b29f` 的 R5 真实 gVisor 组件四场矩阵通过：Pending 的已授权绑定返回409；正常取消与节点恢复先有真实 term143；失联和 PhaseFailed/CS仍Running 两窗各三条新心跳、同目标可达对照和直接 TCP 禁止目标超时；管理员越过 finalizer 后保持 Unknown，恢复节点也不补造证明。PhaseFailed 对象仍占 count/pods，额外 Pod 403；真实删除后同额度允许201且无CRI。原 POST1、身份、deadline 和旧 checkpoint 不变，专属节点/标签/临时凭据清理完成。[实现、实际结果与边界](node-loss-cleanup.md) · [原始与精选证据](evidence/node-loss-cleanup-2026-11-27.json)

这些证明分别表示受信运行时报告 guest 停止、API 对象删除；固定版本下实际 Sentry/gofer/shim 自然回收另行观察。合成组件与有限网络 canary、对象预算不等于真实模型、所有互联网隔离、跨节点物理总额度、硬件 fence 或生产 HA。

独立 `3564b83` 的生产文本提供方另通过真实 Codex 三轮、73条运维事件和11份固定资产复读；只读白名单观察取得3个真实 exit0 停止证明，与实际 UID/node/CID、封存输出和 accepted 一致，停止证明先于删除和成功接收。该源 fmt/check/demo/services/full4 均通过。再合 INT-91 完整头后的 `c383c85` 也通过全部五项（真实 services 43.865秒，四 CLI 取消3.0/3.1/3.8/2.3秒）；两轮均独立核实临时资源清理，原始来源分别保留在上述索引。

## 2026-11-27：P2.5 可信期限巡检

`3c901611485b2d20460644129bc98cd7b0665ee2` 的默认 serve 在有界主键分页后，仅把原期限已到的运行/取消中 Task、Turn 及排队 Turn 转为中断，与固定 `deadline-controller/system` 审计同一 leased SQL 事务；原 JSON 仅替换唯一顶层 state，保留旧 checkpoint、pins、资产、attempt 与期限，不调度执行或伪造清理 ACK。SQLite/真实 PG 的审计/租约故障、Finish 原期限并发、旧字节、分页/坏记录、重启幂等及服务退出通过。真实 BEGIN/已执行 SELECT 回应黑洞分别按 4/2 秒 I/O 边界退出；普通慢 SQL、取消和预算耗尽后原写者 PID/租约与合法写入保持。旧 NUL 键截断和维护预算杀死写者的真实负例单列保留。[实现、验收与边界](deadline-sweep.md)

同一干净源码 fmt/check/demo/services 与完整四 CLI 默认受信矩阵全部退出 0，16 个阶段、八次追问通过；取消为 2.3/3.1/3.0/3.8 秒，本轮节点/存储资源/镜像标签已独立确认清理。此回归使用脚本模型，不重标为真实模型、HA 或新的隔离资源验收。[真实事件、准确来源与原日志摘要](evidence/deadline-sweep-2026-11-27.json)

## 2026-11-27：隔离提供方与 PostgreSQL/S3 组合

干净 `1afa1ea` 的真实 Codex 同一 Session 三轮，各自新 gVisor Pod 且原生计数为 1；第一、二轮后各重建 API Pod，23/48 条已有审计和当前检查点精确保持。最终 73 条运维事件、11 个精确资产链接及旧字节复读、三次原公开 UUID 重放零新增事件通过。实际 PostgreSQL 18.7 和 S3 保留九个唯一 revision 对象（共 15 键），无 SQLite/本地对象回退；六个明确检查的存储环境变量均未进入原生执行。独立只读核对与实际专属资源清理均通过。[完整验收与准确边界](sandbox-persistent-acceptance.md) · [原始记录及来源索引](evidence/isolated-persistent-provider-2026-11-27.json)

## 2026-11-27：P3 共用 Adapter 契约验收首片

`be35146` 将 Codex 文件 Session 与 OpenCode 实际 NativeSessions 方法接入同一公开 HTTP 资产断言，CLI 使用有界合成程序。四类资产、原 Session/Skill 完整文件集合与字节、历史版本、新路径、实际 post-commit 503 重试及不兼容版本拒绝通过；后者在空的新分配验证导出、已有合法 Session 验证导入，避免无关错误掩盖门槛。四个临时 overlay 反例均准确失败；聚焦 race/check/demo 全通过，生产文件未改。[工具、版本与实际范围](adapter-contract.md) · [来源、诊断与无损日志](evidence/adapter-conformance-2026-11-27.json)。这仍是内部工具，未宣称稳定外部 SDK、真实厂商 CLI 或第三方工程师已验收。
