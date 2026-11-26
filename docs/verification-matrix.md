# 验证矩阵：怎样知道 AgentMoor 兑现了约定？

文档版本：0.1 · 基线：[`main@47a5eb9`](https://github.com/intSheep/AgentMoor/tree/47a5eb9ea3088944243fe4c5f4f1429b1cfefb33)；首个[公开预览](https://github.com/intSheep/AgentMoor/releases/tag/v0.1.0-alpha.1)指向不同提交 `bab04e9`。已记录表示已有仓库验收，不表示编辑本文时重跑。计划中的命令和接口变更不当成现有用法。

| 能力 | 入口或计划用例 | 状态与边界 |
| --- | --- | --- |
| 本机资产恢复 | `make demo` | 已有入口；重传及文件恢复，不运行原生 Agent。 |
| 四个原生 CLI 的 SaaS 文本回合 | `make test-k8s [AGENT=codex]`，详见[任务契约](task-contract.md#一条命令复验) | [已记录](mvp.md#kubernetes-任务验收)：Codex、Gemini、OpenCode、Claude Code 在 kind + 确定性模拟模型上交付、取消、连续两次换 Pod 续聊。模型端点[从请求取出并直接汇总 CSV](../tests/kubernetes/model.py)，不是 CLI 用工具读 CSV 和写报告。 |
| PostgreSQL + S3 兼容存储 | `make test-services` | [已记录](mvp.md#prd-验收对应表)：验证资产发布、读回和回收。不是同一次跨 Pod 任务端到端测试。 |
| 本机从 CSV 生成报告 | [文件工具样例](../examples/file-report/README.md) | 可用 `make test-file-report` 直接调用工具，或指定本机 Codex CLI，由回环脚本模型请求执行工具。独立验算读取磁盘上的实际文件。此入口只覆盖例 1 的本机文件生成步骤。 |
| Worker 交付工具文件，业务端下载并去重 | `make test-worker-file-report` | 本地真实 HTTP 平台、两个独立 Worker 进程、SQLite 导入器通过。测试适配器提供会话及最终文本。[本次证据](#2026-11-25worker-文件交付) |
| Codex 文件工具、文件恢复后再编辑 | `make test-k8s-file-report`，例 1–2 | 2026-11-26：固定 Linux CLI + 脚本模型通过；三个全新 Pod 接续、多文件下载、旧版不变。真实模型未跑。[证据](#2026-11-26codex-文件工具集群验收通过) |
| 固定版本 Skill 安装与原生加载 | `make test-k8s-skills` | 2026-11-26：Linux Codex + 脚本模型、三个新 Pod 使用同一 v1，发布 v2 不漂移；相同输入无 Skill 负例失败。[证据](#2026-11-26固定-skill-版本与原生加载) |
| Codex 文件任务取消、失联及重试 | 例 3、`make test-k8s-pod-loss` | 活跃 MCP 工具取消、SIGKILL 后等待原生产期限进入 interrupted、原 UUID 重投和新 UUID 恢复、业务数据库失败重试均已通过。[Pod 丢失验收](pod-loss-acceptance.md) |
| PostgreSQL + 对象存储下的 Kubernetes 文件/Skill 契约 | `make test-k8s-persistent` | 2026-11-26：同一次命令中两场景均通过，含 queued 追问的控制面 Pod 重启和原 UUID 重投；V2 的 Pod 丢失组合、发布边界故障注入仍待验收。[证据](#2026-11-26postgresql--s3-联合集群与队列恢复) |
| 任务冻结资产的发布重试 | `go test -race ./internal/worker` | SQLite + 真实 HTTP/Worker 故障矩阵通过：Begin/Upload/Commit 前后、真实元数据失败恢复、取消/期限、永久错误与重试耗尽。Agent 每轮只执行一次，冻结字节和操作身份不变；PG/S3 同一边界故障组合尚未注入。[证据](#2026-11-26任务冻结发布重试与历史引用回收) |
| 非 head 历史 Artifact/Skill 的 GC 保护 | `TestHistoricalArtifactAndSkillReferencesSurviveGC`、`make test-services` | SQLite 与 PostgreSQL/S3 均通过：v1 被 v2 替代为历史后仍保留完整字节；同次 GC 确实删除孤儿对象，旧版本经 HTTP 下载并核 SHA-256。 |

## 待落地的可运行示例

各例使用合成数据和一次性临时目录；由独立的 SaaS 客户端通过公开接口轮询与下载。Kubernetes 操作仅用于准备测试和注入故障；不让测试客户端用 shell 偷读平台数据库或工作区来代替 API 验证。文件输入的供给方式可先由部署方挂载或放入工作区，不假装现有 `text-turn.v1` 已支持 SaaS 上传通用输入文件。

### 例 1：文件到文件（Codex）

2026-11-24 在 macOS arm64 上运行了[本机样例](../examples/file-report/README.md)。Python 3.15.2 直接执行通过 4 项；Codex CLI `0.171.0-alpha.13.2` 经本机脚本模型调用同一工具也通过 4 项。原生回合的成功样本为 east 98 分、west 343359 分，共 43 字节，SHA-256 为 `84d0c25662e0eb031f1926fcf229ba37a270ace2393cf040a86c10fcd63c0eae`。其余三项分别验证缺文件、错误金额及已有报告。下列 SaaS 下载和集群验收仍待完成。

部署方提供只读 `sales.csv`，含重复地区、退款和每次运行重新生成的一条随机记录。任务仅引用路径，金额及随机值不进入初始指令。要求 Codex 用工具读取文件并生成 `out/report.json`；即使最终文本仅为 “done”，SaaS 下载的文件也要能独立验算。

**通过：** 记录可关联的原生工具事件或经脱敏的请求/响应证明，显示读取与写入；独立客户端通过固定版本的内容 URL 读回实际文件，校验 SHA-256 及整数分区汇总。另跑一次缺文件案例：以显式失败结束，不发布成功产物。脚本模型可驱动真实工具协议，但不得自行汇总 CSV 或伪造文件写入；真实模型版本只要求结构及文件内容正确，不断言具体措辞。真实模型 smoke 使用合成数据，并单独标注 CLI、模型、配置和运行时间。

### 例 2：销毁计算后继续修改已有文件

先跑例 1，并让工具写入一份只进工作区、不进入最终回复的 `state.json`，包含一枚新生成的随机标记。生成工具只返回文件路径，不回显标记；确认首轮回复和首轮模型消息中没有标记。删除 Pod A 及其临时卷。用同一原生会话 ID 发起第一次追问，新 Pod B 读取状态文件，追加一笔退款并生成新版本；再删除 B，Pod C 在同一会话中输出基于修改后文件的审计结果。后续请求不重发原始文件或标记，并用各自的 UUID 原样重复提交一次。

**通过：** 三次 Pod UID 不同，临时工作卷未共享；原生会话 ID 不变；可关联事件表明 Pod B/C 确实读取恢复后的文件，新结果包含工作区中的随机标记和更新后的汇总。原始交付物仍能按原版本下载且字节不变，后续每轮各一个 Job。负对照：禁用工作区恢复后，不能得到相同的成功结果。若 API 只交付文本或偷偷重放输入，不能通过。

#### 2026-11-25：Worker 文件交付

`make test-worker-file-report` 启动临时 HTTP 平台，使用 SQLite 元数据和本地对象存储。两个独立进程通过 `run-task --config` 和 `run-task --config … --turn …` 运行真实 Worker。测试程序只在自己的注册表中增加 `local-report-fixture` 适配器。该适配器调用已有 CSV 工具，并将最终文本的 east 写为 99 分。第一轮结束后删除整个分配目录，第二个 Worker 恢复会话、CSV 和原报告，追加 west 的 9 分退款，再生成报告。

| 本地任务 `file-report` | 实际工具文件（分） | 固定版本 HTTP 路径 | SHA-256 |
| --- | --- | --- | --- |
| 首份 | east 98，west 31 | `/v1/content?id=out%2Freport.json&kind=artifact&version=1` | `f72401084eda1ff431cf23833d909e242747c3ac0a641e90abf16db6183c9836` |
| 修订 | east 98，west 22 | `/v1/content?id=turns%2F3d0aca2f-b8d7-4813-bc14-685013fc49aa%2Fout%2Freport.json&kind=artifact&version=1` | `270255c78b6a7c4a3aba2222aee63729527db768191c281351a6505d4be3c96b` |

这次运行的测试会话为 `06bdd842-9539-43db-ba12-9f95fc05f213`。现有 `ingest.py` 拒绝了两次以最终文本作为预期的导入。按独立验算的金额导入时，每份依次返回 `imported`、`already-imported`。数据库共两行，两个 Worker 均退出后，两条引用均能下载原字节。每次复验生成新的会话和追问 ID。

同日，提交 `9c573b9` 的 `make test-k8s-file-report` 在一次性 kind v0.31.0 集群运行，使用独立 kubeconfig、OrbStack Docker 29.8.2 和 Codex `0.171.0-alpha.13`。首轮任务终态为 `failed`，脚本模型没有收到工具的写入确认。随后在同版本的独立诊断容器中，原生 `report.render` 事件从 `in_progress` 变为 `failed`，结果为 `null`，错误为 `MCP tool call requires approval, but approval policy is never`。这是该非交互调用的拒绝结果，没有产生等待用户确认的请求。没有生成首份集群报告，也没有触发修订 Pod。该提交保留诊断入口，当前内置适配器只声明已验证的文本模式。

本次集群命令使用的 `kind` 位于工作树临时目录，Darwin ARM64 可执行文件的 SHA-256 与官方 v0.31.0 release 记录一致，为 `88bf554fe9da6311c9f8c2d082613c002911a476f6b5090e9420b35d84e70c5c`。下面是实际入口，`tool_dir` 表示该临时目录。验收结束后移除了临时集群、kubeconfig 和工具。

```sh
# 9c573b9，文件回合停在首轮审批
PATH="$tool_dir:/Applications/OrbStack.app/Contents/MacOS/xbin:$PATH" DOCKER_CONTEXT=orbstack make test-k8s-file-report
# 当前分支，本地文件交付通过
make test-worker-file-report
```

#### 2026-11-26：首个 Pod 的诊断中断

在 intSheep 的 MacBook Pro 上，执行会话 `85346` 从 `9c573b9` 的临时副本调用了 `make test-k8s-file-report`。重新连接后，原会话显示 Make 退出码为 2，诊断程序退出码为 1，输出里没有 `FILE_REPORT_DIAG` 记录。当天的探针只转发集群名和完整诊断记录，因此无法确定停止在启动、工具调用或发布的哪一步。11 月 25 日观察到的原生 CLI 审批错误仍是上一次运行的证据。

OrbStack 对 `agentmoor-a7ac05925384-control-plane` 的事件显示，节点于 02:58:08 UTC 退出并销毁。随后按该次运行的名称检查，集群节点和专属镜像标签均已清除。恢复时只读取了原会话，没有重建集群。首个 Pod UID、实际 CLI 版本、原生 session ID、工具接收或写入状态及 Worker 的回执均未取回。两个 Pod 的标记恢复、业务端两版哈希和去重没有本轮验收结果，也没有可交给用户处理的待审批请求。

保留的[诊断程序](../tests/kubernetes/diagnose_file_report.py)现在分段输出集群就绪、镜像构建与加载、Worker Job 创建和任务提交等已观测事件。它区分 MCP 工具发现和 `tools/call`，同时核对原生事件、临时文件、发布入口、Worker 终态确认以及 SaaS 的 HTTP 记录。若调用真的到达测试工具，探针在写入前停止。以下离线检查通过；修订后的探针尚未在集群中运行。

```sh
python3 tests/kubernetes/diagnose_file_report.py --check
```

### 2026-11-26：Codex 文件工具集群验收通过

代码提交 `b153a3d33f8845cd843cf400f6ad40b30cfa9947`，macOS ARM64、Docker 29.8.2、kind 0.33.0、Kubernetes v1.35.0。固定 Linux ARM64 Codex `0.171.0-alpha.13`，使用脚本 Responses 模型、SQLite 与本地不可变对象存储。[结构化验收证据](evidence/codex-file-report-2026-11-26.json)。本机原生恢复往返另用 Codex `0.171.0-alpha.13.2` 通过。

`make test-k8s-file-report` 通过。只给部署端选定的 `report.render` 设置工具级 `approval_mode="approve"`，保留原生 `read-only`、禁 shell、apps、hooks 和多 Agent。工具的文件与外部动作权限由部署端负责。SaaS 指令只引用输入路径，脚本模型不读取 CSV、不计算金额；原生 MCP 工具实际落盘，最终回答仅为 `done`。成功后独立 SaaS 客户端经公开 API 下载固定版本，对金额、SHA-256、两份业务文件和原生会话记录逐项核对；缺输入返回 failed，成功资产为空。

| 回合 | 新 Pod UID | report.json SHA-256 |
| --- | --- | --- |
| 生成 | `ce0d7b56-670a-4516-8b56-65ed4495f492` | `9ff6adafeb0cb99ca79a546ed02272f312a4140c76b1db47f593317cc8c469bc` |
| 修改 | `a5db9578-bb41-4ff3-a74f-4102acf9135e` | `45e2ae257cfb30ffc9c9e036ec59fbf30914c6caa4e37ac43b7cb90d41eb1d0b` |
| 审计 | `89b962aa-c4fc-4ea3-bee6-c0946c8afc41` | `e6772bfd70bf3ac1133f6d1199874d2ac2de0eac49be57c704fa5be0cd1a167f` |

三轮使用同一原生会话，标识 SHA-256 为 `9b396ae4bac350bbe9816b31d9a52cda244d14f7c498bc2e1b9108a00889ecf3`。前两个 Pod 和各自 emptyDir 在追问前已删除；只存在工作区的随机标记没有进入原生历史或模型请求，第三轮从恢复文件读取。旧 Artifact 在后续修改、取消及恢复后仍能按原摘要下载。每轮 `out/report.json` 和 `out/tool-proof.json` 均在成功清单中；完整交付集、同名修改、删除/改名及 Skill 扫描另有 Worker/平台测试。

两份业务报告各自 `imported → already-imported`。修订报告入库前，用真实 SQLite BEFORE INSERT Trigger 触发写入失败，确认此前记录未变；移除 Trigger 后重试只产生第二条记录，没有重新运行 Agent。迟到重放第一份仍是 `already-imported`，数据库保持两行。`make test-worker-file-report` 也验证了真实 Worker 销毁后业务数据库失败恢复；独立消费者的 10 项 HTTP 测试通过，包括拒绝失败、取消、运行中和中断记录。

同会话负例在合成工具开始前移除恢复出的 CSV/state 后执行修订，明确失败且 checkpoint 不变。这是在恢复后模拟所需文件缺失，不是关闭生产 Worker 恢复的实测。另在真实 MCP 进程写出启动标志并进入阻塞后取消，观察到 cancelling/cancelled，25 秒内 Job 退出，没有成功资产；新 UUID 和 Pod `9c82006c-4aca-4e77-9e2d-c86beffcca10` 从最近成功版本继续审计，未归档草稿没有混入。这次短验收没有跑直接杀掉活跃 Worker Pod 的场景；后续保留生产 15 分钟期限的独立[Pod 丢失验收](pod-loss-acceptance.md)已通过。

同一代码的 `make fmt`、`make check`（含 race）、`make demo`、`make test-services` 与四 CLI `make test-k8s` 全部通过。后者仍是四个 CLI 的文本交付、连续新 Pod 追问及运行中取消；本节文件能力仅声明 Codex 已测。上述短验收未覆盖真实模型 smoke、固定版本 Skill 注入和 PostgreSQL/S3 下完整跨 Pod 流程，不据本次通过推断支持；固定 Skill 的后续独立结果见下一节，真实模型和后端组合仍未验收。

### 2026-11-26：固定 Skill 版本与原生加载

实现提交 `6ae64d2`，相同输入负对照提交 `e5c0fc5`；Codex Adapter `0.5.0`。`make test-k8s-skills` 在固定 Linux Codex `0.171.0-alpha.13`、kind 0.33.0 / Kubernetes v1.35.0、脚本 Responses 模型上通过。[结构化证据](evidence/codex-pinned-skills-2026-11-26.json)。

受信测试配置经公开 HTTP 发布完整 `skill-tar.v1` 并选择 v1；SaaS 指令只含报告任务和 CSV，不带 Skill 规则。每个新 Pod 都将原清单、脚本与参考文件恢复到私有 `CODEX_HOME/skills/pinned-report`。Codex 通过当前私有目录的规范路径原生加载正文；脚本模型只有在当前路径对应的原生 `<skill>` 片段中找到独有规则，才能请求正确的工具参数。MCP 工具实际运行包内 `scripts/check.py`，脚本读取 `references/rule.json`，并发布三个成员摘要的 `out/skill-proof.json`。最终文本仍仅为 `done`。没有把正文由 Worker 人工拼成模型指令。

三个成功 Pod UID 分别为 `1042997f-9218-466d-a97a-7e5a832db74d`、`852a4711-fe1e-4990-8ae1-aaffb4437eeb`、`d60bbcb2-f507-439d-a853-1221d63fd8eb`；每轮后都删除前一 Pod 与 emptyDir。原生会话 ID 摘要保持 `c4183ea8b0dae6b3ef617f41952a34dcad92f0348f2467b8bea2678b06e16732`。任务、Parent 和成功 checkpoint 均携带同一 v1 pin，其包 SHA-256 为 `cd4c51c1efc04319fa32c2480cf35593f1ab7670bff857e010b747c4d89ce26a`。首轮后发布 v2，后续两轮仍遵循 v1，原包与旧报告字节不变。相同 CSV 和指令、未安装 Skill 的独立任务明确 failed，成功资产为空。

平台测试另验证摘要、版本、权限、重复名称、过多 pins、同 attempt 幂等、配置漂移拒绝、取消后继承及控制面重启保留；Worker 测试验证完整包恢复、权限保留、已有目标冲突、私有路径边界和来源/原生 Skill 不被改写。`make fmt/check/demo/test-services`、四 CLI 的 `make test-k8s` 文本矩阵与本机 Codex `.13.2` 的 native roundtrip 通过；同一新版适配器的普通文件专项（含活跃工具取消、数据库重试和失败后恢复）也通过。真实模型、其他 CLI 的任务 Skill 安装，以及 PostgreSQL/S3 完整集群流程仍未验收。

### 例 3：取消、失联与重试

从已完成的首轮出发。第一次追问先进入一个可观测的长工具步骤，等待工具启动后发取消。第二个独立追问在等到工具启动后直接删除 Worker Pod，不给正常收尾机会。待终态可见后，用新 UUID 从最近成功的引用继续；先前请求 UUID 仅用于原请求重试，不作为重跑手段。

**通过：** 取消经过可观察的 `cancelling`，终态 `cancelled`；进程在用例声明的超时内退出。Pod 丢失后在文档规定的期限内进入 `interrupted`，不能被误报为成功。两次失败回合均无成功交付引用，之前的工作区/会话引用和版本摘要不变，新回合在新 Pod 从最近成功状态恢复。失败公开记录可识别，说明未发布本地内容会丢失，取消也不撤销外部副作用。

### 2026-11-26：PostgreSQL + S3 联合集群与队列恢复

源代码提交 `64522e6ace4710fc2cbc46639e0a1d70b88c12cd` 的 `make test-k8s-persistent` 通过。一个专属 kind 集群、一份固定 Linux Codex `0.171.0-alpha.13` 镜像，先后在两个独立命名空间运行文件任务和固定 Skill 任务。两者均使用 PostgreSQL **18.7** 元数据与 MinIO **RELEASE.2026-04-17T00-00-00Z** 不可变对象，原生 CLI 连接脚本 Responses 模型。[结构化结果及镜像身份](evidence/postgres-s3-contract-2026-11-26.json)。

| 同次命令场景 | PostgreSQL 成功存储记录 | 核对到 S3 的不同版本对象 | 控制面重启前 / 后 Pod UID |
| --- | --- | --- | --- |
| 文件交付、修订与取消恢复 | 2 tasks、5 turns、16 revisions、16 operations | 14 | `51f7d14d-2271-495d-b651-77c8b3562a95` / `891140d5-ac5f-4f42-a986-77c6b2e2d5a8` |
| 固定 Skill、修订与版本不漂移 | 2 tasks、2 turns、17 revisions、17 operations | 17 | `c1107900-4ece-4059-892f-31c364caacb2` / `17883f69-998c-4f75-add4-1758e2b64ced` |

每个场景的正常 SaaS 追问先被命名空间 Job 配额暂缓调度；公开 API 已持久接受 queued 回合后，再真实替换控制面 Pod。转发器重连同一个回环端口，SaaS 服务身份保持不变。重启后读取相同 queued 回执、parent/checkpoint，原 UUID 重投两次仍是同一记录；解除配额后只有一个对应 Job。新 Worker 从已归档的工作区/会话继续，早期 Artifact 的固定下载及摘要始终不变。

基础设施层直接核对 PostgreSQL 中实际存在 task/turn/revision/operation，所有 revision 的不同内容键都能在 S3 测试桶找到；API 数据卷没有 `catalog.sqlite` 或本地 `objects`，没有回退到默认存储。业务金额、文件内容、版本、checkpoint 与数据库导入断言仍由公开 HTTP 客户端完成，没有拿后端读数代替业务验算。服务镜像版本沿用 `tests/services.compose.yml`，记录包含实际镜像 ID。

完整保留 P0 的文件三 Pod 接续、两份交付物、旧版不变、真实业务 SQLite 写入失败后重试、活跃 MCP 工具取消及成功 checkpoint 恢复。Skill 场景的三个新 Pod 仍固定同一 v1，源包发布 v2 后不会升级；相同输入但未安装 Skill 的负例明确失败、没有成功产物。两份业务报告各导入一次且迟到重放不覆盖新结果。`make fmt`、`make check`（race）和 `make demo` 也通过。

此后合并 `main@a315958`，保留独立 Pod 丢失入口和参数互斥；集成提交 `a3ed8de` 的 `make fmt/check/demo`、Python/Shell/JSON 离线检查再次通过。persistent 专属入口在创建集群前检查 Docker CLI 平台导出能力、服务端 API 及 `DOCKER_API_VERSION` 均支持 API 1.48+，旧版本和缺参数情形有离线验证；普通入口不新增此限制。

这是 P1.1 后端组合与队列恢复的验收，不关闭整个 V2 门槛。尚未在 PostgreSQL/S3 下组合活跃 Worker Pod 突然丢失，或对象上传/数据库发布边界的故障注入；真实模型、多控制面副本和生产 HA 也未验证。独立 P0 Pod 丢失证据不能代替这些组合。

### 2026-11-26：任务冻结发布重试与历史引用回收

源码 `84461ddaf072d3dc26d4115b335e7b8559c378f9` 的 `make fmt`、`make check`（含 race）、`make demo` 通过。真实 HTTP 平台和真实 Worker 使用一个明确命名的测试适配器，调用独立子进程生成文件并计数；不是伪装成供应商 CLI 的测试。[结构化证据](evidence/task-publication-retries-2026-11-26.json)

在 Begin、Upload、Commit 前分别返回瞬态错误，并在各操作实际成功后关闭 TCP 连接丢弃应答，六个场景都在同一回合完成。每轮原生执行计数为 1；首次失败后修改活动报告文件，重复上传和成功交付仍为原先冻结字节。所有重试的 metadata、base 与操作指纹相同，只有一个成功版本和一个对应已提交操作。另用真实 SQLite BEFORE INSERT Trigger 阻止元数据写入，实际 Commit 返回 500；移除 Trigger 后同一冻结操作恢复，不重新执行 Agent。

HTTP 400/401/403/404/409/422/425/501 和 JSON 语法错误只尝试一次。三次 Commit 均失败时，pending 元数据和 blob 保留，目标游标不前进，失败回合无成功资产，最近成功 checkpoint 和旧版文件不变。实际 SaaS 取消及 caller context deadline 都能中断正在等待的发布请求。真实 httptest 的 20ms 响应头超时复现了“错误也匹配 DeadlineExceeded，但任务 context 尚有效”的边界，分类现在允许重试这个传输超时；任务 context 到期则停止。错误文本以及 404/409 的 errors.Is 身份保持原样。

普通 `sync` 没有新增本轮自动重试，原四运行时 `TestFourRuntimeRecovery` 仍先返回首次失败，再在下一次 Sync 从 journal 恢复。历史 GC 新测试先把 Artifact 与完整 Skill 包的 head 从 v1 变成不同字节的 v2，再让 GC 真正删除一个孤儿对象；v1/v2 四份固定版本仍能经真实 HTTP 下载并核对字节、SHA-256、作用域。该历史引用测试在 SQLite/本地对象和 `make test-services` 的 PostgreSQL/S3 都通过。服务测试第一次记录于祖先源码 `3c497396`；后续超时分类及 PG 查询隔离的回归分别另列。

完整 `make test-k8s` 在 `3c497396` 通过：Codex `0.171.0-alpha.13`、Gemini CLI `0.70.0`、OpenCode `1.19.6`、Claude Code `2.1.330` 均完成报告、两个新 Pod 连续追问，以及原生执行和追问取消。活跃原生回合取消约为 3.1、3.0、3.1、3.9 秒。这是后续超时分类和 PG 查询隔离修改之前的矩阵，不标成最新头提交重新跑过。

PG/S3 专项先在 `84461dd`、诊断增强后的 `852166d` 各失败一次，两次均保留为失败记录。第二次收集到调度器的 `pq: unexpected Parse response "(D) DataRow"`，但不能据此确定第一次 HTTP 500 的具体底层错误。`64045e2` 把任务轮询、checkpoint 和待调度查询放到连接池，写入继续绑定独占租约连接。真实 PostgreSQL 测试验证 8 个读者各读 1000 次、SQL 已进入锁等待后取消 HTTP 读取、随后仍返回正确记录和冲突 409、新写入仍成功；主动终止自有租约连接后，写入必须失败且不落新任务。`make test-services` 通过。`62106e5` 另为未知 HTTP 500 记录不含凭据、请求体或查询串的服务端诊断。

最终 `62106e5` 的 `make test-k8s-persistent` 两场景全部通过并清理集群。PostgreSQL 实测 `18.7`、MinIO `RELEASE.2026-04-17T00-00-00Z`；文件场景有 16 份版本、14 个引用对象，Skill 场景有 17 份版本、17 个引用对象，均检查没有 SQLite/本地对象回退。文件交付、两个新 Pod 追问、缺输入/缺工作区、活跃 MCP 取消后恢复、业务数据库失败重试、旧引用以及固定 Skill v1 在来源升级 v2 后不漂移均通过。两场景都完成 queued 追问时重建控制面和同 UUID 重投。具体 Pod、摘要、镜像身份见结构化证据。本次最终修改后没有再单独运行 SQLite 的原生文件/Skill 集群入口；对应业务路径由最终 PG/S3 专项覆盖，SQLite 故障与恢复由单元测试验证。

保留历史合入 `origin/main@ab6c72a` 后，集成源码 `ef8855ed22565b2ffe1f96feea2aae07c4d4faca` 的 `make fmt`、`make check`（含 race、真实 HTTP 头超时回归）和 `make demo` 再次通过；与 `62106e5` 相比没有生产 Go 差异。此处不宣称又重跑过集群矩阵。

发布边界故障矩阵仍限 SQLite/本地对象；不据独立存储服务测试推断 PostgreSQL/S3 同一故障组合已过。真实模型和剩余 V2 组合仍单独记录。

## V2 退出门槛

1. 在同一测试运行中以 PostgreSQL 存元数据、S3 兼容服务存不可变对象，跑完例 1–3。通过部署配置和后端日志确认未回退到 SQLite / 本地磁盘；记录镜像、版本、CLI 和模型类别。
2. 在接受追问后、调度前重启控制面：从持久队列恢复，只创建一个对应 Job，外部客户端使用原 UUID 能取回同一回合。对活跃回合重复提交，仍只有一个执行归属。
3. 在对象上传完成、数据库发布完成前注入故障，并测试一次发布后响应丢失。前者不能让 SaaS 看到不完整的成功记录；受支持的重试不得产生第二个可见成功版本，也不得替换旧 checkpoint。发布后响应丢失时客户端用原请求标识查询，拿到同一组不可变引用。
4. 重启后仍可下载旧版本，摘要与故障前相同，并检查另一个租户凭据无法读取。记录故障注入点、可观察证据与仍未覆盖的情形；不承诺多个控制面副本或生产 HA。

## 证据与文档如何保持一致

- 现有 [CI](../.github/workflows/check.yml) 在推送/PR 上运行 `make check`、`make demo` 和 `make test-services`；完整 kind 矩阵由手动工作流触发。不要将普通 PR 绿灯当作 kind 通过。
- **建议作为门禁：** 适配器、任务契约、样例输入或集群部署变更时，运行无密钥的 Codex 文件/故障 smoke；在发版前跑全 kind 矩阵；改变元数据或对象发布路径时跑 V2 后端组合。真实模型 smoke 用维护者手动触发，不对来自 fork 的 PR 发放密钥，不上传原始会话或凭据。
- 每条发布证明至少记录：提交 SHA、运行时间 UTC、可重放命令/CI run 链接、平台/CLI/模型类别与版本、后端版本、样本摘要、成功/失败/未跑、已知边界。README 只指向本矩阵与 Quick Start。发版以 tag 的证据为准；修改受影响代码后旧 run 不能代表当前提交，可保留为历史结果。示例命令和 fixture 由测试直接复用，不能只靠人工同步 Markdown。没有证据的组合标为未验证。
