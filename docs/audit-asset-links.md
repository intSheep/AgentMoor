# 成功回执的资产归属审计

INT-85 让平台回答“这次任务成功时，实际采用了哪些资产版本”。成功的 `Finish` 会生成 `asset.linked`，把已验证的 Workspace、Session 和每份 Artifact 连接到真实 Task/Turn。它记录版本和摘要，不复制文件正文。

归属来自通过鉴权的成功回执与服务端 `taskAddress`。普通资产名即使写成 `turns/<某个 ID>/out/...`，提交 revision 时也不会因此得到 Turn 归属。已有成功历史不回填虚构事件。

| 记录字段 | 可信来源与含义 |
| --- | --- |
| Scope、TaskID、TurnID | 服务端任务地址；首轮 TurnID 为空，追问使用已存在的 Turn |
| AttemptSHA256 | 当前已领取执行的 attempt，使用 `attempt` 域计算摘要 |
| SessionIDSHA256 | 通过成功回执验证的原生 Session ID，使用 `session-id` 域计算摘要 |
| AssetKind、AssetIDSHA256 | 已验证资产种类，以及按 `asset-id/<kind>` 域计算的资产 ID 摘要 |
| Version、Digest | 回执实际采用的精确版本和内容 SHA-256；不会改用较新的 head |
| Actor | 沿用内部可信 context；HTTP Finish 记录鉴权后的 execute 身份摘要 |
| Action、Component、State | 固定为 `asset.linked`、`task-store`、`succeeded` |
| Cursor、TimeUnixNano | 与其它审计事件共用的事务内全局游标和平台时间 |

审计不保存原生 Session ID、attempt 原值、资产 ID/路径、正文、URL、凭据或原始错误。这里没有新增 `RunID` 关系；Task/Turn 的成功回执与 sandbox Run 之间的独立权威关联仍属后续范围。

任务成功状态、成功事件、全部 `asset.linked` 与成功检查点在同一个持有写租约的数据库事务中提交。每次最多 128 份 Artifact，加上 Workspace 与 Session，共 130 条链接。任何一条链接写入失败，整笔事务回滚：任务不能先成功，也不能推进检查点。首次成功事务还受该执行原始截止时间约束：开始事务前检查，以同一 deadline 限制全部 SQL，并在提交前复查；等待数据库不能续期。相同成功回执重试不会追加链接，既有成功回执过期后仍可重试确认；失败、取消或被拒绝的回执不会生成成功链接。

`agentmoor.audit.v1` 新读取器继续接受此前的事件；固定枚举增加了 `asset.linked` 和仅该 action 可用的 `session_id_sha256`。旧版严格读取器遇到新事件会拒绝，因此运维读取程序应与写入端一同升级。没有新增面向普通用户的审计 HTTP 接口，也没有在本项实现物理 GC、HTTP 拒绝或耗时审计。

复验命令如下。数据、签名密钥与 provider 交互均使用合成夹具，不需要真实模型密钥。

```sh
go test -race ./internal/platform -run '^TestAuditAssetLinks' -count=1 -v
make test-services
make fmt
make check
make demo
make test-k8s
```

真实 HTTP/SQLite 验证包括首轮加两次追问、多产物与旧版本回执、伪造 Turn 路径、错误 Scope/引用、真实 TCP 应答丢失后的重试，以及 130 条上限。SQLite 与真实 PostgreSQL 都用数据库 trigger 分别拒绝第 1、2、4 条链接，检查所有链接、任务状态及旧检查点一同回滚。独立 PostgreSQL 测试数据库还会在第二条链接时实际终止持有写租约的连接，确认不能成功、不能改用读取连接继续写；新的合法写入者可以重试完成。另有真实 SQLite 第二条链接的受控等待，以及已在 PostgreSQL 后端观察到的第二条链接 `PgSleep`，验证原始截止时间能使整个成功事务回滚。数据库驱动取消 SQL 时可能丢弃持有写租约的连接；此时 Catalog 拒绝后续写入，需重建合法持有者，不会自动借用读取连接继续写。

四种 CLI 的 `make test-k8s` 是任务状态变更的集群回归；它不替代独立 gVisor 隔离提供方验收，也不声称本项已验证真实模型、SaaS 恢复或 sandbox Run 审计关联。具体源码、结果与日志摘要见[证据索引](evidence/audit-asset-links-2026-11-27.json)。

2026-11-27，冻结源码 `56fee1d` 的 `make fmt`、`make check`、`make demo`、`make test-services` 和完整 `make test-k8s` 均通过。四 CLI 共完成 16 组契约、8 次连续追问；原生执行取消分别耗时 2.3、3.1、4.6、2.3 秒。随后合入主线 `43e454c` 的差异仅含审计文档与证据，执行代码保持相同。两个依赖 fixture 缓存标签明确保留，临时节点、四个运行镜像标签及服务容器/网络已实际检查不存在。
