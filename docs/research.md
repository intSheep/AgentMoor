# 通用 Agent 资产适配调研

调研与本机验证日期为 2026-11-22。首批覆盖 Claude Code、Codex CLI、Gemini CLI 和 OpenCode。前两项参考实现帮助确定了目录归属和适配器边界，会话恢复行为另用四个真实 CLI 验证。[版本与结果](mvp.md)

## Magpie 和 CC Switch 的哪些做法适合这里

| 项目 | 观察 | AgentMoor 的取舍 |
| --- | --- | --- |
| Magpie | 按目标 Agent 描述资产目录，多个目标可能指向同一个共享目录。[目标定义](https://github.com/yetone/magpie/blob/main/internal/library/targets.go) · [同步实现](https://github.com/yetone/magpie/blob/main/internal/library/sync.go) | 每个适配器描述自己的标记和 Skill 来源。公共代码处理目录遍历。一个项目可同时匹配多个 Agent。 |
| CC Switch | Skill 服务管理统一来源和各应用的安装目录，并记录分发目标。[Skill 服务](https://github.com/farion1231/cc-switch/blob/main/src-tauri/src/services/skill.rs) | 区分平台管理与运行时自带的 Skill。保留来源路径和原始包，按内容摘要发布，同名候选各自保留。 |

本项目还有一个额外要求，即源实例销毁后仍能继续原会话。因此适配器保留运行时原始内容，Worker 通过受权的平台 API 发布不可变版本。登记资产、下载字节和实际续聊有独立的验证步骤。

## 各运行时有什么差异

| 运行时 | 项目 Skill 发现范围 | 会话接入 |
| --- | --- | --- |
| Claude Code | `.claude/skills/<目录>/SKILL.md`。 | SDK 提供会话存储扩展点。v0.1 直接兼容项目的落盘 Transcript，保留同 ID 关联目录。恢复到相同项目路径后，已测版本可用 `--resume` 继续主会话。[Skills](https://code.claude.com/docs/en/skills) · [会话存储](https://code.claude.com/docs/en/agent-sdk/session-storage) |
| Codex CLI | 指定项目根下的 `.agents/skills/<目录>/SKILL.md`。上游还支持沿目录向仓库根查找。 | App Server 支持按线程读取与恢复。此次通过独立运行时的专属 rollout 目录保存原始文件，并使用真实 `exec resume` 验证读回。[Skills](https://developers.openai.com/codex/skills) · [App Server](https://developers.openai.com/codex/app-server) |
| Gemini CLI | `.agents/skills/` 和 `.gemini/skills/` 的根清单及直接子目录清单。 | 记录项目 chats 的原始 JSON 或 JSONL 和可识别的项目标识。已测 CLI 使用原会话 ID 续聊。此适配器针对 Gemini CLI 的历史工作区。[Skills](https://geminicli.com/docs/cli/skills/) · [加载实现](https://github.com/google-gemini/gemini-cli/blob/main/packages/core/src/skills/skillLoader.ts) · [会话管理](https://geminicli.com/docs/cli/session-management/) |
| OpenCode | `.opencode/skills/`、`.claude/skills/`、`.agents/skills/`。支持来源根的 Markdown 单文件，以及最多六层分组的 `SKILL.md`。 | 针对实测 1.19.6 调用 `session list`、`export`、`import`，导出时核对会话所在的工作目录。CLI 命令随版本变化，原生模式会检查版本。[Skills](https://opencode.ai/docs/skills/) · [CLI 文档](https://opencode.ai/docs/cli/commands/) · [实测调用](../internal/adapters/opencode/native.go) |

## 工程选择

- 一个 Go module、一个 CLI，核心实现放在 `internal`。控制面通过 HTTP 与 Worker 交互。每个适配器只描述它能提供的操作和证据。[Go 模块布局](https://go.dev/doc/modules/layout)
- 从明确分配的目录句柄读取，保留原始文件。Skill 的已知字段可索引，其余自定义字段仍保存在清单内。[Go 目录访问边界](https://go.dev/blog/osroot) · [Skill 格式](https://agentskills.io/specification)
- Watcher 能降低常见变更的等待时间，周期扫描用来补齐遗漏事件。Worker 逐目录安装监听，每次同步校验落盘内容。[fsnotify](https://github.com/fsnotify/fsnotify)
- PostgreSQL 保存版本和同步操作。S3 兼容服务保存带作用域的不可变字节。平台先确认对象，再用数据库事务提交版本和操作记录。SQLite 与文件存储复用相同发布流程，便于本地演示。[S3 条件写](https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-writes.html) · [平台实现](../internal/platform/catalog.go)

## 云端多 Agent 与数算分离

2026-11-22 对照了公开产品、项目文档和故障记录。本节讨论中心 SaaS 向多种原生 Agent 派任务、保存执行结果、回收或更换计算节点的需求。这里的“多 Agent”包括平台可选择不同运行时。多个 Agent 协作时，可以用有版本的业务产物交接任务，原生会话的跨运行时转换需要单独验证。

| 参照 | 对 AgentMoor 有意义的证据 | 对照时要区分的范围 |
| --- | --- | --- |
| Notion External Agents | Claude 和 Cursor 接入同一工作区。以 Cursor 为例，会话在 Cursor 的基础设施执行，用户在 Notion 派活和接收结果。[产品发布](https://www.notion.com/releases/2026-07-01) · [执行位置](https://www.notion.com/help/connect-cursor-to-notion) | 证明业务产品能把执行交给外部 Agent。完整原生会话仍由执行方管理。 |
| AgentRouter（perixtar） | 自托管 API 可创建、跟踪、继续和取消 Codex 或 Claude Code 任务。Worker 领取任务并启动 Daytona 沙箱，PostgreSQL 存任务和事件，R2 兼容存储保存产物、补丁和清单。[项目](https://github.com/perixtar/AgentRouter) · [部署与生命周期](https://github.com/perixtar/AgentRouter/blob/main/docs/self-hosting.md) | 与可嵌入业务 SaaS 的执行服务最接近。项目自标 alpha，续跑窗口和沙箱回收有独立配置。 |
| UseAgent | 适配 Claude Code、Codex、OpenCode 等引擎。控制面记录任务和事件，可交付文档、报告及其他文件。[项目架构](https://github.com/useagenthq/useagent#architecture) | 每条线程优先复用自己的工作站。其沙箱接口未提供 checkpoint 或 snapshot，已发布产物和整机状态的恢复保证不同。[工作区说明](https://useagent.org/docs/features/workspace/) |
| Claude Managed Agents、阿里云百炼 | Anthropic 将会话日志、驱动 Agent 的循环和执行沙箱独立，可更换故障组件。百炼把事件历史持久化，支持空闲回收沙箱，开启文件系统快照后可在保留期内恢复。[Anthropic 架构](https://www.anthropic.com/research/managed-agents) · [百炼概述](https://docs.agent.bailian.aliyun.com/zh/managed-agents) · [快照语义](https://help.aliyun.com/zh/model-studio/managed-agents-session-operations) | 展示了计算与持久状态分离的两种实现。要让多种第三方原生 CLI 共用恢复契约，仍需逐个核验。 |
| GitHub Agent HQ | 同一个任务能分配给 Copilot、Claude、Codex，团队可以比较结果并在代码审查流程中接手。[GitHub 产品说明](https://github.blog/news-insights/company-news/pick-your-agent-use-claude-and-codex-on-agent-hq/) | 平台已有代码托管、Issue 和 PR 作为工作上下文。面向报告、表格等交付物的 SaaS 需定义自己的验收和归档规则。 |

需求侧也有实际使用信号。Notion 在与 Anthropic 的公开访谈中披露，External Agents 上线前三周，客户创建了 18,000 个 Agent，90% 的 Agent 活动由自动触发器启动。这个口径是创建数与触发方式，可以支持工作流需求判断，无法据此估算独立基础设施产品的付费客户数。[Notion 产品团队访谈](https://claude.com/customers/notion-qa)

### 换机之后能恢复到哪一步

- Rivet Sandbox Agent 的 TypeScript SDK 文档明确说明，连接失效时可以创建新的运行时会话，把最近持久化的事件作为下一条提示的上下文。这能恢复对话线索，但有事件数和字符数上限。[Rivet 恢复说明](https://rivet.dev/sandbox-agent/docs/session-restoration/)
- OpenHands 曾记录回收云沙箱后原生 Agent 会话无法继续的故障。平台已有事件日志，但具体 CLI 需要的会话标识、工作目录和原生文件也在被回收的环境里。该问题现已关闭，记录区分了补入历史摘要和恢复原生会话两种方法。[OpenHands 故障记录](https://github.com/OpenHands/OpenHands/issues/14260)
- Cloudflare 对不同运行时分别标注恢复能力。其 Claude Code、Codex 实验集成在容器丢失时，当前轮默认失败，也可重跑。文件可以从快照恢复，上次快照后的修改可能丢失。[Cloudflare 能力表](https://developers.cloudflare.com/agents/harnesses/)

### 对项目的判断

对已有 SaaS，优先复用沙箱与执行组件，把租户权限、任务身份、产物版本和验收规则留在自己的业务层。若把 AgentMoor 单独卖给其他 SaaS，值得验证的客户是既需要不同供应商的原生 Agent，又要求资产留在自己账户、节点回收后可交接的团队。可以让试点客户拿一项真实任务，在现成方案和 AgentMoor 上分别回收节点，对比能恢复到哪一步、是否重复执行外部动作、谁能查到最终交付版本，以及接入成本。

本仓库已验证四种 CLI 在同版本、新运行时目录中用原会话续聊。运行中的外部工具动作和跨 Agent 原生会话迁移超出本轮实测范围。以上竞品能力依据一手公开资料，本轮未部署竞品做相同的故障测试。[本项目验收记录](mvp.md)
