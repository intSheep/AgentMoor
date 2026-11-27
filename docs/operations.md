# 运行手册

第一次体验可直接运行 `make demo`。下面用一个 Codex 实例说明怎样接入自己的平台。示例路径由部署者按实际挂载替换。

## 1. 启动平台

先用 `make build` 构建。下面的目录由平台服务账号持有并独立于 Agent 的文件系统。第一次运行 `init` 会创建签名密钥，后续直接使用 `serve`。

```sh
./bin/agentmoor init --data /srv/agentmoor/platform
./bin/agentmoor serve --data /srv/agentmoor/platform --listen 127.0.0.1:7401
```

默认在平台目录保存 SQLite 元数据和文件对象。远程访问时，在 `serve` 前配置 HTTPS 反向代理。每个元数据库运行一个平台服务进程。更换数据后端时，向该平台进程提供以下环境配置。

| 环境变量 | 用途 |
| --- | --- |
| `AGENTMOOR_POSTGRES_DSN` | 现有 PostgreSQL 数据库的连接串。服务会创建表和索引。 |
| `AGENTMOOR_S3_ENDPOINT`、`AGENTMOOR_S3_BUCKET` | S3 兼容服务的 `host[:port]` 和已创建的桶。默认使用 HTTPS。 |
| `AWS_ACCESS_KEY_ID`、`AWS_SECRET_ACCESS_KEY`、`AWS_SESSION_TOKEN` | 对象存储凭据。最后一项用于临时凭据。 |
| `AGENTMOOR_S3_HTTP=1` | 供本地 HTTP 对象存储测试使用。 |

服务集成测试自建临时 PostgreSQL 与 S3 兼容容器，验证写入、读取、回收，退出后清理。运行 `make test-services` 需要 Docker Compose。[测试配置](../tests/services.compose.yml)

### PostgreSQL / S3 下的集群工作流

```sh
make test-k8s-persistent
```

此入口另要求 Docker CLI 支持 `docker image save --platform`，服务端 API 至少 **1.48**；设置了 `DOCKER_API_VERSION` 时也不能低于 1.48。创建 kind 集群前会检查，不影响普通集群入口。[Docker 官方参数说明](https://docs.docker.com/reference/cli/docker/image/save/#save-a-specific-platform---platform)。

此入口复用一个专属 kind 集群和一份固定 Codex 镜像，依次用 PostgreSQL 元数据、MinIO S3 对象存储跑文件交付和固定版本 Skill 工作流。每个场景使用独立命名空间、数据库/对象卷及测试桶；服务镜像与 `tests/services.compose.yml` 一致。无需模型密钥，首次仍需下载依赖。测试使用真实原生 CLI 和脚本 Responses 模型。

基础设施夹具在正常 HTTP 接受追问前，用命名空间 Job 配额暂缓调度。控制面 Pod 更换后，SaaS 按原 UUID 重投，再恢复调度；验收要求仍只有一个 Job、原 checkpoint 和固定版本下载不变。转发器重连原本的回环端口，业务数据库中的服务身份不因 Pod 重启而改变。夹具另外核对 PostgreSQL 表、S3 版本对象，以及平台目录中不存在 SQLite / 本地对象，避免误把默认后端的通过当作组合后端通过。

退出时自动删除专属集群、应用镜像标签和临时 kubeconfig；服务镜像保留在主机缓存。Docker 使用 containerd 镜像存储时，夹具只导出本机架构的缓存镜像，避免 kind 导入多架构索引时追索未下载的架构。本入口未组合 15 分钟 Worker Pod 突然丢失用例，也不代表真实模型或生产高可用验收。

### PostgreSQL/S3 发布故障与真实 Worker 丢失

```sh
make test-k8s-persistent-faults
# 同一集群和 CLI 镜像依次运行文件、固定 Skill、故障/失联三场景：
make test-k8s-v2
```

两条命令各自独立运行，按需选择。前者在同一命名空间、数据库与 S3 桶中组合文件块发布前后故障、queued 追问控制面重启，以及 SIGKILL 后等待生产 900 秒 deadline。后者先跑文件和固定 Skill，再跑该组合；三个场景使用隔离命名空间和各自 PG/S3，同时验证 API 重启后合法外租户凭据无法读取旧固定版本。普通短入口保持原行为。双 TLS 故障代理、固定内容和单次原生执行的检查，以及各次实际结果和边界，见[组合验收说明](persistent-faults-acceptance.md)。

## 2. 给 Worker 分配身份和目录

在平台侧签发短期凭据，并通过挂载文件或密钥管理系统把它交给对应 Worker。以下命令的凭据文件父目录应已存在，且仅平台和获授权的 Worker 可访问。

```sh
umask 077
./bin/agentmoor issue --data /srv/agentmoor/platform \
  --tenant team --project demo --runtime primary --adapter codex --write --ttl 1h \
  > /run/agentmoor/primary.token
export AGENTMOOR_TOKEN_FILE=/run/agentmoor/primary.token
```

省略 `--write` 可签发只读凭据，`--gc` 单独授予回收权限。凭据默认 1 小时，最长 24 小时。长期运行的 Worker 每次请求都会重读凭据文件，控制面可在到期前原子替换文件。

HTTP 按以下能力分别授权；同一凭据可以组合多种能力。

| 能力 | 允许的操作 | CLI 签发方式 |
| --- | --- | --- |
| Read | 列举资产、head、操作状态，下载内容，读取任务、追问和 checkpoint | 默认包含 |
| Write | 开始资产变更、上传完整内容或清单/文件块、提交资产版本 | `--write` |
| Submit | 提交任务或追问、请求取消 | `--submit` |
| Execute | 领取任务或追问、报告完成 | `--execute`；CLI 同时授予 Write，供 Worker 发布结果 |
| Collect | 回收本作用域的过期无引用对象 | `--gc` |

`GET /v1/context` 用于查看当前有效凭据自身的授权。Submit 不授予 Execute 或 Write，Write 不授予 Submit 或 Execute，Collect 也不授予 Write。SaaS 使用 `--submit`，Worker 使用 `--execute`；回收工具单独使用 `--gc`。所有操作都受 Tenant、Project、Runtime、Adapter 四项作用域限制。

[HTTP 授权矩阵](../internal/platform/authorization_matrix_test.go)逐条列出当前 22 条公开路由：有效但缺少能力的凭据返回 403，无效身份返回 401；四个作用域维度各自变化时，另一个合法凭据访问已有固定引用返回 404，原归属数据保持不变。它包含各接口的有效正例和同摘要文件块的隔离验证；新增路由时应同步补入矩阵。矩阵本身不代表凭据治理或沙箱验收；可选的密钥轮换与单凭据撤销见[策略运行手册](authentication-policy.md)，客户代码/第三方 Skill 见[不可信执行边界](security-boundary.md)。

将下面配置保存在受信目录，例如 `/etc/agentmoor/primary.json`。`workspace`、`sessions` 和 `state_dir` 使用绝对路径。前两者分配给一个逻辑运行时，`state_dir` 由 Worker 独占，可跨进程重启保留。平台密钥、Worker 凭据和配置放在工作区之外，工作区快照会收录整个目录树，包括点文件。

```json
{
  "endpoint": "http://127.0.0.1:7401",
  "scope": {
    "tenant": "team",
    "project": "demo",
    "runtime": "primary",
    "adapter": "codex"
  },
  "workspace": "/srv/agents/primary/work",
  "sessions": "/srv/agents/primary/codex-home/sessions",
  "artifacts": "out",
  "state_dir": "/srv/agentmoor/workers/primary"
}
```

`artifacts` 是工作区内已存在的交付目录。省略它可只捕获工作区、Skill 及已配置的会话。`artifact_session` 可由控制面明确提供关联会话 ID。`managed_skills` 是平台管理的 Skill 清单路径数组，例如 `[".agents/skills/greet/SKILL.md"]`，路径相对于工作区。

`runtime_cli` 可指定工作区外受信可执行文件的绝对路径。提供它时，平台会记录实测版本。`runtime_version` 可要求该程序的 `--version` 输出精确匹配，`adapter_version` 可固定适配器实现版本。使用 `agentmoor inspect <adapter> <workspace> <绝对可执行文件>` 查看两者。

### 任务执行的信任策略

`run-task` 的 `execution_policy` 由受信 Worker 配置指定，任务文本不能覆盖它：

| 配置值 | 当前行为 |
| --- | --- |
| 未填写或 `trusted-local.v1` | 兼容已有受信开发/批准工具的本地子进程流程，尚无代码隔离保证。 |
| `untrusted-isolated.v1` | 要求 `isolated_provider: "kubernetes-gvisor.v1"` 和已配置的 HTTPS 控制面分配；Worker 不启动宿主原生 CLI。缺提供方、错版本或不可用分配拒绝，不能回退。 |
| 其他值 | 配置校验失败。 |

客户代码和第三方 Skill 必须按不可信执行处理，不能改成 `trusted-local.v1`。按[隔离提供方部署契约](sandbox-provider.md)配置双 TLS listener、公开 CA、最小 RBAC、NetworkPolicy、固定镜像和实际节点边界；Worker 省略 `runtime_cli`、原生会话与宿主导入配置。`19d0d5b` 已通过真实 Codex 文件/Skill、取消和 guest 丢失验收，隔离文本回合另验。缺配置的拒绝检查仍保留，不会改写旧成功 checkpoint 或静默执行。

这个策略覆盖任务首轮和追问，不覆盖普通 `sync` 或 `restore-native` 等受信维护入口。隔离任务的原生导入/导出发生在 guest；独立维护命令仍要求受信输入。[执行边界](security-boundary.md)

`make test-k8s-gvisor-feasibility` 仅检验运行时和缺 handler 拒绝。完整应用探针使用 `python3 tests/kubernetes/sandbox_provider_smoke.py --scenario both`，独立合成配额用 `python3 tests/kubernetes/sandbox_provider_quota.py`；两者支持 `--bundle`、`--calico-manifest`、`--containerd-cache` 和 `--output`，缓存仍按固定 SHA 验证。它们各建专属临时 kind 节点并在退出时清理，需 OrbStack/Docker、kind、kubectl 和联网构建；不在已有生产节点上运行安装步骤。[固定源码、实际结果与限制](sandbox-provider.md#2026-11-27-实际验收)

### 可选：固定任务 Skill

任务执行可通过受信 Worker 配置的 `skill_pins` 固定 Skill。以下示例中的引用应从该作用域的资产 API 取得，替换为真实 ID、固定版本、SHA-256 及对应 content_path；不能写 `version: 0` 或指向可变 head。

```json
{
  "skill_pins": [{
    "name": "report-policy",
    "source": {
      "kind": "skill",
      "id": "catalog/skills/report-policy",
      "version": 1,
      "sha256": "<资产 API 返回的 64 位小写 SHA-256>",
      "content_path": "/v1/content?id=catalog%2Fskills%2Freport-policy&kind=skill&version=1"
    }
  }]
}
```

此片段合并进原 Worker 配置，不是独立配置。首轮及追问 Job 必须携带相同的 pins。仅 Codex 已实现任务 Skill 安装：每轮下载并校验 `skill-tar.v1`，恢复到私有 HOME，使用原生 Skill 加载正文。来源包的 `SKILL.md` 名称必须与 `name` 相同，安装保留附属脚本、参考文件及权限。更新来源 Skill 的 head 不会更新原任务；配置漂移会在 claim 返回 409。已有目录、摘要不符、错误包格式或不支持此能力的适配器都会失败，不会覆盖旧内容。[任务中的固定版本规则](task-contract.md#固定版本-skill)

## 3. 同步并查看结果

```sh
cfg=/etc/agentmoor/primary.json
./bin/agentmoor sync --config "$cfg"
./bin/agentmoor assets --config "$cfg"
./bin/agentmoor status --config "$cfg"
./bin/agentmoor watch --config "$cfg" --interval 1m
```

`assets` 返回当前作用域的所有版本，包含标识、来源格式、SHA-256 和鉴权后的读取路径。`status` 返回 Worker 最后成功时间、待发项、错误和平台记录。平台连接中断时仍输出本地状态并返回非零退出码。

控制面可在运行时完成写盘后直接调用 `sync`，收到成功退出码即完成本次对账。`watch` 会合并文件事件，并定期重扫。只有定时能力的环境可加 `--periodic-only`。OpenCode 原生数据库的外部更新会在下一次定时扫描或主动 `sync` 时捕获。

## 4. 在新实例恢复

恢复目标的父目录须存在，目标路径须尚未创建。Worker 先在旁边校验、展开，再一次性发布。以下例子恢复最新工作区和一个历史交付文件。

```sh
./bin/agentmoor restore --config "$cfg" --kind workspace --id root --to /srv/replacement/work
./bin/agentmoor restore --config "$cfg" --kind artifact --id out/report.pdf \
  --version 1 --to /srv/replacement/report-v1.pdf
```

Skill 的 ID 是 `assets` 返回的原始清单相对路径。目录型 Skill 使用 `restore --kind skill` 恢复到目标 Agent 的新 Skill 目录。单文件 Skill 的目标是一个新文件。

要让原生 CLI 继续会话，在新实例保持相同的项目绝对路径、逻辑运行时身份和兼容 CLI 版本。将新 Agent 配置为使用它自己的专属用户目录，先恢复工作区，再按下表恢复会话。

| 适配器 | 捕获配置与恢复位置 |
| --- | --- |
| `claude-code` | `sessions` 指向 `CLAUDE_CONFIG_DIR/projects/<项目编码>`。`restore-sessions --to` 指向新实例的同一项目历史目录。 |
| `codex` | `sessions` 指向专属 `CODEX_HOME/sessions`。批量恢复到新的 `sessions`，保留每条 rollout 的相对路径。 |
| `gemini-cli` | `sessions` 指向该项目实际使用的 `chats` 目录。批量恢复到新实例的对应项目目录。 |
| `opencode` | 实时接入用 `"native_sessions": true` 和 `runtime_cli`，Worker 使用专属的 `HOME` 与 `XDG_DATA_HOME`。恢复工作区后，使用 `restore-native --id <ID> --to <新的 XDG_DATA_HOME>` 原生导入一条会话。也可用 `sessions` 指向已有 JSON 导出目录，此模式的普通 `restore` 输出导出文件。 |

```sh
./bin/agentmoor restore-sessions --config "$cfg" --to /srv/agents/primary/codex-home/sessions
```

批量恢复固定本次读取到的每条会话版本。只需某条历史会话时，使用 `restore --kind session --id <ID> --version <N> --to <新路径>`。OpenCode 的 `native_sessions` 和 `sessions` 两种配置互斥。

## 5. 处理失败或冲突

四类资产的创建、更新、本地删除和恢复规则见[资产生命周期](asset-lifecycle.md)。当前没有远端资产 DELETE/tombstone；GC 不会删除仍有历史版本引用的对象。

- 上传中断或服务重启后，使用同一 `state_dir` 再运行 `sync`。Worker 会重试已冻结的待发内容，已提交的操作可安全重放。
- `run-task` 的资产发布在同一回合内对明确的瞬态 HTTP/网络错误最多尝试三次，间隔为 100、200 毫秒；始终复用原冻结文件、元数据、base 和操作键，不重新执行 Agent 或扫描目录。任务取消或期限到达会中断等待及请求。冲突、缺失、鉴权、JSON 格式、TLS 校验及本地文件错误不自动重试。重试耗尽时任务失败，`state_dir/publish` 保留 pending 和冻结文件，未提交资产的游标不前进；不要把重新执行旧请求当作恢复手段。追问可用新 UUID 从最近成功 checkpoint 继续，首轮失败则按任务契约分配新的逻辑任务。
- 遇到 `conflict`，用 `status` 找到待发资产，用 `assets` 查看平台版本，必要时先恢复到新路径比较。如果决定发布本地冻结版本，在 `resolve --config "$cfg" --expected-version N` 中填写亲自检查过的平台版本，再运行 `sync`。此期间平台版本再次变化会重新报冲突。若要沿用平台内容，将该版本恢复到新的工作区，并用新 Worker 状态目录接管。
- 持有回收权限的运维进程可按需或定时运行 `gc --config "$cfg"`。平台只清理此作用域内超过 24 小时、没有已发布版本引用且没有在途写入的对象。

## 单独复验真实 CLI

在 macOS 或 Linux 安装要测的原生 CLI 及它依赖的运行环境，再运行。

```sh
make build
python3 tests/native_roundtrip.py codex /absolute/path/to/codex
```

第一个参数也可以是 `claude-code`、`gemini-cli` 或 `opencode`。本次使用的精确版本见[验收矩阵](mvp.md)。测试给 CLI 创建独立临时主目录和合成凭据，并让它向本机模拟模型发出两轮请求。

## 可选：开启工作区增量上传

先升级到实现该协议的平台和 Worker，再在原 Worker 配置中加入以下字段；默认省略时仍上传完整工作区 tar。

```json
{
  "workspace_transfer": "tar-parts.v1"
}
```

这个片段需要合并进已有配置。首轮 Worker 与追问 Job 如需都使用增量传输，须更新各自部署配置。之后正常运行 `sync` 或 `watch`：Worker 上传缺少的文件 body 块和 gzip 清单，公开工作区内容仍是原来的完整 `workspace-tar.v1`，旧客户端的下载和恢复命令不变。

关闭时省略该字段或设为空字符串。开关只作用于新建操作；已有 pending 会先按它记录的旧传输方式、操作键和冻结字节恢复，再处理新内容。保留同一 `state_dir`，不要删除 journal 来切换。旧平台不支持该模式，大小上限或协议错误会明确失败，不会自动退回整包。

文件改名、权限和删除会产生新的工作区版本，相同 body 可复用，历史版本继续保留。恢复仍必须使用新目标路径。增量上传仍有完整本地扫描、打包，以及平台提交时的完整后端读取成本，不能据上传量减少承诺耗时变短。限制、GC、可复制的测量命令和 `make test-k8s-workspace-parts` 入口见[工作区增量上传](workspace-incremental.md)。
