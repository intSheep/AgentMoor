# PostgreSQL/S3 发布故障与 Worker 丢失组合验收

本入口把文件块发布故障和活跃 Worker 突然丢失放在同一套 PostgreSQL/S3 环境中验证。它使用固定版本的真实 Codex CLI、脚本 Responses 模型和合成数据，不需要模型密钥。

**当前为阶段性结果，尚未记为整体验收通过。** 源码 `109b534a5e9d65dcd19ffb34cab4c118f18406b7` 已完成八个发布边界、三次 manifest 失败耗尽及其新 UUID 恢复，随后真实 SIGKILL 退出 137；截至 2026-11-26 15:20 UTC，仍在等待公开的生产 deadline `2026-11-26T15:30:25.760940572Z`。期限后的 interrupted、旧 UUID 重投、新 UUID 恢复和最终后端核验尚无本次成功结果，见[阶段记录](#2026-11-26阶段记录)。

## 怎样运行

```sh
make test-k8s-persistent-faults
```

依赖与[普通持久化集群入口](operations.md#postgresql--s3-下的集群工作流)相同：Go、Python、Docker、kind、kubectl，以及支持 `docker image save --platform` 的 Docker 服务端 API 1.48 以上。首次运行还需要下载固定 CLI 和镜像。实际入口固定 Codex，开启 `tar-parts.v1`；普通内容下载仍是原始 `workspace-tar.v1` 完整 tar。

这条命令显式包含生产环境的 **900 秒** Worker 期限等待，构建和其他场景另计时间。它创建专属 kind 集群、kubeconfig、命名空间和镜像标签；退出时由夹具清理，不切换用户当前 Kubernetes context。PostgreSQL 与 MinIO 使用隔离的合成凭据和测试桶。

`make test-k8s`、`make test-k8s-persistent`、`make test-k8s-workspace-parts` 等普通入口保持原行为，不加入这段 15 分钟等待。独立的 `make test-k8s-pod-loss` 仍保留原用途。

## 同一个环境中的执行顺序

1. 在同一个命名空间、PostgreSQL 数据库和 S3 桶中生成首份报告；删除初始 Pod 及其临时工作卷。
2. 接受 queued 追问后真实更换控制面 Pod，使用原 UUID 重投，再恢复调度。新 Worker 按原会话 ID 恢复工作区并修订报告。
3. 逐个运行八个发布故障回合，每个回合使用新 UUID 和独立 Pod。随后让 manifest 连续三次失败，确认任务失败，再用新 UUID 从最近成功 checkpoint 恢复。
4. 让下一轮真实原生工具进入活跃等待，从专属 kind 节点向已核验的 Worker 进程发 SIGKILL；核对退出 137 后删除其 Job、Pod 和临时卷。
5. 等待公开任务 deadline，要求状态从 running 变成 interrupted。重投原 UUID 两次不能创建替代 Job；再用新 UUID、新 Pod 从旧成功 checkpoint 续接并审计报告。
6. 最后核对真实 PostgreSQL 表、S3 对象和历史文件块引用，记录实际服务版本与镜像身份，并确认没有 SQLite/本地对象存储回退。

业务状态、金额、固定版本下载和 checkpoint 都通过公开 HTTP 验证。Kubernetes 操作只用于部署、故障同步、计数取证和清理；最终存储检查用于证明后端选择，不替代 HTTP 业务断言。

## 故障怎样注入

Worker 的 HTTPS endpoint 指向独立的测试代理；代理通过另一段 HTTPS 连接 API，并校验 API 的 CA 和主机名。两段都限制为 HTTP/1.1，以便真实关闭 TCP 后产生当前重试策略能识别的 EOF/连接错误。SaaS 客户端始终直连 API，不经过故障代理。

管理端口固定为代理 Pod 内的 `127.0.0.1:7444`。受信 runner 通过 `kubectl exec` arm、查看状态和 release，不把管理接口暴露给 Worker 网络。第一次选中的请求停在可见的 barrier；等待时不占代理锁，公开监控 GET 和管理查询照常工作。接口及内存边界见[代理说明](../tests/kubernetes/faultproxy/README.md)。

| 发布阶段 | 请求前故障 | 请求成功后丢应答 |
| --- | --- | --- |
| Begin | 不发往 API，返回一次 503 | API 的真实 2xx 响应完整读完并关闭后，断下游 TCP |
| Prepare manifest | 不登记清单，返回一次 503 | 清单登记实际成功后，完整读完 2xx 再断 TCP |
| Upload one part | 不向 API 上传选中的 body，返回一次 503 | 该 body 上传实际成功后，完整读完 2xx 再断 TCP |
| Commit | 不请求元数据提交，返回一次 503 | 元数据实际提交成功后，完整读完 2xx 再断 TCP |

上游非 2xx 会正常转发，不算“成功后丢应答”。part 故障只选首次成功 Prepare 返回的第一个 missing 摘要，避免并行上传把一次尝试误算成多个命中。成功上传后，下一轮 missing 可以不再含这个 body；测试不强求所有 PUT 都重复两次。

## 冻结内容、幂等与单次执行

runner 在提交回合前注册精确的 `turns/<uuid>/root` Workspace key。代理使用真实 `asset.Change` 和 `asset.Fingerprint` 识别操作，记录逻辑 tar 摘要、base、canonical 未压缩 manifest 摘要、实际 gzip 字节摘要，以及 part 的 SHA-256、大小、路径和上游状态。不在 Python 中重写操作键算法，也不记录凭据、请求正文或 URL 查询串。

barrier 出现后，夹具修改当前 Worker 中已经冻结的非 Artifact 文件 `state.json` 的 marker，确认本地字节确实变化。恢复后的完整 tar 必须仍符合首次 Change 的 SHA-256 和大小，marker 仍为冻结前值。`out/` 报告不被这次注入修改，业务端继续校验原生工具生成的报告与工具证明。

每轮原生 `codex exec` 由测试专用[包装脚本](../tests/kubernetes/native_count.sh)计数；`--version` 不计。计数文件在 workspace/state 之外，脚本随后 `exec` 固定的真实 CLI，不在 Worker 外套 shell，因此 Worker PID 1 仍是 `agentmoor`。已退出 Pod 从 `terminated.message` 的 `native_exec_count` 验证恰为 1；SIGKILL 前的活跃 Pod 直接读取独立计数文件。模型请求次数或工具事件次数不充当原生执行次数。

每个成功 Workspace key 只能有一个版本和一个原操作身份。暂停、耗尽和丢失时，最近成功 checkpoint 不得越过失败；只有整轮成功 finish 后，新的 Workspace/Session 引用才一起进入 checkpoint。Commit 成功但应答丢失时，单个 revision 可能已经存在，此时 checkpoint 仍必须保持旧值。

## 历史版本与 GC 的范围

测试在发布暂停、存在 pending 以及恢复成功后，用单独的作用域 GC 凭据调用公开 `/v1/gc`，再经公开 HTTP 重新下载此前全部固定版本，核对身份、大小和摘要。文件块的历史引用不只看 head；最终后端检查同时核对普通 `/objects/` 和历史 `/parts/` 对象。

公开 GC 仍保留 **24 小时**孤儿窗口。约 15–25 分钟的本次组合只能验证历史版本及有效 pending 在这一窗口内不被误删，不能证明 24 小时后真的删除孤儿。过期孤儿的真实删除正控制和失败重试仍由 `make test-services` 中的存储测试覆盖。没有改数据库时间、任务状态、生产超时或测试时钟。

## 2026-11-26 阶段记录

当前记录来源为 `109b534a5e9d65dcd19ffb34cab4c118f18406b7` 的实际运行。固定原生 CLI 为 Codex `0.171.0-alpha.13`，模型为脚本 Responses；配置的服务镜像为 PostgreSQL `18-alpine` 和 MinIO `RELEASE.2026-04-17T00-00-00Z`，实际后端版本、对象计数与完整镜像身份须等待最终 `PERSISTENT_RESULT`。

| 已观察阶段 | 当前结果 |
| --- | --- |
| 首份报告与换 Pod 修订 | 已完成；queued 追问更换控制面、原 UUID 重投阶段已完成 |
| Begin / manifest / part / commit 的 before、after 共八个回合 | 已完成；每个选中故障命中一次，同操作/清单、冻结 marker、checkpoint 和原生计数断言通过 |
| manifest-before 连续三次 503 | 按预期 failed，无成功资产，checkpoint 保持；随后新 UUID 恢复已完成 |
| 发布阶段汇总 | 已记录 `successful_rounds=11`、`failed_rounds=1`，包含初始、修订、八个故障恢复及耗尽后的恢复 |
| 活跃工具 Worker 突然丢失 | 已核对退出 137、删除专属 Job/Pod；公开状态仍为 running，无 Worker 失败确认 |
| 900 秒生产 deadline | `2026-11-26T15:30:25.760940572Z`；截至 15:20 UTC 仍在等待，尚未观察 interrupted |
| 丢失后的原 UUID 不重建 Job、新 UUID 新 Pod 恢复 | 尚未完成本次断言 |
| 最终 PG/S3 核验、完整结果与清理 | 尚未完成，未生成成功证据 JSON |

只有本次运行输出完整 `POD_LOSS_RESULT`、`PERSISTENT_RESULT` 且命令成功结束后，才能把本页更新为组合验收通过。现有阶段记录不能替代该结果，也不代表真实模型、多控制面副本或生产高可用验收。
