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
| Codex 文件任务取消、失联及重试 | 例 3 | 活跃 MCP 工具取消、恢复后新回合、业务数据库失败重试已通过；工具运行中 Pod 突然丢失仍未验收。 |
| PostgreSQL + 对象存储下的完整 Kubernetes 契约 | V2 退出门槛 | 计划。不据后端测试推断跨 Pod 端到端已过。 |

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

同会话负例在合成工具开始前移除恢复出的 CSV/state 后执行修订，明确失败且 checkpoint 不变。这是在恢复后模拟所需文件缺失，不是关闭生产 Worker 恢复的实测。另在真实 MCP 进程写出启动标志并进入阻塞后取消，观察到 cancelling/cancelled，25 秒内 Job 退出，没有成功资产；新 UUID 和 Pod `9c82006c-4aca-4e77-9e2d-c86beffcca10` 从最近成功版本继续审计，未归档草稿没有混入。直接杀掉活跃 Worker Pod 的 interrupted/恢复场景仍未跑。

同一代码的 `make fmt`、`make check`（含 race）、`make demo`、`make test-services` 与四 CLI `make test-k8s` 全部通过。后者仍是四个 CLI 的文本交付、连续新 Pod 追问及运行中取消；本节文件能力仅声明 Codex 已测。真实模型 smoke、固定版本 Skill 注入和 PostgreSQL/S3 下完整跨 Pod 流程均未验收，不据本次通过推断支持。

### 2026-11-26：固定 Skill 版本与原生加载

实现提交 `6ae64d2`，相同输入负对照提交 `e5c0fc5`；Codex Adapter `0.5.0`。`make test-k8s-skills` 在固定 Linux Codex `0.171.0-alpha.13`、kind 0.33.0 / Kubernetes v1.35.0、脚本 Responses 模型上通过。[结构化证据](evidence/codex-pinned-skills-2026-11-26.json)。

受信测试配置经公开 HTTP 发布完整 `skill-tar.v1` 并选择 v1；SaaS 指令只含报告任务和 CSV，不带 Skill 规则。每个新 Pod 都将原清单、脚本与参考文件恢复到私有 `CODEX_HOME/skills/pinned-report`。Codex 通过当前私有目录的规范路径原生加载正文；脚本模型只有在当前路径对应的原生 `<skill>` 片段中找到独有规则，才能请求正确的工具参数。MCP 工具实际运行包内 `scripts/check.py`，脚本读取 `references/rule.json`，并发布三个成员摘要的 `out/skill-proof.json`。最终文本仍仅为 `done`。没有把正文由 Worker 人工拼成模型指令。

三个成功 Pod UID 分别为 `1042997f-9218-466d-a97a-7e5a832db74d`、`852a4711-fe1e-4990-8ae1-aaffb4437eeb`、`d60bbcb2-f507-439d-a853-1221d63fd8eb`；每轮后都删除前一 Pod 与 emptyDir。原生会话 ID 摘要保持 `c4183ea8b0dae6b3ef617f41952a34dcad92f0348f2467b8bea2678b06e16732`。任务、Parent 和成功 checkpoint 均携带同一 v1 pin，其包 SHA-256 为 `cd4c51c1efc04319fa32c2480cf35593f1ab7670bff857e010b747c4d89ce26a`。首轮后发布 v2，后续两轮仍遵循 v1，原包与旧报告字节不变。相同 CSV 和指令、未安装 Skill 的独立任务明确 failed，成功资产为空。

平台测试另验证摘要、版本、权限、重复名称、过多 pins、同 attempt 幂等、配置漂移拒绝、取消后继承及控制面重启保留；Worker 测试验证完整包恢复、权限保留、已有目标冲突、私有路径边界和来源/原生 Skill 不被改写。`make fmt/check/demo/test-services`、四 CLI 的 `make test-k8s` 文本矩阵与本机 Codex `.13.2` 的 native roundtrip 通过；同一新版适配器的普通文件专项（含活跃工具取消、数据库重试和失败后恢复）也通过。真实模型、其他 CLI 的任务 Skill 安装，以及 PostgreSQL/S3 完整集群流程仍未验收。

### 例 3：取消、失联与重试

从已完成的首轮出发。第一次追问先进入一个可观测的长工具步骤，等待工具启动后发取消。第二个独立追问在等到工具启动后直接删除 Worker Pod，不给正常收尾机会。待终态可见后，用新 UUID 从最近成功的引用继续；先前请求 UUID 仅用于原请求重试，不作为重跑手段。

**通过：** 取消经过可观察的 `cancelling`，终态 `cancelled`；进程在用例声明的超时内退出。Pod 丢失后在文档规定的期限内进入 `interrupted`，不能被误报为成功。两次失败回合均无成功交付引用，之前的工作区/会话引用和版本摘要不变，新回合在新 Pod 从最近成功状态恢复。失败公开记录可识别，说明未发布本地内容会丢失，取消也不撤销外部副作用。

## V2 退出门槛

1. 在同一测试运行中以 PostgreSQL 存元数据、S3 兼容服务存不可变对象，跑完例 1–3。通过部署配置和后端日志确认未回退到 SQLite / 本地磁盘；记录镜像、版本、CLI 和模型类别。
2. 在接受追问后、调度前重启控制面：从持久队列恢复，只创建一个对应 Job，外部客户端使用原 UUID 能取回同一回合。对活跃回合重复提交，仍只有一个执行归属。
3. 在对象上传完成、数据库发布完成前注入故障，并测试一次发布后响应丢失。前者不能让 SaaS 看到不完整的成功记录；受支持的重试不得产生第二个可见成功版本，也不得替换旧 checkpoint。发布后响应丢失时客户端用原请求标识查询，拿到同一组不可变引用。
4. 重启后仍可下载旧版本，摘要与故障前相同，并检查另一个租户凭据无法读取。记录故障注入点、可观察证据与仍未覆盖的情形；不承诺多个控制面副本或生产 HA。

## 证据与文档如何保持一致

- 现有 [CI](../.github/workflows/check.yml) 在推送/PR 上运行 `make check`、`make demo` 和 `make test-services`；完整 kind 矩阵由手动工作流触发。不要将普通 PR 绿灯当作 kind 通过。
- **建议作为门禁：** 适配器、任务契约、样例输入或集群部署变更时，运行无密钥的 Codex 文件/故障 smoke；在发版前跑全 kind 矩阵；改变元数据或对象发布路径时跑 V2 后端组合。真实模型 smoke 用维护者手动触发，不对来自 fork 的 PR 发放密钥，不上传原始会话或凭据。
- 每条发布证明至少记录：提交 SHA、运行时间 UTC、可重放命令/CI run 链接、平台/CLI/模型类别与版本、后端版本、样本摘要、成功/失败/未跑、已知边界。README 只指向本矩阵与 Quick Start。发版以 tag 的证据为准；修改受影响代码后旧 run 不能代表当前提交，可保留为历史结果。示例命令和 fixture 由测试直接复用，不能只靠人工同步 Markdown。没有证据的组合标为未验证。
