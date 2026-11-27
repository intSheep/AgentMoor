# Linux 集群的隔离执行提供方

本页描述 `kubernetes-gvisor.v1` 的部署契约。`19d0d5b` 已完成生产提供方的真实 Codex 文件、固定 Skill、取消及 guest 丢失集群验收；`4ce08cf` 另通过隔离文本三轮。两者使用私有 CA TLS 和脚本模型。[实际结果与范围](#2026-11-27-实际验收)；真实模型调用按用户安排后补。

## 部署方先准备的边界

控制面、可信 Worker 和不可信 Pod 分开。控制面持有 Kubernetes 身份、平台私钥和上游模型密钥；Worker 持有自己范围的 Execute token 和发布 journal；guest 只得到本次输入、输出、模型三种短期能力。guest 的原生命令、客户代码和第三方 Skill 都按不可信程序处理。

首版要求部署方管理 Linux Kubernetes 节点，安装已经验证的 gVisor `runsc`/systrap、官方 containerd **2.4.2** 和执行 NetworkPolicy 的 CNI。当前参考组合使用 Kubernetes 1.35、gVisor `release-20261123.1`、Calico 3.33；其他组合需独立复验，不能把某个 RuntimeClass 名称当作实际沙箱的证明。[gVisor 可行性](gvisor-feasibility.md) · [文件系统要求](native-guest.md)

旧 containerd 2.2 在 pause 先于 guest 删除时会漏发最后一次 Shutdown，已实际复现管理进程在 Pod/CRI 消失后残留。官方 2.4.2 包含[每次任务删除后发 Shutdown 的修复](https://github.com/containerd/containerd/commit/402eb3166e088938090c759965f2ea7a3bbf22d4)；参考探针在新节点上按 2.2→2.3.7→2.4.2 逐 minor 升级并保留元数据验证。它不是已有生产集群的自动升级器。生产部署须事先完成节点配置和验收，不使用额外的特权回收器弥补旧运行时。

- 节点的 runsc 配置必须有 `overlay2 = "all:memory,size=512m"`，并保留只读根、公开 CA 和只读能力挂载。512 MiB 是每个可写挂载的 overlay 上限，不是额外的内存预算；Sentry、匿名内存和文件写入仍需一起受实际 cgroup 限制。
- 首版禁用 guest 的 swap，节点按 `NoSwap` 配置，并检查真实 Sentry/Pod 的 cgroup 及父层，不能只看 Pod 的 `memory` YAML。使用嵌套 Docker 测试节点时，父容器 `--memory-swap` 与有限的 `--memory` 相同才禁用 swap。[Kubernetes swap 管理](https://kubernetes.io/docs/concepts/cluster-administration/swap-memory-management/) · [Docker 资源约束](https://docs.docker.com/engine/containers/resource_constraints/)
- 节点 kubelet 设置 `podPidsLimit: 256`；guest 启动器在任何原生版本探测、导入或执行前，另设不可提升的 **64** 个虚拟进程/线程硬上限。gVisor 的虚拟任务还需要 Sentry/sysmsg 等宿主任务，不能将两种计数一对一换算。实际 fork 负例须证明 guest 到限拒绝，且宿主 `pids.events` 和 OOM 计数未先增加；旧的宿主 128 额度会先耗尽，不是合格的 guest 64 验收。
- 先应用并实际验证 [sandbox-network.yaml](../deploy/kubernetes/sandbox-network.yaml)，再运行任务。只有专用模型/能力网关 TCP 7443 可达；主 API 7401、Kubernetes API、其他租户、metadata、外网和 DNS 不开放。NetworkPolicy 的执行依赖具体 CNI。[Kubernetes 说明](https://kubernetes.io/docs/concepts/services-networking/network-policies/)
- 安装 [sandbox-rbac.yaml](../deploy/kubernetes/sandbox-rbac.yaml)，将受信控制面的 ServiceAccount 绑定到沙箱 namespace 的 `pods get/create/delete`。它不需要 guest 的 exec、日志、Secrets、TokenRequest 或 RBAC 权限；与可选追问 Job dispatcher 的权限分别配置。
- 按专用节点容量设置 [sandbox-quota.yaml](../deploy/kubernetes/sandbox-quota.yaml) 的 Pod 数和总 CPU/内存额度；网关的流并发限制不能代替 Pod 配额。达到额度时 Kubernetes 拒绝创建，不重置原 attempt 或重新运行旧任务。另给容器运行时和 kubelet 配置有限日志轮换，生产关闭 runsc debug；探针的调试日志仅用于专属临时节点。

节点、CNI、镜像、证书与上述约束属于受信部署配置。新集群要复验实际允许/拒绝流量、运行时进程和资源耗尽，不能只检查 YAML 是否存在。运行前的两个固定连接探针仅能发现常见缺配；它们不能证明所有 IP、端口或未来策略变更都安全。

## 控制面配置

复制并填写 [sandbox-deployment.example.json](../deploy/kubernetes/sandbox-deployment.example.json)。占位符会被拒绝，必须替换成真实镜像 manifest SHA-256、网关 Service ClusterIP、固定模型及上游地址。镜像预装指定原生 CLI、AgentMoor、公开网关 CA；Codex 的模型 HTTPS 也必须信任该 CA，例如在构建镜像时安装到系统信任库。TLS 私钥和上游模型 key 不放进镜像。

每个 profile 的四维 `scope` 对应一个固定分配。执行镜像、RuntimeClass、可执行文件、模型、资源和网络 canary 都由部署配置决定；HTTP 请求不能覆盖。模型 profile 的 scope、模型 ID 和执行指纹由配置加载器从执行 profile 派生，预算或并发缺失时拒绝启动。

```sh
agentmoor serve --data /var/lib/agentmoor \
  --auth-policy /etc/agentmoor/auth-policy.json \
  --listen 0.0.0.0:7401 \
  --tls-cert /etc/agentmoor-tls/cert.pem \
  --tls-key /etc/agentmoor-tls/key.pem \
  --sandbox-config /etc/agentmoor/sandbox.json
```

沙箱模式强制 TLS 和两个不同的固定端口。两个 listener 必须都成功绑定后才开始提供服务；一个退出会一起关闭。主 API handler 不挂到 guest 网关端口。网关的 Service 同时发布 7401/7443，保留原端口并指向同一个控制面 Pod；NetworkPolicy 只允许 7443。guest 以固定 HostAlias 访问网关，不需要 DNS。

in-cluster Kubernetes 地址、公开 CA 和可轮换 SA token 默认来自标准 ServiceAccount 位置。集群外运行控制面时，配置明确的 Kubernetes HTTPS origin、CA、token 文件，以及 guest 看到的实际 Kubernetes Service `IP:port` canary。生产加载器总是为 guest 生成两个 canary，不能关闭；TCP 拒绝、错误路由、调用取消或 TLS 失败都不算隔离成功，只有有效网关 TLS 正例和两个实际超时拒绝才放行。

配置在启动时冻结；活跃执行及尚未发布的输出完成前，保留原 profile。重启只核对已有执行，不重置 create fence、预算、claim 或原 deadline。模型 key 文件可以在同一路径原子轮换，不重置已花预算。[持久围栏](sandbox-execution-intents.md) · [模型预算](sandbox-model-budget.md)

## 可信 Worker 配置

参考 [sandbox-worker.example.json](../deploy/kubernetes/sandbox-worker.example.json)。配置必须同时指定 `execution_policy: "untrusted-isolated.v1"` 和 `isolated_provider: "kubernetes-gvisor.v1"`，使用 HTTPS，省略 `runtime_cli`、`sessions`、`native_sessions` 和 `artifact_session`。版本约束可选，但必须与内置适配器验证过的固定版本一致。凭据文件和 StateDir 留在可信 Worker 的挂载中。

Worker 按原私有 attempt 领取并交接任务，经专用 API 等待沙箱封存结果、终止且按 UID 删除后，下载和校验整包。它不探测、启动、导入或导出宿主原生 CLI；仅在新目录恢复数据，验证产物后复用原冻结发布 journal。成功发布以后仍通过原 `finish` 协议提交 checkpoint。

取消或失败先撤销本次能力并请求删除原 Pod。没有确认物理删除时不发送成功的取消 ACK，任务可继续显示 `cancelling`，控制面后台继续核对与清理；不会改写旧 checkpoint 或延长原 deadline。调用失败、丢 ACK 或 Worker 重启不会新分配一次原生执行。缺提供方、版本不匹配或部署缺 profile 会拒绝，不能退回本地执行。

空策略及 `trusted-local.v1` 仍用于受信开发；普通原生导入/导出是独立的受信维护操作。本提供方不自动扩展到其他 CLI，不宣称生产 HA、硬件虚拟机边界或任意系统调用兼容。

## 2026-11-27 实际验收

干净源码 `19d0d5b40a48226402768f49d49228f9845901a3` 的 [production SaaS 探针](../tests/kubernetes/sandbox_provider_smoke.py) 退出 0。它实际启动 `serve --sandbox-config`、独立 TLS 能力/模型网关、固定 Pod controller、data-only Worker 和 Linux Codex `0.171.0-alpha.13`；SaaS 只用公开 HTTP 提交、下载、验算和写入独立业务数据库。模型服务使用随机合成共享 key，仅挂给可信控制面/模型服务，guest 镜像只带公开 CA。此次存储是 SQLite/本地对象，不将此前的 PostgreSQL/S3 组件结果当作同一组合通过。

| 实际检查 | 结果 |
| --- | --- |
| 文件与固定 Skill | 各三轮、每轮新 gVisor Pod；同一原生 Session ID 接续，旧固定字节可读，业务金额独立验算。Skill 来源 head 升为 v2 后仍实际执行原 v1 的脚本/参考文件，包成员原 SHA 保持。 |
| 原生执行与运行时 | 共 10 个不同 Pod UID、10 个独立 nonce，每 Pod 真正 `exec` 计数为 1；实际 CRI sandbox 和同归属 Sentry 关联。官方 containerd 2.4.2、runsc/systrap、Calico 3.33；guest RLIMIT_NPROC 软硬均 64，kubelet PodPidsLimit 256，专属父节点内存 4 GiB、实际 `memory.swap.max=0`。 |
| 不可信 Skill | 三轮均在原生 Skill 脚本中生成独立 proof。九个私有路径不可读，包括实际控制面 `/tls/key.pem`、`/signing/signing.key` 与模型 `/model-tls/key.pem`、`/model-auth/key`；平台/Worker/AWS/PG/Kubernetes 凭据环境缺失。API 7401 和 Kubernetes 443 实际连接超时，镜像中 mode 0666 文件写入返回 EROFS。MCP 子进程的模型 key 环境实际为空；未保存能力值。 |
| 执行中取消 | 活跃工具已写入同步标志后取消，1.247 秒确认；删除完成后才结束，旧 checkpoint/字节不变，新 UUID 从旧会话恢复且未带回未发布草稿。 |
| 精确 guest 丢失 | 对实际活跃 Pod UID `d54c4351-2f7b-4235-8062-1106b32df5fa` 发带 UID 前置条件的 DELETE；回合失败，无成功资产/新 checkpoint，原 deadline 不延长。原 UUID 重投两次仍失败且没有新 guest；新 UUID 从旧会话恢复，脏文件不在恢复内容中。 |
| 每个 workflow 的物理删除 | API 中原 UID 已不存在，并独立等待没有同归属 Sentry/gofer/shim 和 Ready CRI。Stopped CRI 历史另列，没有强删运行进程来制造通过。最终专属节点和镜像标签已删除。 |

[原始 JSON](evidence/sandbox-provider-2026-11-27.json) 保留 812,490 原字节，SHA-256 为 `b38e107336a7b0ca88949cdb0859474279cd9ab2b0e00660762c9045850b84dc`，含 Pod/CRI/进程归属、十次原生计数、三份九路径 proof 和各轮固定引用。实际私有挂载的存在由独立[只读可信 Pod/Service 观察](evidence/sandbox-provider-trusted-mounts-2026-11-27.json)佐证：未读取 Secret 内容，保存前删除所有 annotations/managedFields。全部已提交证据扫描无 `sbx1.` 能力值和私钥 PEM。[证据索引、原日志摘要与限制](evidence/sandbox-provider-index-2026-11-27.json)

独立的[合成 Pod 配额探针](../tests/kubernetes/sandbox_provider_quota.py)也在同一源码的新专属集群通过：restricted namespace 配 `pods=1`，首个固定 sleep Pod 创建返回 201，并实际关联 gVisor/Sentry；第二个同请求返回 403，查询 404，无 UID、无 CRI。删除首个后 quota used 回到 0，真实进程自然消失；仅之后清理 stopped CRI 历史。这是 Kubernetes admission 并发配额证明，未声称两个 SaaS 原生任务并行成功。[原始配额证据](evidence/sandbox-pod-quota-2026-11-27.json)

同一干净 `19d0d5b` 的 `make fmt/check/demo` 与完整四 CLI `make test-k8s` 均退出 0；四 CLI 原生取消约 3.7/3.0/3.0/3.8 秒，两次自动新 Pod 追问及首轮/追问取消通过，专属集群已删除。该四 CLI 矩阵仍是默认受信文本路径，不能代替隔离文本或其他 CLI 隔离验收。[命令、日志 SHA 与退出码](evidence/sandbox-provider-regressions-2026-11-27.json)

### 隔离文本回合补充

干净源码 `4ce08cf815581dfc5d31820aed3756e8ee5975ed` 的同一探针以 `--scenario text` 独立退出 0。专属 `text-job` 分配不配置 FileTool 或 Skill pins，data-only Worker 通过生产交接 API 执行。SaaS 首轮给随机 CSV，第二轮只给确认码，第三轮只给新校验码；三个新 gVisor Pod 沿同一个原生 Session，独立验算原金额、前次确认码和本次校验码。每 Pod 原生计数为 1、RLIMIT 为 64，原生 Session 首条 ID 保持；11 份固定资产在后续轮次后重读，原字节未变。每轮独立观察原 UID 消失、无同归属 Sentry/gofer/shim 和 Ready CRI，专属节点与本次镜像标签已清理。[原始文本证据](evidence/sandbox-provider-text-2026-11-27.json)

可信观察者只读收集了三次实际上游请求的有界结构日志：`text.format` 与 `verbosity` 均未出现，也没有 MCP 报告工具；模型返回合法 named Responses SSE。没有改写原生请求、绕过生产网关或放宽校验，不能据此宣称支持任意 `json_schema`。[实际请求摘要](evidence/sandbox-provider-text-wire-2026-11-27.json)

前次 `24cf983` 的真实运行完成首轮和第一次追问后，夹具误选旧 `report.json`，本地验算失败；文本追问的主文件实际为 `row.spec.output` 指向的 `reply.txt`。该[原始夹具失败](evidence/sandbox-provider-text-fixture-failed-2026-11-27.json)保留，没有写成生产失败或通过。修正选择后才运行上述新节点复验。为避免每次重下 CLI，保留明确的 `agentmoor-validation-cache:provider-codex-0.171-alpha.13` 依赖缓存：位于加入本次公开 CA 前，不含本轮 CA/私钥/运行密钥；`docker build --cache-from` 仍按当前 Dockerfile/COPY 构建 AgentMoor，再生成新 CA child。清理声明只覆盖专属节点及本轮标签，不包括这个公开依赖缓存。

上述验收使用真实原生 CLI、脚本模型和合成业务/Skill，不证明真实模型质量、任意恶意程序不会逃逸、生产 HA 或全部系统调用兼容。其他 CLI 隔离路径、真实模型及 PostgreSQL/S3 与完整隔离提供方的组合分别验收；早期组件页所写“未接线”描述该页当时的独立组件范围，当前接线状态以本页为准。
