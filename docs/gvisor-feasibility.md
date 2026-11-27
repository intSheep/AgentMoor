# gVisor systrap 运行时可行性验收

这个入口检查测试机是否真正运行了 gVisor、运行时缺失时是否拒绝启动。验收范围是底层运行时；客户代码、第三方 Skill 与可信 Worker 之间的凭据、文件和网络边界需要另外验收。

当前 `untrusted-isolated.v1` 应用执行模式仍由 [Worker](../internal/worker/execution_policy.go)明确拒绝，直到独立隔离 provider 完成。运行本探针或填写一个 RuntimeClass 名称不会解除应用入口的阻断。

## 怎样运行

需要 Python **3.12+**、Docker 的 Linux 服务端、kind、kubectl、curl，以及网络访问 GitHub 官方发布和 Docker Hub。脚本支持获取 aarch64/x86_64 固定包；本页实际结果仅来自 aarch64。脚本修改的是自己创建的一次性 kind 节点内的 containerd，不修改宿主 Docker 服务或已有集群。

```sh
make test-k8s-gvisor-feasibility

# 如需保留完整原始证据，输出路径必须尚不存在，父目录必须已存在：
GVISOR_PROBE_OUTPUT=/absolute/new-gvisor-evidence.json make test-k8s-gvisor-feasibility

# 可复用已下载的官方 bundle，仍然复制、冻结并严格校验固定 SHA512：
python3 tests/kubernetes/gvisor_probe.py \
  --bundle /absolute/gvisor-aarch64.tar.bz2 \
  --output /absolute/new-gvisor-evidence.json
```

本机若通过 OrbStack 提供 Docker/kubectl，可将其工具目录加入本次命令的 `PATH`，例如 `/Applications/OrbStack.app/Contents/MacOS/xbin`。不需要模型密钥，也不运行原生 Agent。

每次创建 `agentmoor-<随机值>` 专属集群，所有 kubectl 操作都带显式 context 和私有 kubeconfig。正常结束、命令失败或收到可处理的退出信号时，夹具都会尝试删除自己的集群；成功结果还要求 Docker 中已无该节点、私有 kubeconfig 已移除。进程被直接 SIGKILL 或宿主中断时仍需按日志里的专属集群名人工清理；清理失败会令整个命令失败，并保留该名称。

```sh
# 只针对当前失败日志中归属本次探针的名称：
kind delete cluster --name agentmoor-<本次随机值> --kubeconfig /absolute/disposable-kubeconfig
```

无 `--output` 时只打印阶段和简要结果；需要审阅原始 Pod/CRI/事件与配置时应指定证据路径。公共镜像可能留在 Docker 依赖缓存中。

## 固定输入

| 输入 | 固定值 |
| --- | --- |
| gVisor | `release-20261123.1`，上游提交 `81f68cb7d4661a9313d891a1758a765abe8eb27e` |
| 安装包 | 该 release 的 `gvisor-<architecture>.tar.bz2`；SHA512 固定在脚本中，来自同 release 的 `SHA512SUMS` |
| 多文件布局 | 同级 `runsc`、`containerd-shim-runsc-v1` 和完整 `gvisor-bin/`，额外核对节点内三枚核心文件的实际摘要 |
| kind 节点 | `kindest/node:v1.35.0`；证据记录实际节点镜像 ID、内核、架构和 containerd 版本 |
| 测试负载 | BusyBox 1.37.0 的固定镜像索引 `sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec` |
| gVisor 平台 | 显式 `systrap`，不需要 `/dev/kvm` |

[官方发布](https://github.com/gvisor/gvisor/releases/tag/release-20261123.1) · [固定 SHA512 清单](https://github.com/gvisor/gvisor/releases/download/release-20261123.1/SHA512SUMS) · [多文件安装说明](https://gvisor.dev/docs/user_guide/install/) · [平台要求](https://gvisor.dev/docs/architecture_guide/platforms/)

## 实际检查

正例 Pod 使用 UID/GID 65532、禁止提权、只读根文件系统、drop ALL capabilities、RuntimeDefault seccomp、显式资源 limits，关闭服务账号 token 自动挂载与 Service 环境注入。检查 API 中实际生效的配置、容器实际 UID/GID、零重启以及工作负载输出。

RuntimeClass 名称和容器里的 `uname` 只是一部分证据。夹具同时要求：

1. Pod 的 UID 与节点 CRI sandbox 记录一致，CRI 的 handler 为 `runsc`、runtime type 为 `io.containerd.runsc.v1`。
2. 同一 sandbox ID 存在真实 `runsc-sandbox --platform=systrap` Sentry 进程。
3. 节点内 `runsc --version` 与固定 release 一致，核心可执行文件摘要等于已校验的官方包。

负例创建一个 RuntimeClass，其 handler 故意未配置。Pod 必须已经调度到节点、仍为 Pending，并在至少 10 秒观察中出现两次明确的 `no runtime ... is configured` 拒绝。CRI 中不得存在该 Pod 的 sandbox，容器不得启动。镜像拉取失败、无法调度或瞬时 Pending 都不能代替这个负例，也不会退回 runc。

[八项离线回归](../tests/kubernetes/gvisor_probe_test.py)已接入 `make check`，验证伪装的 RuntimeClass/guest uname、错误 CRI type、不同 sandbox 的进程、负例意外启动、单次事件、移除 Pod 限制、包摘要变化和越界/不完整 bundle 都不能产生通过结果。它们不创建集群或联网。

## 2026-11-27 实际结果

新仓库入口 `make test-k8s-gvisor-feasibility` 实际退出 **0**，时间为北京时间 **11:49:07–11:50:07**（UTC `03:49:07–03:50:07`）。[完整独立证据](evidence/gvisor-systrap-feasibility-2026-11-27.json)保留实际 Pod、CRI、进程、配置、事件和清理记录。

| 项目 | 本次结果 |
| --- | --- |
| 执行来源 | 仓库基础 `cf143f8910294d61d003326f1b4f3c9c182df304`；当时探针尚未提交，实际脚本 SHA256 为 `113bde3493d6a4a0ff54e2688a45d0fc4a584791039c1d02eac8b8972411e417`。后续提交不冒称已在该提交重新运行。 |
| 环境 | Linux aarch64、OrbStack 内核 `7.2.12-orbstack-00394-gbecdd6130068`、节点 containerd `2.2.0`；私有 kind 集群 `agentmoor-4114ee7daecc` |
| bundle | aarch64 完整包 SHA512 与固定官方值一致，节点核心文件再次核验一致 |
| 正例 | 实际 UID/GID 65532、零重启，CRI handler/type 与真实同 sandbox 的 systrap Sentry 均确认；负载报告虚拟内核 `4.19.0-gvisor` |
| 缺 handler 负例 | Pending、两次明确拒绝，10 秒观察；未创建 CRI sandbox、未启动容器、没有回退 |
| 清理 | 专属集群删除，Docker 节点为空，私有 kubeconfig 移除 |

这是新仓库入口的首次实际运行记录，代码来源按上表的未提交脚本摘要保存；更早的手工探索使用另一个独立集群，不混作本条命令的来源。

本次只运行静态 BusyBox 负载，尚未验收 AgentMoor、Codex 或 Skill；资源 limits 的存在也不等于资源耗尽防护已实测。凭据隔离、NetworkPolicy/受限出口、跨租户攻击、取消和断电恢复、宿主逃逸抵抗与生产 HA 都需要后续专门用例。gVisor 仍能访问挂入沙箱的文件及获准网络，平台凭据必须位于不可信边界之外。[官方安全模型](https://gvisor.dev/docs/architecture_guide/security/)

Kata 的硬件 VM 路径需要裸机或嵌套虚拟化；OrbStack 当前文档说明 Apple Silicon 不支持嵌套 KVM。这里的 systrap 结果不扩成 Kata/KVM 的验收。[Kata 要求](https://github.com/kata-containers/kata-containers/blob/main/docs/installation.md) · [OrbStack 限制](https://docs.orbstack.dev/machines/)
