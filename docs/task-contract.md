# 从 SaaS 提交任务到 Agent 交付

第一个任务是汇总一份 CSV。SaaS 提供地区和金额，选定的原生 Agent 运行一个文本回合，返回每个地区的合计。AgentMoor 将返回文本保存为指定的交付文件，并发布原生会话和工作区。SaaS 独立验算金额后，才可以把这份报告视为业务验收通过。

## 谁负责什么

| 边界 | 输入与输出 | 责任 |
| --- | --- | --- |
| SaaS → AgentMoor | HTTPS 任务资源，输入是指令、文本和期望文件名，输出是任务状态及固定版本的资产引用 | SaaS 决定用户权限和业务验收。AgentMoor 管租户作用域、任务状态、取消和交付记录。 |
| 部署方 → Worker | 选择适配器和镜像，分配执行凭据、独立工作目录与计算资源。Kubernetes 中运行一次性 Job | 当前每个任务对应一个由控制面预先分配的逻辑 runtime ID。Job 可先启动并等待提交，默认等待两分钟。首轮 Job 由部署方创建。启用续聊后，AgentMoor 按受信模板自动创建后续 Job。 |
| Worker → 原生 Agent | `task.Executor.ExecuteTask(ctx, environment, spec)` 返回文本、原生会话 ID 及可导出的历史目录 | 适配器封装原生调用、退出状态和会话定位。输入上限、权限、资产版本和发布流程由中间件统一处理。 |
| AgentMoor → 存储 | 现有的资产 API 和版本目录 | 发布交付物、工作区、原生会话三份版本，再一次性记录任务成功。读取方按固定版本下载并校验 SHA-256。 |

第一版执行能力名为 `text-turn.v1`。输入直接交给模型，Worker 同时把它保存为 `input.txt`。各适配器从原生 CLI 的文本结果生成报告。CSV 汇总规则留在任务指令里。业务测试负责判断报告是否正确。

## SaaS 的 HTTP 接口

作用域由控制面签发的短期凭据决定。`{id}` 等于已分配的 `scope.runtime`，同一租户、项目、适配器下每个新任务使用独立 ID。请求体不能指定租户、镜像、命令、可执行文件路径或云端凭据。

客户代码和第三方 Skill 按不可信代码处理。当前同容器直接执行路径只供受信开发及批准工具使用；可信 Worker 与不可信执行区的分离、实际沙箱与对抗验收按 [P2 执行边界](security-boundary.md)推进，不能用既有文件/Skill 契约通过代替。

| 操作 | 权限 | 语义 |
| --- | --- | --- |
| `PUT /v1/tasks/{id}` | `issue --submit` | 创建不可变任务，返回当前记录。相同 ID 和内容重试安全，修改已提交内容返回 409。尚未实现文本执行的适配器返回 422。 |
| `GET /v1/tasks/{id}` | 读 | 读取输入、状态和已成功发布的版本。其他作用域不可见。 |
| `POST /v1/tasks/{id}/cancel` | 提交 | 排队任务直接取消。运行中的任务先进入 `cancelling`，Worker 停止原生进程并确认后进入 `cancelled`。已完成任务返回 409。 |
| `GET <assets[].content_path>` | 读 | 获取指定版本的原始内容，用该引用的 `sha256` 验证。 |

```json
{
  "instruction": "按 region 汇总 amount_cents，以 JSON 返回每个地区的总额。",
  "input": "region,amount_cents\neast,105\nwest,31\neast,-7\n",
  "output": "report.json"
}
```

指令最多 8 KiB，文本最多 64 KiB，交付文件名最长 128 字节且只有一级。成功响应的 `assets` 包含 `artifact`、`workspace`、`session`。每条引用有 `kind`、`id`、`version`、`sha256` 和 `content_path`。消费者按 `kind` 和完整路径选择资产。

### 文件交付和多份产物

`file-turn.v1` 要求适配器生成磁盘上的新文件。Worker 验证临时结果是普通文件后，将它放到 `out/<output>`；最终文本不会代替这个文件。首轮和追问都以 `out/` 为交付目录，包含子目录中的全部普通文件，每轮最多 128 份。符号链接等特殊文件导致发布失败。`output` 指定的主文件必须存在，消费者按路径选择主文件，不能假定成功记录只有一份 Artifact。

每轮的 `assets` 是该轮 `out/` 的完整交付集，包含从上轮恢复后未修改的文件。修改同名文件会产生本轮独立引用；删除的文件不进入新回合集，旧引用仍可下载；重命名视为新路径，旧路径的历史版本仍可读。首轮使用 `out/<path>`，追问使用 `turns/<uuid>/out/<path>`。任务作用域确定 Tenant、Project、Runtime 和 Adapter；Artifact 元数据保存原生 SessionID、Adapter/CLI 版本、MediaType 和 SHA-256，资产键中的 UUID 标识来源回合。Skill 继续独立扫描与归档，不因发布主文件而跳过。

发布时，Worker 冻结并上传文件字节，再向 `finish` 提交本轮实际扫描所得的固定版本清单。平台验证每条引用属于本回合 `out/`、关联同一原生会话且摘要与存储一致，同时要求主文件、工作区和会话都已提交。清单不会自动吸收其他回合或未列出的部分发布文件。只有成功记录中的清单代表业务交付；失败、取消或失联回合不产生成功清单。

Codex 文件模式由部署端设置 `CODEX_FILE_TOOL=/absolute/path/to/tool`。此可执行文件必须在 Agent 工作区之外，提供 stdio MCP 服务器 `report` 的 `render` 工具；模型不能选择程序或输出路径。Worker 经环境变量 `REPORT_RESULT` 提供新的工作区相对结果路径，`REPORT_PREVIOUS` 提供上一份 `out/<output>`。工具成功后，适配器还要求原生 `mcp_tool_call` 完成事件；缺文件、工具失败或仅回复 `done` 都不会成为成功交付。

固定 Codex `0.171.0-alpha.13` 使用 `mcp_servers.report.tools.render.approval_mode="approve"` 仅授权这个部署端工具，同时保留 `read-only` 和禁用 shell、apps、hooks、多 Agent 的设置。[固定版本的工具审批实现](https://github.com/openai/codex/blob/rust-v0.171.0-alpha.13/codex-rs/codex-mcp/src/mcp/mod.rs#L96)。MCP 程序由部署方信任并约束自己的文件读写与外部动作；原生 shell 的沙箱不替这个程序提供隔离。集群测试使用只接受有限操作、不能接收任意路径或命令的合成报告工具，不把它注册为通用业务工具。

## SaaS 追问同一个会话

### 固定版本 Skill

无模型密钥的集群复验入口是 `make test-k8s-skills`。它经公开 API 发布完整的合成 Skill 包，三次换新 Pod 实际加载原生规则，并由规则驱动工具执行包内脚本、读取参考文件；发布 v2 后仍选择 v1；使用相同 CSV 和指令但未安装 Skill 的负例明确失败。

Skill 由受信部署配置的 `skill_pins` 选择，SaaS 的任务指令不能指定包版本或安装位置。每项包含原 Skill 的完整固定版本 `source` 引用（`kind=skill`、ID、version、SHA-256、content_path）和安全的 `name`，最多 16 项。当前任务安装只接受 `skill-tar.v1` 包；单文件 `skill-markdown.v1` 会明确失败。清单中的名称必须与包内 `SKILL.md` 的 frontmatter name 相同。

Worker 在首次 `claim` 用执行凭据提交选定 pins；平台验证本作用域的版本与摘要，将它们冻结到任务记录。成功 checkpoint 和之后的追问记录包含同一份 `skills`。同一 attempt 重试、替换 Worker 或后续配置不得改动或省略既定 pins，否则返回冲突。发布 Skill 新版本不会升级正在接续的会话；采用新版本需要部署方显式选择新的逻辑任务与对应作用域资产。

每次执行都将包的原始清单、脚本、参考文件和权限恢复到新的私有运行时 HOME，先验证字节再安装，不覆盖已有目录。Codex 适配器使用私有 `CODEX_HOME/skills/<name>` 并通过规范绝对路径的原生 Skill 提及加载正文，保持 read-only、禁 shell 的设置。运行时自己的工作区 Skill 和平台中原有 Skill 版本不会改写；任务记录的 pins 表示本轮平台托管安装，工作区原生 Skill 仍按原规则发现和归档。受信 MCP 工具收到 `AGENTMOOR_SKILLS` 的名称到规范清单路径映射，模型不能为工具选择脚本路径。

从首轮记录的 `assets` 取 `kind=session` 的 ID，或读取 `GET /v1/tasks/{id}/checkpoint` 的 `session_id`。SaaS 为每个新问题生成一个小写、带连字符的 UUID；网络重试沿用这个 UUID。以下请求只需原任务的提交凭据。

```http
PUT /v1/tasks/report-job/turns/780dc160-b331-43f7-afba-a60ef48257af
Content-Type: application/json
Authorization: Bearer <submit-token>

{"session_id":"<原生会话 ID>","message":"刚才哪一个地区的合计最高？"}
```

| 操作 | 返回和用途 |
| --- | --- |
| `PUT /v1/tasks/{id}/turns/{uuid}` | 首次提交返回已持久化的记录。原样重试返回同一回合；同一 UUID 改写问题或同一会话已有待执行回合时返回 409。内容上限为 8 KiB。 |
| `GET /v1/tasks/{id}/turns/{uuid}` | 轮询状态。成功时，下载 `assets` 中 `kind=artifact` 的 `content_path` 并校验 `sha256`，正文就是 Agent 的回答。先前回合的回答可以按各自的 URL 再取。 |
| `POST /v1/tasks/{id}/turns/{uuid}/cancel` | 沿用首轮的排队取消和执行中取消确认语义。后续追问从最近成功的回合继续。 |
| `GET /v1/tasks/{id}/checkpoint` | 读凭据可获取当前原生会话 ID 和最近成功发布的工作区、会话引用。新问题固定从接受时的这组版本出发。 |

调度器和 HTTP 服务共用持久目录。服务启动或重启后会检查已排队的追问，以作用域和请求 ID 生成稳定的 Job 名称。Worker 领取成功后恢复两份固定版本、调用原生续聊命令，再分别发布回答、工作区和会话。每轮写入独立的资产键，失败或取消的发布不会替换已成功的会话。排队和执行各有 15 分钟期限，失联的执行超过期限后显示 `interrupted`。新请求可以从最近成功的回合重试。

### 在 Kubernetes 打开自动调度

在运行 HTTP 服务的命名空间应用 [RBAC 清单](../deploy/kubernetes/turn-rbac.yaml)，给服务 Pod 指定 `serviceAccountName: agentmoor-dispatch`。将 JSON 配置挂载到服务 Pod，在 `agentmoor serve` 后加 `--turn-jobs /dispatch/profiles.json`。配置是数组，每项含完整 `scope` 和受信的 Kubernetes `pod_spec`。可复用首轮 Job 的 `spec.template.spec`，镜像、模型凭据、工作目录、网络和资源限制都在这个模板里配置。一个服务可处理多项已登记的作用域。

例如，首次交付成功后，可用已有的 Job 生成配置文件。生产部署可在创建首轮 Job 时直接使用同一份 Pod 模板生成这份配置。下面假定当前命名空间中已有名为 `produce` 的首轮 Job，且它的 Worker 配置使用示例中的四项作用域。

```sh
kubectl get job produce -o json | python3 -c '
import json, sys
job = json.load(sys.stdin)
scope = {"tenant":"saas","project":"reports","runtime":"report-job","adapter":"codex"}
print(json.dumps([{"scope":scope,"pod_spec":job["spec"]["template"]["spec"]}]))
' > profiles.json
```

模板中的 `worker` 容器挂载该作用域的执行凭据和 `/config/worker.json`；工作目录使用全新的临时卷，路径与首轮一致。调度器填入 `agentmoor run-task --config /config/worker.json --turn <uuid>`，并关闭 Worker Pod 的 Kubernetes 服务账号令牌自动挂载。跨天接续时，部署方需为同一作用域续签执行凭据并更新 Secret，新 Pod 从挂载的文件读取凭据。未配置续聊的作用域提交追问会返回 422。可运行的配置生成及部署示例见[集群验收](../tests/kubernetes/contract.py)。[Kubernetes Pod 身份](https://kubernetes.io/docs/tasks/run-application/access-api-from-pod/) · [Job 生命周期](https://kubernetes.io/docs/concepts/workloads/controllers/job/)

## Worker 协议

部署方使用 `issue --execute` 签发带读写资产权限的执行凭据，用 `agentmoor run-task --config FILE [--wait 2m]` 运行一次。配置沿用[运行手册](operations.md)，需要 `runtime_cli` 指向已验证的原生程序。`workspace` 是父目录中尚未创建的新路径。`sessions` 由执行器发现，产物目录由 Worker 设置为 `out`。跨 Pod 通信可以用 `serve --tls-cert PEM --tls-key PEM` 提供 HTTPS。

Worker 调用 `POST /v1/tasks/{id}/claim`，传入随机 `attempt`。并发争抢只有一个执行者获准，相同 attempt 可安全重发。它以相同 attempt 调用 `POST /v1/tasks/{id}/finish`，成功时提交 `session_id` 和 `artifacts` 固定版本引用数组，失败时提交 `error`。`finish` 请求体最多 1 MiB，清单最多 128 项，不允许重复路径。平台检查清单及工作区、会话版本后，才对外发布成功记录。旧 Worker 省略 `artifacts` 时，仍按原协议仅交付 `out/<output>`。成功应答丢失后重发 `finish` 返回已冻结的成功记录，不读取更新的资产 head。SaaS 客户端使用提交凭据，不持有执行权限。

任务的每份冻结资产发布最多尝试三次（100、200 毫秒退避），覆盖 Begin、Upload、Commit 的瞬态失败和成功后应答丢失。HTTP 408/429/500/502/503/504、EOF、连接重置/拒绝与网络超时可重试；任务上下文取消或到期优先停止。HTTP 自己较短的响应头超时与任务期限分别判断：任务仍有效时可以重试前者。重试不重新执行 Agent、不重扫工作目录，也不改变冻结 blob、元数据、base 或操作键。永久错误及耗尽仍报告失败，保留待发文件和 journal，未提交内容的游标不会前进；最近成功 checkpoint 与旧引用继续可读。普通 `sync` 仍在首次错误时退出，由下一次扫描使用 journal 恢复。[故障边界验证](verification-matrix.md#2026-11-26任务冻结发布重试与历史引用回收)

Worker 每秒检查一次取消请求。运行中失联且未确认结束时，15 分钟执行期限后查询显示 `interrupted`。这时重新执行业务需要显式分配新任务。取消与中断不会撤销已产生的外部动作。当前执行模式只在完成回合后归档，任务中途丢失 Pod 时，未发布的本地字节仍会丢失。

## 一条命令复验

从项目根目录运行 `make test-k8s`。需要 Go、C 编译器、Docker、kind、kubectl、Python 3 和 OpenSSL。脚本构建本地测试镜像，创建专属 kind 集群及 kubeconfig，并在退出时回收。每个镜像只装一种固定版本的 CLI，构建时校验其 SHA-512。默认依次运行 Codex、Gemini CLI、OpenCode，再回归 Claude Code。单独复验可用 `make test-k8s AGENT=gemini-cli`，其他适配器 ID 见下表。2026-11-26 复验使用 kind v0.33.0 和 Kubernetes v1.35.0，模型由集群内的确定性服务模拟。

1. 独立的 [SaaS 测试客户端](../tests/saas_contract.py) 只发 HTTP 请求，提交含随机金额、重复地区和负数的 CSV，检查重复提交和冲突。
2. Pod A 领取任务，选定版本的真实 CLI 发起模型请求。SaaS 轮询任务、下载报告、校验摘要，并用独立的预期金额验算。
3. 删除首轮 Pod 和临时卷。SaaS 连续追问两次，每轮完成后都删除该轮 Job。第一次回答需记得原始 CSV，第二次回答还需记得第一次追问中的随机码。检查每个请求 ID 只产生一个 Job，删除 Pod 后两份回答都能取回。
4. 对已到达模型端点的追问提交取消，确认最近成功的会话版本不变。同时对独立首轮任务提交取消请求，检查 `cancelling → cancelled`、执行 Job 在 25 秒内退出且成功资产列表为空。测试模型保持请求 60 秒，确保检验的是执行中取消。

## 部署方需要提供的运行时设置

Codex 文件工具专项使用相同的主机依赖，无需模型密钥：

```sh
make test-k8s-file-report
```

它依次运行生成、修订、审计三个不同的 Pod，保留原生会话 ID；每轮通过公开 HTTP 下载两份文件并核对摘要。金额由独立 SaaS 客户端计算。后续注入业务数据库写入失败、同会话所需恢复文件丢失和活跃 MCP 工具取消，再用新 Pod 从最近成功版本继续。恢复丢失负例是在合成工具开始前移除所需文件，并非关闭生产 Worker 的恢复流程。测试在退出时删除专属集群、镜像标签和 kubeconfig；失败时先检查输出中的 Worker/模型日志及 `docker info`。成功输出 `FILE_REPORT_RESULT`，包含不同 Pod UID、会话 ID 摘要、固定 Artifact 引用与文件 SHA-256。冷启动耗时取决于下载与构建，本项目尚未给出启动时间承诺。

首轮和追问分别使用上面的请求格式，四种适配器共享这两种格式。部署方把模型凭据注入 Worker，并在接手续聊的 Pod 提供相同模型配置。归档保存原生会话和交付数据。示例里的 `fixture` 是集群内测试服务使用的假密钥。

| 适配器和已测版本 | Worker 模型设置 | 替换 Pod 如何继续 |
| --- | --- | --- |
| `codex` · `0.171.0-alpha.13` | `OPENAI_API_KEY`，可选 `CODEX_MODEL` 和 Responses API 的 `OPENAI_BASE_URL` | 恢复 rollout 到新 `CODEX_HOME/sessions`，用 `codex exec … resume <id>`。自定义端点在两端使用同一个提供方 ID `operator`。 |
| `gemini-cli` · `0.70.0` | `GEMINI_API_KEY`，可选 `GEMINI_MODEL` 和 `GOOGLE_GEMINI_BASE_URL` | Worker 从 Gemini 自己的项目登记表定位 chats。在新 Pod 沿用工作区路径，把会话恢复到该项目的 chats，再用 `--resume <id>`。 |
| `opencode` · `1.19.6` | 本轮验证 OpenAI 提供方，使用 `OPENAI_API_KEY`，可选 `OPENAI_BASE_URL` 和 `OPENCODE_MODEL`（默认 `openai/gpt-4.1`） | 交付时从任务专用数据库导出单个会话。新 Pod 用 `restore-native --id … --version … --to …` 导入新数据目录，将 `XDG_DATA_HOME` 指向该目录，用 `opencode run --session <id>` 继续。 |
| `claude-code` · `2.1.330` | `ANTHROPIC_API_KEY`，可选 `ANTHROPIC_MODEL` 和 `ANTHROPIC_BASE_URL` | 恢复对应项目的 transcript，把已恢复的 transcript 路径传给 `claude --resume`，并校验返回的原生会话 ID。 |

SaaS 发起每次追问，替换 Pod 使用执行凭据交回结果。运行中丢失 Pod 可以回到最近一次成功交付的回合。

## 增加下一个 Agent

选定具体 CLI 版本，实现 [task.Executor](../internal/task/task.go)，在适配器描述中声明 `text-turn.v1`。复用该运行时现有的会话导出与工作区发布逻辑。实现 `task.Continuation` 并声明 `continue-turn.v1`。在适配器里恢复提供的固定版本并执行原生续聊，为集群验收提供该 CLI 的镜像和模型设置。用同一个 SaaS 测试客户端验证交付、重提、取消与换 Pod 恢复。
