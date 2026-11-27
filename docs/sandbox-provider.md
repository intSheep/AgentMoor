# Linux 集群的隔离执行提供方

本页描述 `kubernetes-gvisor.v1` 的部署契约。完整集群验收仍在进行中；组件各自通过测试，不代表整条生产执行链路已经验收。真实模型调用按用户安排后补。

## 部署方先准备的边界

控制面、可信 Worker 和不可信 Pod 分开。控制面持有 Kubernetes 身份、平台私钥和上游模型密钥；Worker 持有自己范围的 Execute token 和发布 journal；guest 只得到本次输入、输出、模型三种短期能力。guest 的原生命令、客户代码和第三方 Skill 都按不可信程序处理。

首版要求部署方管理 Linux Kubernetes 节点，安装已经验证的 gVisor `runsc`/systrap 和执行 NetworkPolicy 的 CNI。参考配置使用固定版本与镜像摘要；不能把某个 RuntimeClass 名称当作实际沙箱的证明。[gVisor 可行性](gvisor-feasibility.md) · [文件系统要求](native-guest.md)

- 节点的 runsc 配置必须有 `overlay2 = "all:memory,size=512m"`，并保留只读根、公开 CA 和只读能力挂载。512 MiB 是每个可写挂载的 overlay 上限，不是额外的内存预算；Sentry、匿名内存和文件写入仍需一起受实际 cgroup 限制。
- 首版禁用 guest 的 swap，节点按 `NoSwap` 配置，并检查真实 Sentry/Pod 的 cgroup 及父层，不能只看 Pod 的 `memory` YAML。使用嵌套 Docker 测试节点时，父容器 `--memory-swap` 与有限的 `--memory` 相同才禁用 swap。[Kubernetes swap 管理](https://kubernetes.io/docs/concepts/cluster-administration/swap-memory-management/) · [Docker 资源约束](https://docs.docker.com/engine/containers/resource_constraints/)
- 节点 kubelet 设置有限 `podPidsLimit`；guest 启动器在任何原生版本探测、导入或执行前，另设不可提升的进程硬上限。gVisor 的虚拟进程与宿主进程不是一对一，不能用宿主 `pids.max` 替代 guest 的实际 fork 负例。
- 先应用并实际验证 [sandbox-network.yaml](../deploy/kubernetes/sandbox-network.yaml)，再运行任务。只有专用模型/能力网关 TCP 7443 可达；主 API 7401、Kubernetes API、其他租户、metadata、外网和 DNS 不开放。NetworkPolicy 的执行依赖具体 CNI。[Kubernetes 说明](https://kubernetes.io/docs/concepts/services-networking/network-policies/)
- 安装 [sandbox-rbac.yaml](../deploy/kubernetes/sandbox-rbac.yaml)，将受信控制面的 ServiceAccount 绑定到沙箱 namespace 的 `pods get/create/delete`。它不需要 guest 的 exec、日志、Secrets、TokenRequest 或 RBAC 权限；与可选追问 Job dispatcher 的权限分别配置。

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
