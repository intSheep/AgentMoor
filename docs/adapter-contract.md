# 当前适配器契约与合成验收

新增适配器时，把原生目录、格式和命令留在自己的包，通过现有接口接入共用流程。本页说明当前仓库内部契约和 INT-94 的合成验收工具。仓库外模块使用后续交付的[Go SDK 0.x 预览](adapter-sdk.md)，不要直接导入 `internal/`；稳定 1.0 接口仍未冻结。

## 生命周期与职责

| 阶段 | 适配器负责 | 共用流程负责 |
| --- | --- | --- |
| 注册与描述 | 提供固定 ID、适配器版本、实际能力和精确实测 CLI 输出版本 | CLI 启动时显式构造 Registry；检查 ID/版本非空、拒绝重复 ID，按 ID 查询 |
| 发现 | 根据专属工作区标记给出证据；按自己的规则识别 Skill 和 Session 身份 | 在受信分配的 `os.Root` 中调用接口，不把共享 Skill 目录当作运行时身份 |
| 捕获与导出 | 返回原始 Session 文件及受支持的关联文件，或调用原生 CLI 导出 | 校验、打包、冻结资产；通过已鉴权 HTTP 发布不可变版本与摘要，维护 journal |
| 恢复 | 需要原生数据库时，把已校验的导出包导入新数据目录 | 读取精确版本，验证摘要，在同一文件系统的 staging 中完成后无覆盖发布到新路径 |
| 执行与续聊 | 实现所声明的任务接口；决定原生命令、原生 HOME 布局、Session ID 读取与 Skill 安装路径 | 按策略授权、准备独立状态与工作区、传递固定父版本/pins、处理取消、冻结发布和成功回执 |

普通 `sync`、`restore-native` 是受信维护入口。客户代码与第三方 Skill 的任务执行继续使用[隔离提供方](sandbox-provider.md)；这套合成验收不新增执行策略，也不授权在 Worker 宿主运行不可信 CLI。

## 必要与可选接口

| 接口 | 当前含义 |
| --- | --- |
| `adapter.Adapter` | 必要接口：`Descriptor` 与 `Detect`。空发现结果表示没有匹配证据，不等于错误。 |
| `adapter.SkillDiscoverer` | 发现相对于工作区根的 manifest 路径。共享目录只用于候选发现，具体规则归对应适配器。 |
| `adapter.SessionSource` | 从单一受信范围的 Session 根返回 `NativeSession`：原生 ID、格式、文件条目及 `Raw` 标志。文件名不代替内容中的原生身份。 |
| `adapter.NativeSessions` | 可选原生 CLI 导出/导入。操作者提供绝对可执行路径和专用数据目录；导入目标必须是新路径。 |
| `task.Executor` | 声明 `text-turn.v1` 时所需的首轮文本执行。 |
| `task.Continuation` | 声明 `continue-turn.v1` 时所需的原会话续聊；通过 `RestoreHistory` 恢复固定父历史。 |
| `task.FileExecutor` | 声明 `file-turn.v1` 时所需的文件交付；输出路径由共用流程准备，原生参数和格式仍归适配器。 |
| `task.SkillInstaller` | 将已校验的固定 Skill 放进适配器的私有 HOME 布局。能同步 Skill 不代表已支持任务中的原生 Skill 加载；当前任务安装由 Codex 实现。 |

`NativeSessions` 与 `SessionSource` 可以由同一个适配器实现。OpenCode 支持读取已有 JSON 导出，也支持调用 CLI 导出/导入数据库；验收必须说明实际走了哪条路径。任务和继续执行的完整状态约定见[任务契约](task-contract.md)。

能力声明检查核对所需接口是否存在，不替代任务执行正确性测试。声明了某个模式，不表示这套资产往返 suite 已执行过该模式。

## 描述字段与版本边界

`Descriptor.ID` 标识适配器，`Version` 标识适配器实现；它们不是原生 CLI 版本。`TaskModes` 声明实际任务模式，`Capabilities` 按资产种类列出操作。`Operation`、`Mode` 和 `Reliability` 都是当前描述字符串；没有可靠性等级的高低顺序，也没有由 Registry 强制的通用字符串白名单或 SemVer 兼容政策。

本片 `ValidateDescriptor` 只认识上述三种任务模式；未知任务模式会明确报告“不在本工具覆盖范围”，不会被默认为已经验证。Session 的 `runtime-cli` 声明要求 `NativeSessions` 和非空 `TestedRuntime`。其它能力模式与可靠性字符串保持描述性质，这不是新增的生产 Registry 策略。

例如 `verified-checksum` 表示相关字节经过摘要校验，`persisted-only` 限定为已经落盘的可观察状态，`tested-main-session` 限定为已验证的主会话行为。不能从其中任意一个词推导所有分支、工具状态、实时内存状态或任意历史版本都能恢复。能力列表和 map 的排列顺序不表达优先级。

`TestedRuntime` 保存精确的 CLI 输出字符串。`adapter.Inspect` 在独立临时状态目录执行有界 `--version` 探测，逐字比较后报告 `matches_tested_version`；它不是版本范围。未提供可执行文件时没有实测版本，不能据此声称兼容。

| 适配器 ID | 当前适配器版本 | 精确 `TestedRuntime` | 原生 Session 机制 |
| --- | --- | --- | --- |
| `codex` | `0.5.0` | `codex-cli 0.171.0-alpha.13` | rollout JSONL 的 `session_meta` ID 与原始日期目录 |
| `opencode` | `0.3.0` | `1.19.6` | 已导出 JSON；或 CLI list/export/import 到新数据库 |
| `gemini-cli` | `0.3.0` | `0.70.0` | 项目 chats 的原始记录与项目标识 |
| `claude-code` | `0.3.0` | `2.1.330 (Claude Code)` | UUID transcript 及实际支持的同 ID 关联目录 |

普通文件同步可以只捕获可观察的原始文件；不要把它等同于原生导入或执行已经通过版本门槛。需要原生导入/执行的路径会拒绝未验证或配置不符的版本，允许必要的版本探测，不应继续 import/export/execute 或产生成功发布。鉴权、读取上下文或 claim 可能已发生，不能把这个约定泛化成所有路径都在任何 HTTP 之前拒绝。

普通 `sync` 先重放已有合法的冻结 pending 操作，再探测新捕获所需的版本。历史操作的发布与本次新捕获的拒绝分别记录；版本反例使用空的新分配，避免已有 head 冲突提前挡住操作、掩盖版本检查本身。

## 复用合成验收

测试用途的入口在 `internal/worker/contracttest`。适配器自己的 case 只准备合成数据和专属 CLI 夹具，共享 runner 负责授权、HTTP、Scope、Worker state 和共同断言。调用方使用外部测试包 `worker_test`，避免 helper 导入 Worker 时产生循环。

```go
func TestAdapterAssetContract(t *testing.T) {
    contracttest.Run(t, codexcase.New())
    contracttest.Run(t, opencodecase.New())
}
```

每个 `Case` 提供名称与 `Prepare(t, Paths)`。`Paths.Workspace`、`Sessions`、`Runtime` 是分开的临时目录；CLI 调用计数写进 Runtime，不应因此改变工作区版本或原生 Session 正文。`Fixture` 返回适配器、机制、原生 ID/格式、Skill manifest 和预期 Session 原始字节。`SessionFiles` 使用相对文件名；单文件 `Raw` 导出用空 key 表示原始文件本身。

runner 用 `t.Setenv` 隔离 HOME、XDG 目录和合成凭据探针，因此它及上层测试不能调用 `t.Parallel`。合成凭据只用于检查夹具观察到的环境，不是实测平台密钥。

`NativeFixture` 只提供专用可执行文件、调用计数、切换版本和检查导入结果的回调。OpenCode 的命令参数、list/export/import 响应及数据库布局由 `opencodecase` 管理；通用 runner 不按适配器 ID 分支，也不解释厂商 JSON 字段。

本片的两种机制是 Codex 文件树和 OpenCode 的真实接口调用。后者调用实际适配器的 `NativeSessions.ExportNative/ImportNative`，可执行程序是有界的合成 CLI 夹具；它证明共用路径确实经过 list/export/import，不是一次新的厂商二进制或真实模型验收。

共同断言以原始字节、原生身份和精确版本为中心：Workspace、Artifact、Skill、Session 可往返读取；Session 和 Skill 恢复要匹配预期的完整普通文件集合与原字节，Workspace 则核对指定交付文件。head 前进后旧引用仍有效；已有目标、坏摘要、缺失引用和预先取消的恢复不能覆盖恢复目标或推进该次操作的成功状态。这里的取消检查在调用 `Restore` 前取消 context，不代表已经验证执行中 CLI 的取消。普通同步已独立提交的资产版本仍保留，失败重试继续使用 journal 和原操作键，不承诺全部资产一次原子提交。原生数据库的导入检查身份和恢复内容，不要求其物理数据库字节等于导出包。具体通过项和机制调用计数以本片最终测试日志为准，不能以 case 名称代替断言。

共享 Skill 目录不能单独证明运行时身份，已有 CLI 测试 `TestSharedSkillDirectory`（`cmd/agentmoor/main_test.go`）负责这个反例；本片 runner 只检查各 case 已准备的专属工作区能够被识别，不新增该反例的覆盖声明。

```sh
go test -race ./internal/worker -run '^TestAdapterAssetContract$' -count=1 -v
go test -race ./internal/worker/contracttest -count=1 -v
make fmt
make check
make demo
```

真实厂商 CLI 的原生往返命令仍为 `python3 tests/native_roundtrip.py <adapter> <绝对可执行路径>`；版本与已有证据见[验收矩阵](mvp.md)、[运行手册](operations.md)和[固定版本引用](verification-matrix.md)。四 CLI 任务矩阵、gVisor 隔离和持久服务各有独立验收范围，合成 suite 不继承它们的新来源通过结论。

## 2026-11-27 实际验证

干净源码 `be351461f600774eae438e116452819a1383a9f8` 的聚焦 race、完整 `make check` 和 `make demo` 均退出 0；此前 `make fmt` 无改动。Codex 文件与 OpenCode 原生接口两套 case 使用相同断言，后者实际记录 version/list/export/import 为 9/3/3/1，未调用厂商二进制。四种现有 Adapter 的声明正例及虚假接口声明反例也通过。

同一干净源码使用四份临时 Go overlay：关闭摘要、关闭原生导出版本门槛、关闭导入版本门槛，以及给会话增加未声明文件，均在对应断言准确失败。overlay 不修改受版本控制的生产文件。初版工具曾误要求可选 SessionID 关联字段、只校验会话文件子集，以及被已有 head 冲突掩盖版本门槛；开发期诊断分别保留，不写成生产缺陷或最终干净来源。

[命令结果、源码摘要及无损原始日志](evidence/adapter-conformance-2026-11-27.json)。本片没有重跑真实 PG/S3、四 CLI 集群矩阵或 gVisor，不将旧来源改标为本片结果。

## 尚未承诺的外部接入

当前可在仓库内新增独立 case 验证复用性，不能据此声称仓库外工程师已完成接入。外部可导入 SDK、兼容维护窗口、动态插件、跨语言 RPC、Native Hook 或 Provider 对外协议都不在本片范围。将来需要这些承诺时，再形成具体方案与维护约束；本片保持生产注册、任务 JSON、存储和执行门禁不变。
