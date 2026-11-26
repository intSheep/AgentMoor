# PostgreSQL/S3 发布故障与 Worker 丢失组合验收

本入口把文件块发布故障和活跃 Worker 突然丢失放在同一套 PostgreSQL/S3 环境中验证。它使用固定版本的真实 Codex CLI、脚本 Responses 模型和合成数据，不需要模型密钥。

**最新完整 V2 套件通过（89c3c155）。** `make test-k8s-v2` 在一个 kind 集群、一份 Codex 镜像中依次跑完 parts 文件、固定 Skill、长故障/Pod 丢失三个场景，全部完成合法外租户拒读和后端核验；本次自身还包含两次明确 running 时的原 UUID 重投检查。命令退出 0，集群已清理。[最新完整结果](#2026-11-26完整-v2-套件重跑通过) · [独立证据](evidence/postgres-s3-v2-suite-2026-11-26.json)

**第一轮组合验收通过（109b534）。** 源码 `109b534a5e9d65dcd19ffb34cab4c118f18406b7` 的长入口已成功结束并清理专属集群：八个发布边界、三次 manifest 失败耗尽及新 UUID 恢复、SIGKILL 137、真实生产期限、旧 UUID 不重建 Job、新 UUID 恢复和最终 PG/S3 核验均通过。[完整结果](#2026-11-26实际结果) · [原始结构化证据](evidence/postgres-s3-faults-2026-11-26.json)

`109b534` 当次结果不是整个 V2/P1 退出证明：它没有把例 1–3 放在同一 suite 命令中，也没有明确验证另一合法租户凭据拒读这些旧固定引用。这些项目由后续新运行独立补齐，不回填为 `109b534` 已通过的项目。[V2 退出门槛](verification-matrix.md#v2-退出门槛)

中间 `54cb556` 完整套件尝试在最后的跨租户读取发生 TLS 握手超时，命令退出 2，**该次整套未通过**。其已完成阶段与缺失结果仍保留在[失败记录](#2026-11-26完整-v2-套件失败记录)，不把后来 `89c3c155` 的成功写成这次也通过。

## 怎样运行

完整套件：

```sh
make test-k8s-v2
```

只复验同环境发布故障与 Pod 丢失的长组合：

```sh
make test-k8s-persistent-faults
```

依赖与[普通持久化集群入口](operations.md#postgresql--s3-下的集群工作流)相同：Go、Python、Docker、kind、kubectl，以及支持 `docker image save --platform` 的 Docker 服务端 API 1.48 以上。首次运行还需要下载固定 CLI 和镜像。实际入口固定 Codex，开启 `tar-parts.v1`；普通内容下载仍是原始 `workspace-tar.v1` 完整 tar。

这两条命令均显式包含生产环境的 **900 秒** Worker 期限等待，构建和其他场景另计时间。各自创建专属 kind 集群、kubeconfig、命名空间和镜像标签；退出时由夹具清理，不切换用户当前 Kubernetes context。PostgreSQL 与 MinIO 使用隔离的合成凭据和测试桶。

`make test-k8s`、`make test-k8s-persistent`、`make test-k8s-workspace-parts` 等普通入口保持原行为，不加入这段 15 分钟等待。独立的 `make test-k8s-pod-loss` 仍保留原用途。

完整套件同一命令共用一个 kind 集群和一份 Codex 镜像，依次跑 parts 文件、固定 Skill、长故障/Pod 丢失三个场景。每个场景使用自己的命名空间、数据库和桶；第三个场景内部继续共用同一套 PG/S3。`89c3c155` 已取得整套成功退出记录，来源与早期单项复验分开保存。

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

公开 GC 仍保留 **24 小时**孤儿窗口。本次分钟级组合只验证历史版本及有效 pending 在这一窗口内不被误删，不能证明 24 小时后真的删除孤儿。过期孤儿的真实删除正控制和失败重试仍由 `make test-services` 中的存储测试覆盖。没有改数据库时间、任务状态、生产超时或测试时钟。

## 2026-11-26：实际结果

运行源码固定为 `109b534a5e9d65dcd19ffb34cab4c118f18406b7`，不替换成后续文档提交。固定原生 CLI 为 Codex `0.171.0-alpha.13`，模型为脚本 Responses。实际 PostgreSQL 为 `18.7`，MinIO 为 `RELEASE.2026-04-17T00-00-00Z`。命令退出码为 0，已输出完整 `POD_LOSS_RESULT`、`PERSISTENT_RESULT`，自有集群已清理。

| 验收项 | 本次结果 |
| --- | --- |
| 首份报告与换 Pod 修订 | 通过；queued 追问更换控制面后仍为 queued，原 UUID 重投两次并恢复单 Job 调度 |
| Begin / manifest / part / commit 的 before、after 共八个回合 | 全部成功；各命中一次，固定操作、manifest、冻结 marker 与 checkpoint 断言通过 |
| manifest-before 连续三次 503 | 按预期 failed，无成功资产、checkpoint 不前进；随后新 UUID 恢复成功 |
| 原生执行计数 | 全部 14 个独立 Pod 各计数 1：12 个成功、1 个发布耗尽失败、1 个真实丢失 |
| 活跃工具 Worker 突然丢失 | SIGKILL 退出 137，删除专属 Job/Pod；最初公开状态仍为 running，无 Worker 确认 |
| 900 秒生产 deadline | 声明 `2026-11-26T15:30:25.760940572Z`；观察 interrupted 为 `2026-11-26T15:30:27.127846+00:00`，约晚 1.37 秒 |
| 丢失后的同 UUID 与新 UUID | 原 UUID 重投两次，新增 Job 为 0；新 UUID、新 Pod 恢复成功，沿用原会话，未恢复未发布脏文件 |
| checkpoint、业务结果与历史引用 | checkpoint 保持，业务库仍两行；GC 和丢失后恢复阶段逐一重读此前 44 份历史引用并核 SHA-256 |
| PostgreSQL 实际记录 | tasks 1、turns 13、revisions 48、operations 49 |
| S3 与文件块表示 | 12 份 Workspace parts 版本；核对 40 个被引用对象，其中 part 对象 22 个；无 SQLite/本地对象回退 |
| 同源码默认四 CLI 回归 | Codex、Gemini CLI、OpenCode、Claude Code 全部通过，专属集群已清理 |

同源码的 `make test-k8s` 另跑默认文本工作流：四个 CLI 均验证交付、两次新 Pod 连续追问、活跃追问和原生执行取消；原生取消约为 4.6、3.1、3.8、3.9 秒。各 CLI 的固定版本与实际镜像 ID，以及长组合全部九个 case 的完整 ledger、14 个计数、Pod 身份、摘要和后端信息，均保存在[证据 JSON](evidence/postgres-s3-faults-2026-11-26.json)，两份真实 RESULT 原样保留。

`make fmt`、`make check`、`make demo` 在创建该源码提交前、相同 Go/Python 源码树上通过，不冒称文档更新后又重跑。组合故障与 Pod 丢失本次覆盖的是 Codex + `tar-parts.v1` + PG/S3；默认四 CLI 回归是另一条文本工作流，不据此扩成所有 CLI 的同一故障组合。真实模型、多控制面副本和生产高可用仍未验收。

## 2026-11-26：完整 V2 套件失败记录

来源 `54cb556a3786b055e1f6f927196b069109b26e07` 的 `make test-k8s-v2` 实际退出 **2**，专属集群已清理。[独立失败证据](evidence/postgres-s3-v2-suite-failed-2026-11-26.json) 保存全部已输出 RESULT、完整九个故障台账和最终异常链；没有生成该来源的成功套件证明，也不修改第一轮 `109b534` 证据。

| 场景或阶段 | 该次实际范围 |
| --- | --- |
| parts 文件流程 | 契约、合法外租户拒读和后端检查均已完成；外租户对 16 份旧固定版本返回 404，owner 重读不变 |
| parts 固定 Skill 流程 | 契约、合法外租户拒读和后端检查均已完成；外租户对 17 份旧固定版本返回 404，含 Skill v1/v2 |
| 长场景 `POD_LOSS_RESULT` | 已成功输出，含九个故障台账、14 个原生计数、SIGKILL 137、旧 UUID 两次重投零新 Job、新 UUID 恢复及旧引用核验 |
| 该次真实期限 | deadline `2026-11-26T16:00:03.945143143Z`；观察 interrupted 为 `2026-11-26T16:00:05.292061+00:00`，约晚 1.35 秒 |
| 最终长场景租户隔离 | 对外租户固定版本内容 GET 的 TLS 握手超时，尚未输出第三份 `CROSS_TENANT_RESULT`；不能把超时计为拒读 404 |
| 最终长场景存储核验 | 位于租户检查之后，未执行到完整结果；只有前两份 `PERSISTENT_RESULT`，不能推算第三份通过 |

异常为 `urllib.error.URLError: <urlopen error _ssl.c:1129: The handshake operation timed out>`，发生于 `verify_tenant_isolation → require_404 → foreign GET`。此前已完成多少次长场景的外租户拒读没有独立计数输出，故不补猜数字；该 traceback 本身也不能确定底层原因。当时需要处理原因后重新跑完整套件；后续 `89c3c155` 的通过另列如下。

### 活跃回合原 UUID 重投的独立补充

另一次短文件复验以 host `75b44a181b75bd59f13e38b0d4ed02965bda9c5a`、预构建镜像来源 `54cb556`，在同一 kind 的新命名空间通过。真实 running 且工具阻塞时原 UUID PUT 两次，再观察 2.387 秒，完整记录与 checkpoint 保持，Job 1、Pod 1、同一容器、重启 0；随后取消与文件流程、外租户对 16 份版本拒读均通过。[独立证据](evidence/postgres-s3-active-replay-2026-11-26.json)

该短复验没有原生命令计数包装器，也没有重跑 SIGKILL/900 秒期限；它证明 Job/Pod/容器执行归属，不能改写成 `54cb556` 完整套件当时已经执行了活跃重投断言，也不能替代完整套件缺失的最终租户和后端结果。Docker 与运行时各自报告的镜像身份应分别记录，不据不同工具的 SHA 值推断未观察的类型或不匹配。

## 2026-11-26：完整 V2 套件重跑通过

实际来源为 `89c3c155a707dc977fd15692d4c54fff032b6dfd`，命令 `make test-k8s-v2` 退出 **0**。一条命令共用 kind `agentmoor-c463e4a82b85` 和一份固定 Codex 镜像，依次完成以下三场景；每场都有自己的完整契约、`CROSS_TENANT_RESULT` 与 `PERSISTENT_RESULT`，集群已清理。

| 场景 | 已提交版本 | parts Workspace 版本 | 被引用对象（其中 part） | 合法外租户固定内容 404 |
| --- | --- | --- | --- | --- |
| 文件交付与恢复 | 16 | 4 | 24（14） | 16 份 |
| 固定 Skill v1 / 来源 head v2 | 17 | 3 | 30（16） | 17 份，含 Skill v1/v2 |
| 发布故障与 900 秒 Pod 丢失 | 48 | 12 | 40（22） | 48 份 |

三场都先确认另一合法只读凭据的 `/v1/context` 正常、只有 tenant 不同，再检查其 assets/heads 为空、任务与 checkpoint 返回 404、逐份固定内容返回 404 且不泄露 revision header；owner 随后重读元数据和字节仍相同。共 81 次固定内容拒读覆盖实际历史版本，超时或鉴权失败不充当这项证明。各场真实 queued 控制面重启及原 UUID 重投也保留。

本次第一和第三场各自明确检查 running 回合的相同 question、相同 UUID PUT 两次。继续观察 **2.353 秒、2.330 秒**，完整记录与 checkpoint 不变，Job 1、Pod 1、同容器、重启 0；第三场的这个 Pod 随后才接受 SIGKILL。这是本次新套件自己的两份 `ACTIVE_REPLAY_RESULT`，不借用 `75b44a1` 短复验结果。

长场景再次完成八个 before/after 成功故障、一次三次 manifest 失败耗尽及新 UUID 恢复。14 个长场景原生计数都为 1；SIGKILL 退出 137，真实 deadline 为 `2026-11-26T16:25:14.293264351Z`，观察 interrupted 为 `2026-11-26T16:25:15.957088+00:00`，约晚 **1.66 秒**。之后原 UUID 重投两次不新增 Job，新 UUID、新 Pod 从最近成功 checkpoint 恢复；此前 44 份历史固定引用在 GC、丢失和恢复后仍核对一致。以上计数不扩成第一、第二场也启用了原生命令包装器。

转发修复限于测试基础设施的 port-forward 输出处理：stdout/stderr 改写自有临时文件，以 `pread` 读取完整 readiness 行，不改变子进程的文件位置；重连保留原回环端口，失败时保留有限尾日志。自有假子进程 150,000 行、4,350,000 字节 stdout 能确定性复现旧 PIPE 阻塞，五项离线测试验证修复后的进展、完整端口、重连、诊断及清理。这与原 TLS 超时相符，但原 traceback 本身不能直接证明同一底层原因；没有通过忽略超时或把它改判 404 完成验收，生产 Go 行为保持原样。

同一干净 `89c3c155` 的 `make fmt`、`make check`、`make demo` 实际退出 0，含五项 port-forward 测试；Go race 结果包含正常缓存命中。另一次同源码默认四 CLI `make test-k8s` 也退出 0，均完成两次新 Pod 追问和两类取消，原生取消约 2.3、3.0、3.8、3.8 秒，专属节点与四个镜像标签均确认清理。

[最新结构化证据](evidence/postgres-s3-v2-suite-2026-11-26.json) 原样保存三个场景的全部契约、三份租户证明、三份后端结果、完整九个故障 ledger 和两份 active 重投记录，并记录同源码四 CLI 镜像身份与 Host 检查。Docker/kind-load 报告的镜像 ID 与 Pod 状态报告的 image ID 分字段保存，不推断其未观察的类型。旧 `109b534` 通过记录、`54cb556` 失败记录和 `75b44a1` 独立短复验均保持原来源；真实模型、超过 24 小时的孤儿删除、多控制面副本和生产 HA 仍不在本次证明范围内。
