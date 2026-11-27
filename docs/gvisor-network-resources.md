# gVisor 的网络、资源与运行时回收

[INT-80](https://linear.app/intsheep/issue/INT-80) 增加可复验的网络和资源组件；[INT-82](https://linear.app/intsheep/issue/INT-82) 定位并验证了 OOM 后的运行时残留问题。结果是：指定 TLS 网关可连，受控的禁止地址被实际阻断；CPU、内存、共享内存、进程数、截止和取消都有真实的允许与拒绝证据。旧运行时的失败记录保留，正式参考组合使用含官方修复的 containerd `2.4.2`。

这是合成程序的组件验收。没有在此测试生产 Gateway、原生 CLI、客户 Skill 或 SaaS 发布整条链路，也没有使用真实模型密钥。`untrusted-isolated.v1` 门禁仍等待独立 [INT-81](https://linear.app/intsheep/issue/INT-81) 的提供方整链路验收。已有四 CLI 契约回归属于受信开发路径，不会因此开放客户代码执行。

实现与证据：[探针](../tests/kubernetes/gvisor_security_probe.py) · [合成负载](../tests/kubernetes/gvisor_security_fixture.py) · [官方运行时安装器](../tests/kubernetes/gvisor_security_containerd.py) · [离线负例](../tests/kubernetes/gvisor_security_probe_test.py) · [精选结构化证据](evidence/gvisor-network-resources-2026-11-27.json)。

## 实测组合与部署条件

正式 R20/R21 来源为干净的 `38a7a39b9a78b35a62877159a9a1f859762b967f`。它们没有被改写为随后合并主分支的结果。

| 部件 | 本次实际值 |
| --- | --- |
| 节点 | 新建的单节点 kind，`kindest/node:v1.35.0`；Linux ARM64、cgroup v2、`7.2.12-orbstack-00394-gbecdd6130068`。镜像 ID 在证据中。 |
| containerd | 官方 `2.4.2`，提交 `b7a25261e029d68978bbf290df0a62fc819b3c51`，归档和每个已安装二进制均核对 SHA256。 |
| gVisor | 官方 `release-20261123.1`，提交 `81f68cb7d4661a9313d891a1758a765abe8eb27e`；归档 SHA512、三个核心文件及实际 CRI/Sentry 都核对。 |
| 执行方式 | `runsc` / `io.containerd.runsc.v1`、systrap、`overlay2=all:memory,size=512m`；另有缺 handler 不回退的真实反例。 |
| CNI | 官方 Calico `3.33.0`；禁用默认 CNI 后创建节点，安装完成才等 Ready。官方清单 SHA256 与三个镜像摘要固定。 |
| 网络 | Pod CIDR 固定 `10.244.0.0/16`，避免与本机 Docker 网段重叠；IPIP 关闭、VXLAN 开启。 |
| 内存与交换空间 | 仅本次 kind 节点的父 cgroup 固定 2 GiB，实际 `memory.swap.max=0`。不改宿主全局 swap。 |
| 两层任务数 | 不可信 guest 的不可提升硬限制为 **64 个虚拟任务**；kubelet 的实际 Pod `pids.max` 为 **256 个宿主任务**。两者计数不同。 |

Kind 自带 containerd `2.2.0`。探针先读取真实 `ExecStart` 和二进制目录，再按官方支持的逐 minor 方式执行 `2.2.0 → 2.3.7 → 2.4.2`，保留新集群已有的控制面元数据。每步记录版本、官方归档/二进制摘要、CRI 和 API Ready。保存的 v2 配置字节没有改变；实际加载的配置 dump 自动迁移为 v4，并通过真实 CRI 和 gVisor Pod 验证。[官方升级与支持政策](https://containerd.io/releases/)

这只是该精确组合的实测。containerd 官方 Kubernetes 1.35 表未列 2.4，不能把本次结果写成该组合已获广泛官方兼容声明；AMD64 校验值已冻结，但本机没有执行 AMD64、生产多节点、HA 或带真实数据的升级验收。

gVisor 的 guest 进程并非宿主进程。Sentry、sysmsg 和运行时也需要宿主任务预算；Go PID1、原生程序和工具共同消耗 guest 预算。可复用的 [`LimitGuestProcesses`](../internal/guesttransport/process_limit.go) 为后续生产入口提供这一限制；本次测试使用 Go 启动器先调用它，再 `exec` 合成程序，检查继承值、拒绝升到 65，以及实际创建失败。将它接入完整提供方属于 INT-81，不能把组件探针当成该接线已通过。[gVisor 资源模型](https://gvisor.dev/docs/architecture_guide/resources/) · [Kubernetes PID 限制](https://kubernetes.io/docs/concepts/policy/pid-limiting/)

探针启用 runsc debug 以便定位运行时问题；这是测试设置。生产应关闭该 debug 并配置运行时、kubelet 日志轮换，日志容量和生产运维另验，不能把本探针当作宿主磁盘配额证明。

## 先证明能连，再证明被拦

受信 fixture 的同一个进程、同一个 Pod 和同一个 Service 同时提供网关 TLS `7443` 与主 API `7401`。Guest 固定 `hostAliases`，不开放 DNS。策略默认拒绝 ingress/egress，唯一 egress peer 同时匹配控制命名空间和网关 Pod 标签，只准 TCP `7443`。

| 实际地址 | 选中策略的 gVisor guest | 未选中的 gVisor control / 无策略基线 |
| --- | --- | --- |
| 网关 TLS 7443 | CA 和 ServerName 验证成功，实际 HTTP 响应正确。 | 成功。 |
| 同 Service 主 API 7401 | TCP 超时拒绝。 | 成功，确认与网关同一进程。 |
| Kubernetes Service 443 | TCP 超时拒绝。 | 成功。 |
| 另一租户 Pod 8123 | TCP 超时拒绝。 | 成功。 |
| 模拟 metadata `169.254.169.254:80` | TCP 超时拒绝。 | 成功，实际转向本测试的受控 server。 |
| Pod/Service CIDR 外的受控 server | TCP 超时拒绝。 | 成功。 |
| DNS TCP/UDP 53 | 超时拒绝。 | TCP 连接与 UDP DNS 应答均成功。 |

metadata 的 DNAT 只写入自己创建的 kind 节点网络命名空间，目标是自己创建的 Docker fixture；没有访问真实云 metadata，也没有修改宿主网络或已有集群。每个禁止地址都有实际可达的对照，不把无路由或端口未监听当成隔离通过。

探针还调用生产的 [`CheckNetwork`](../internal/guesttransport/network.go)：先向固定 Service IP 的 7443 做真实 TLS 正向检查，再检查同 IP 的 7401 和 Kubernetes 443。只有连接超时且父 context 没有取消才接受为拒绝；拒绝连接、无路由和证书错误都会阻止启动。策略正常时合成启动计数为 1；删掉策略后八项恢复可达，生产检查拒绝，计数仍为 1。这里的计数是明确的 fixture，原生 CLI 执行数一直为零。

R21 在相同最终运行时、host PID 预算和源码上使用默认 kindnet 作为反例。该固定 kindnet 版本已经实现部分 NetworkPolicy，不能称它完全没有策略能力；但它仍允许必须拒绝的 Kubernetes API canary，生产检查因此拒绝，fixture/原生执行均为 0。现代 kindnet 的实现可在[固定镜像对应源码](https://github.com/kubernetes-sigs/kind/blob/20ccfc88/images/kindnetd/cmd/kindnetd/main.go)中核对。

这些有限 canary 可发现忘装策略、默认 CNI 或部分规则未生效等常见错误，不证明任意互联网地址都已隔离。正确的 CNI、固定部署策略和节点权限仍是受信部署条件。

## 资源限制的实际效果

所有负载都在创建 Pod 时先固定硬上限和原始 `activeDeadlineSeconds`，再由可信测试触发，逐项独立观测。轮询确认 Pod UID、创建时间、startTime 和 deadline 没变；没有用重试刷新期限。CPU 固定 `200m`，宿主 PID 上限先安装并从真实 Sentry 所属的 Pod cgroup 读取。

| 项目 | 正式 R20 观察 |
| --- | --- |
| CPU | 实际 `cpu.max=20000 100000`，使用量增长，`nr_throttled` 增加 64。 |
| 匿名内存 | Pod `memory.max=128 MiB`，`oom_kill` 增加 2，程序终止。 |
| 共享内存 | `/dev/shm` 实际容量 64 MiB，写到边界返回 `ENOSPC=28`，没有 OOM。 |
| 临时文件与内存合计 | `/tmp` 写入受同一 128 MiB 内存 cgroup 约束，`oom_kill` 增加 1，程序终止。 |
| guest 虚拟任务 | 继承 soft/hard `[64,64]`，升到 65 被拒；创建 62 个子进程后得到 `EAGAIN=11`，无 OOM、无宿主 `pids.events.max` 增长。 |
| 宿主任务 | 实际 Pod `pids.max=256`；R19 的旧 128 上限先触发宿主事件并使容器退出，故保留为失败，不能拿它证明 guest 64。 |
| 原始墙钟截止 | 8 秒 Pod deadline 产生 `DeadlineExceeded`，任务终止，没有换 Pod 或重置期限。 |
| 整 Pod 取消 | 活跃负载开始后删除原 Pod，实际 Sentry/gofer/shim 消失。 |

每项先确认没有活的 Sentry、gofer 或 shim，再仅删除已经 `SANDBOX_NOTREADY` 的该 Pod CRI 历史记录，最后确认进程和 CRI 都为空。正式允许路径不执行 `stopp`、`kill`、`USR1` 或 `QUIT` 来促成通过。额外三个不同 UID/CRI 的新 Pod 各自触发一次文件内存 OOM，均按上述规则清理，没有累计 shim。最终节点、outside fixture、专属镜像标签和私有临时目录清理完成。

`emptyDir.sizeLimit` 和 `ephemeral-storage` YAML 不被当作同步硬磁盘配额。本次实际 ENOSPC 来自测量过的 `/dev/shm`；`/tmp` 的边界来自内存 cgroup。512 MiB overlay 是每个文件系统的容量，不是额外可用 RAM；Sentry、匿名内存和 tmpfs 共同受内存限制。早期没有验证并禁止父级 swap 的样本分配 256 MiB 后仍存活，不能当作 128 MiB 总预算通过；正式样本的父级 swap 已实际禁止。[Docker 的 memory/swap 语义](https://docs.docker.com/engine/containers/resource_constraints/)

## OOM 后残留 shim 的原因与官方修复

旧 containerd `2.2.0` 上，R11/R13/R14/R15/R17 多次在 OOM 后发现：guest、Sentry、gofer 与任务记录消失，shim 却仍在 `/system.slice/containerd.service` 中睡眠。它不受原 Pod 的内存/PID 配额约束，重复触发可能累积消耗可信节点资源，因此没有忽略它来让测试通过。

R17 是独立、始终不计验收的诊断。OOM 前保存的 metadata 表明：pause 与工作负载共享一个 task API v3 shim，工作负载 `SandboxID` 指向 pause。日志记录并发 Delete；pause 在 `07:08:12.674873385Z` 发出 Shutdown，而 `07:08:12.675319639Z` 返回仍服务 1 个容器。随后目标任务已消失，却没有最后一次 Shutdown；60 秒后 shim 仍在。对已确认归属、没有 guest/Sentry/gofer/CRI 的 own shim 先发非致命 USR1，从已删除日志 FIFO 的有界 reader 捕获完整 30,666 字节栈，再发 QUIT。栈显示 reaper、processExits、forward 等处于 idle，不是仍在运行客户程序，也没有 Shutdown callback/Init 锁阻塞的证据。

结合固定源码，旧 containerd 对带 `SandboxID` 的 v3 子任务删除会省略 Shutdown。若 pause 先完成删除，第一次 Shutdown 时其他任务尚未删完；最后子任务删除后便缺少让空 shim 退出的调用。[官方修复 `402eb316`](https://github.com/containerd/containerd/commit/402eb3166e088938090c759965f2ea7a3bbf22d4) 改为每次任务删除都调用 Shutdown，并附 pause 先退出的 CRI 集成回归。本次没有自建 shim、修改 gVisor 或增加宿主特权回收器。

正式候选固定官方 `2.4.2`，而不是把缺少修复的 `2.3.7` 作为最终环境。R18 在旧 guest 32 预算下只做运行时机制对照，完整矩阵和三个额外 OOM 通过；它不是最终 64 预算的验收。R20/R21 才是最终 64/256 配置的正式正反例。更早的缺 blob、归档路径、CIDR 重叠、swap、观测过早、OOM 和宿主 PID 先耗尽等失败各有原日志摘要与 SHA256，均保留在精选证据索引中，没有改写成成功。

索引中的 `diagnostic_only` 明确区分仅定位的运行：即使没有复现 orphan，诊断模式也故意不返回验收成功，不能把它计成新的运行时故障。

探针的 `--stage oom-diagnostic --baseline-containerd` 仅用于复现旧环境。主动删除已停止 CRI 记录和 USR1/QUIT 都明确标成诊断动作；无论是否复现，它不会返回验收成功。生产验收路径不会用这些动作掩盖残留。

## 复验

需要 Python 3.12+、Go、Docker、kind、kubectl、curl 和 OpenSSL。命令只创建、配置和清理自己的临时集群；安装 Calico 所需的 root/privileged 和集群 RBAC 仅存在于该集群。[Calico 官方 kind 安装说明](https://docs.tigera.io/calico/latest/getting-started/kubernetes/kind)

```sh
make fmt
make check
make demo
make test-k8s-gvisor-security
GVISOR_SECURITY_OUTPUT=/absolute/new/evidence.json make test-k8s-gvisor-security
GVISOR_SECURITY_OUTPUT=/absolute/new/negative.json make test-k8s-gvisor-security STAGE=kindnet-negative
make test-k8s
```

`GVISOR_SECURITY_BUNDLE`、`GVISOR_SECURITY_CALICO_MANIFEST` 与 `GVISOR_SECURITY_CONTAINERD_CACHE` 可指向已经下载的固定包、清单或目录；复用缓存仍强制比对固定摘要。未指定 `GVISOR_SECURITY_OUTPUT` 时只打印进度和结果，不写证据文件。`STAGE=network/resources/pids/oom-natural` 可分项复验；正式 `all` 包含七项资源与额外三次 OOM。

合并 main `d307e7e` 后，整合源码 `2b2538d` 通过 fmt/check/demo、15 项离线证明负例及完整四 CLI 契约：16 组检查、8 次追问，活跃原生回合取消耗时为 Codex 3.8 秒、Gemini 3.0 秒、OpenCode 3.7 秒、Claude Code 2.3 秒。专属集群和四个任务镜像标签已独立确认清理，完整来源和日志摘要记录在精选证据的 `merged_source_regression` 中。该回归与 R20/R21 的来源分开记录，不更改原始组件来源。真实模型、并发 Pod 配额、硬件虚拟化、生产 HA 和完整客户代码/Skill/SaaS 链路不在本组件的已通过范围内。
