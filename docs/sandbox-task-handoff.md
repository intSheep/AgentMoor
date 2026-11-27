# 任务与沙箱之间的交接接口

[INT-78](https://linear.app/intsheep/issue/INT-78) 将已领取的任务、固定输入版本和沙箱执行记录连接起来。当前交付是控制面的 typed HTTP API、原字节输入封包及下载 SDK；`serve` 和 Worker 的隔离提供方尚未接线，`untrusted-isolated.v1` 的 Worker 拒绝门禁保持关闭。这里的测试不能证明 SaaS 已在沙箱中运行真实 Agent。

## 从受信状态准备输入

控制面只接受私有 claim 的 `attempt`。作用域、任务、追问、parent checkpoint 和 Skill pins 来自签名身份与持久任务记录；适配器版本、运行时版本及 profile 指纹来自部署方的 `SandboxExecutionProfile`，不由任务文本或请求体选择。

`PrepareSandboxInput` 按固定版本读取原 Workspace、Session 和 Skill 包，核对作用域、版本和摘要，封装为最多 **64 MiB** 的 `native-guest.v1` 输入，再计算整个外包的 SHA-256 和大小并封存。平台不解包或导入原生会话，也不启动原生 CLI。原生格式的恢复仍由 guest 及对应适配器负责。

同一 claim、profile 和输入重试沿用原执行身份；来源 Skill 或 parent 的 head 升级不会改变已固定的包。失败的临时封包会清理，重启只清理本组件拥有的未登记上传临时文件，保留已封存输入和无关文件。[持久执行围栏](sandbox-execution-intents.md)

## 八条执行方接口

下表中的基路径有两种：首轮 `/v1/tasks/{id}/sandbox`，追问 `/v1/tasks/{id}/turns/{uuid}/sandbox`。每种各四条，全部是 **POST**，只接受执行权限；普通 SaaS 提交凭据、读取凭据、写资产凭据或 GC 凭据均不能调用。

| 后缀 | 作用 |
| --- | --- |
| 无 | 从当前有效 claim 准备输入，并调用受信 `SandboxController.Start`。 |
| `/status` | 调用控制器核对状态，返回执行记录和由受信状态构建的 native request。 |
| `/output` | 下载满足发布条件的已封存输出；返回整个外包的 SHA-256、大小及二进制内容。 |
| `/cancel` | 请求控制器取消当前执行；取消或到期后仍可按原 claim 查询和清理。 |

请求体只能是 `{"attempt":"<私有 claim UUID>"}`，最多 128 字节。重复 key、大小写别名、额外字段及尾随 JSON 均拒绝。其他作用域不可见，错误 claim 不得替代原执行者。没有配置 profile/controller 时返回不支持，不退回本地执行。

claim 的 `execution_policy` 与 Skill pins 一起冻结。未配置沙箱策略时，省略值兼容 `trusted-local.v1`；配置 `SandboxProfiles` 后，只接受该作用域已分配 profile 且 controller 可用的 `untrusted-isolated.v1` claim，缺 profile 也必须拒绝。同一 attempt 不能在两种执行方式之间切换。旧持久记录省略新字段，保留原执行围栏指纹。

## 下载完成后再使用

`Client.SandboxTask` 只访问固定任务相对路径；不能通过查询、fragment、authority 或 operation 参数改写目的地。`Client.SandboxOutput` 使用独立状态响应中的 sealed SHA-256/大小检查响应头，并在读取到 EOF 时验证实际完整字节。调用方必须读完并检查错误后才能执行或发布，不能只信归档内部的摘要。

若二进制流在发送响应头后失败，服务端中止连接，不把通用 API 错误正文追加进归档。输出下载不等于任务成功；后续可信 Worker 仍需验证结果、发布资产并完成原有 `finish` 协议。

## 2026-11-27 实际验证

实现源码 `628d849cca0c551e96c20e47989c97a098272513` 的 `make fmt/check/demo`、真实 PostgreSQL/S3 `make test-services` 和完整四 CLI 默认 `make test-k8s` 通过。[结构化证据](evidence/sandbox-task-handoff-2026-11-27.json)

HTTP 与 SDK 测试使用明确的合成 controller/会话数据，未启动 Pod 或供应商 CLI。当前任务/资产 API 合计 30 条路由，授权回归由原有 22 条矩阵和新增 8 条独立矩阵组成。本次验证八条路由的角色及四维作用域拒绝、私有 claim、严格请求体、策略冻结和缺 profile 拒绝、取消后清理、同长度输出损坏及独立外包摘要。输入测试另验证 Skill v1 和 parent 原字节不随 head 漂移、错误权限或损坏源不产生有效封包、重启清理及旧指纹兼容；相同输入准备断言也在真实 PostgreSQL/S3 上执行。

四 CLI 回归仍为受信文本路径、脚本模型、SQLite/本地对象存储；Codex、Gemini CLI、OpenCode、Claude Code 均完成交付后删除原 Pod、连续两次新 Pod 续聊及两种取消，原生取消分别约 **2.3/3.1/3.1/3.1 秒**。专属集群、节点和本次镜像标签已清理。这不是这八条新接口的沙箱端到端验收。

合入能力网关后的 `bb6cd7c6125520541cdb27d212ba941db89d62eb` 再次通过 `make fmt/check/demo`；没有把此前存储和四 CLI 测试写成合并后重跑。尚未验证 Worker/`serve` 接线、真实隔离 SaaS 文件/Skill/取消接续、实际 Codex 经私有 CA 网关访问模型、CNI 流量拒绝、资源耗尽、真实模型或生产 HA。相关组件的独立结果不能代替整条链路的验收。
