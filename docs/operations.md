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

此入口复用一个专属 kind 集群和一份固定 Codex 镜像，依次用 PostgreSQL 元数据、MinIO S3 对象存储跑文件交付和固定版本 Skill 工作流。每个场景使用独立命名空间、数据库/对象卷及测试桶；服务镜像与 `tests/services.compose.yml` 一致。无需模型密钥，首次仍需下载依赖。测试使用真实原生 CLI 和脚本 Responses 模型。

基础设施夹具在正常 HTTP 接受追问前，用命名空间 Job 配额暂缓调度。控制面 Pod 更换后，SaaS 按原 UUID 重投，再恢复调度；验收要求仍只有一个 Job、原 checkpoint 和固定版本下载不变。转发器重连原本的回环端口，业务数据库中的服务身份不因 Pod 重启而改变。夹具另外核对 PostgreSQL 表、S3 版本对象，以及平台目录中不存在 SQLite / 本地对象，避免误把默认后端的通过当作组合后端通过。

退出时自动删除专属集群、应用镜像标签和临时 kubeconfig；服务镜像保留在主机缓存。Docker 使用 containerd 镜像存储时，夹具只导出本机架构的缓存镜像，避免 kind 导入多架构索引时追索未下载的架构。本入口未组合 15 分钟 Worker Pod 突然丢失用例，也不代表真实模型或生产高可用验收。

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

- 上传中断或服务重启后，使用同一 `state_dir` 再运行 `sync`。Worker 会重试已冻结的待发内容，已提交的操作可安全重放。
- 遇到 `conflict`，用 `status` 找到待发资产，用 `assets` 查看平台版本，必要时先恢复到新路径比较。如果决定发布本地冻结版本，在 `resolve --config "$cfg" --expected-version N` 中填写亲自检查过的平台版本，再运行 `sync`。此期间平台版本再次变化会重新报冲突。若要沿用平台内容，将该版本恢复到新的工作区，并用新 Worker 状态目录接管。
- 持有回收权限的运维进程可按需或定时运行 `gc --config "$cfg"`。平台只清理此作用域内超过 24 小时、没有已发布版本引用且没有在途写入的对象。

## 单独复验真实 CLI

在 macOS 或 Linux 安装要测的原生 CLI 及它依赖的运行环境，再运行。

```sh
make build
python3 tests/native_roundtrip.py codex /absolute/path/to/codex
```

第一个参数也可以是 `claude-code`、`gemini-cli` 或 `opencode`。本次使用的精确版本见[验收矩阵](mvp.md)。测试给 CLI 创建独立临时主目录和合成凭据，并让它向本机模拟模型发出两轮请求。
