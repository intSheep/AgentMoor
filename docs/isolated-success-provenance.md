# 隔离成功回执与真实沙箱结果的关联

INT-89 在隔离任务成功时新增 `sandbox.result_accepted`。平台根据真实 Task/Turn 和私有 attempt 选择唯一 Run，把已经封存的输出摘要、实际 Pod UID 与成功回执连接起来。客户端不能指定 Run，也不能通过资产路径选择追问归属。

仅显式 `untrusted-isolated.v1` 首次从 `running` 变成 `succeeded` 时需要这项检查。普通受信开发执行保持原行为；已经成功的同一回执仍可重试确认，不依赖旧输入仍然存在、旧 profile 仍被启用或原截止时间尚未到达。

## 成功前核对什么

服务端先核对当前任务、Scope、Turn、私有 attempt、隔离策略和原截止时间，再读取大对象。实际 Run 必须属于这份原始 claim，处于 `ready`，没有被收集，没有错误，且输入、输出都已封存。原始任务 Spec、Parent、Skill pins 和 deadline 通过持久指纹绑定；受信配置选择的 profile、适配器版本及运行时版本也必须仍匹配。

持久输入不是只看一个摘要就接受。平台读取真实封存字节直到 EOF，独立校验整个 envelope 的大小和 SHA-256；`nativeguest.InspectRequest` 同时校验严格元数据、固定普通文件 slot、各 blob 的大小和摘要。读出的原始 request 必须与实际 claim 和当前 profile 重建的 request 完全相同。这个检查不解包内层目录，不落盘，不运行原生 CLI，也不读取 Session 正文来猜版本。

Run 还必须记录同一 Pod UID 已正常终止、退出码为零并完成删除确认。它保留原来的创建围栏、模板摘要和确定性 Pod 名称。本片使用现有控制器的终止与删除记录，不增加节点失联时的物理 fencing，也不把 API 对象不存在扩展为独立的节点进程证明。

大对象 I/O 在 Catalog 串行锁之外完成，因此取消可以继续提交。取得锁后，平台再次核对当前任务、profile、原 Run/输入/输出/Pod 身份及期限；中途取消、被收集或发生身份变化都会拒绝成功。整个首次成功事务仍受原始 deadline 限制，读取和等待数据库都不能续期。

## 与资产链接一起提交

首次成功的任务状态、成功事件、`sandbox.result_accepted`、全部 `asset.linked` 和检查点使用同一个持有写者租约的 SQL 事务。任何审计 INSERT 失败或写者租约丢失时，整笔事务回滚；不能先留下成功状态或部分资产链接。重复确认不追加事件。

新事件只含固定字段：实际 Scope、Task/Turn、attempt 域摘要、RunID、PodUID、封存 output SHA-256，以及前后状态。动作固定为 `sandbox.result_accepted`，组件为 `task-store`。HTTP Finish 记录已鉴权 execute 凭据的域摘要；它识别授权凭据，不是真实人员身份。事件不保存 attempt、token、能力、模型密钥、原生 Session ID、路径、Prompt、Session、Skill 或产物正文。

没有新增客户端字段或网络审计入口，也不改写旧 Task、claim 或创建围栏。固定审计枚举增加了这个 action；旧版严格读取器会拒绝未知事件，运维读取程序应与写入端一起升级。历史成功记录不回填虚构的关联。

资产 refs 继续复用成功回执已有的版本与摘要检查。可信 Worker 负责完整解码 guest result、校验和发布原始字节。平台没有在 Finish 时重建 guest 工作区并重新计算每份资产，因此这条关联不声称“服务端已独立证明 output 到所有 refs 的每个字节”。

## 验证与边界

聚焦测试通过真实 HTTP/SQLite 验证缺失或错误 Run、Scope/attempt、未 Ready、未终止、未删除、非零退出、收集墓碑、过期、profile/版本变化及输入等长损坏均不能成功。错误 attempt 在读取输入对象前被拒绝；对象读取期间的取消不会被串行锁阻塞。测试在 accepted 事件或第二条资产链接处实际拒绝 SQL INSERT，核对任务、accepted 和全部链接一起回滚，再用同一回执成功重试。成功后移除旧输入、停用 profile 并使期限过去，同一 ACK 仍可确认且不增加事件。

真实 PostgreSQL/S3 另测 accepted INSERT 被 trigger 拒绝及持有写者租约的连接被终止，不能改用读取连接继续提交成功。

```sh
go test -race ./internal/nativeguest -run '^TestInspectRequest' -count=1
go test -race ./internal/platform -run '^TestSandboxProvenance' -count=1
make fmt
make check
make demo
make test-services
make test-k8s
make test-k8s-sandbox-audit AUDIT_OUTPUT=/absolute/new/audit-chain.json
```

默认四 CLI 矩阵是任务接口的兼容性回归。独立 gVisor 组合验收使用真实固定 Codex、脚本模型，完成首轮加两次连续追问。SaaS 客户端只使用公开 HTTP；顶层受信集群夹具另通过运维只读 CLI 分页读取审计，与公开回执和实际 Pod UID 核对 Run → Pod → accepted → 每份精确资产版本。它不新增租户审计接口，也不把脚本模型描述成真实模型服务。

实际来源、命令、结果、原始日志摘要及清理记录见[验收证据索引](evidence/isolated-success-provenance-2026-11-27.json)。本片不实现过期任务的持久扫描、节点失联恢复策略、不可变外部审计归档或整个 P2 的完成。

2026-11-27，生产源码 `efa0b9915506f972fa27a05efbc633d6edfa707e` 的 `make fmt/check/demo/test-services` 和完整四 CLI `make test-k8s` 均通过；16 个交付/恢复/取消阶段、8 次连续追问，原生取消分别为 3.8/3.1/3.7/3.1 秒。真实 PostgreSQL/S3 的触发器失败与写者连接终止仍保持整笔回滚、无读取池写入回退；本次专用节点、四个运行标签和存储服务容器/网络已核对清理。

独立实际 gVisor 组合来源为 `003c425ebff0c0ec43ad7615665f557e5a5ddd8a`；它与上述生产源码的 `cmd`、`internal`、`go.mod`、`go.sum` 没有差异。固定真实 Codex 配合脚本 TLS 模型，在三个新 Pod 中沿同一原生 Session 完成首轮与两次追问，每个 Pod 只执行一次且实际 hard/soft NPROC 均为 64。11 份已交付资产的字节再次读取保持不变。运行时固定为官方 containerd 2.4.2、Calico 3.33.0、gVisor/systrap；不是一次新的全部资源/逃逸或节点失联验收。

单独的运维只读 CLI 以每页 7 条读取正在运行的 SQLite，共 11 页、64 条真实事件。三个 Run 与实际 Pod UID、输入/输出封存摘要、成功回执和全部 11 条资产链接一致；平台 execute 身份与实际 Worker 凭据域摘要相符，模型/输出请求分别保留 guest 委托身份。审计不含测试模型密钥、Worker token、能力值、原生 Session ID 或用户确认内容。原公共请求 UUID 重投三次没有新增事件；私有 Finish ACK 重投由前述 HTTP/数据库测试单独验证。比较器还拒绝替换 RunID、资产版本或输出 actor 的篡改样本，这是证明比较器的负例，不冒充后端故障注入。[完整组合原始记录](evidence/isolated-success-audit-chain-2026-11-27.json)

两次先前失败各保留实际来源与原件：第一轮在任务启动前遇到 Docker 旧缓存恢复错误；相同源码跳过缓存提示后构建成功。夹具现在保留失败构建日志，并仅对该明确缓存错误重试一次原固定版本构建。第二轮真实三轮交付已完成，但比较器把协议状态 `ready-to-publish` 误写为 `ready`，因此整体仍记失败；修正验收脚本后完整重跑，生产条件和版本未放宽。第三轮通过后，实际执行节点、运行镜像标签及单独构建诊断标签已检查清理；仅明确的公共依赖缓存保留。
