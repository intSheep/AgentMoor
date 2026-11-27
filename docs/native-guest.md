# 原生 Guest 协议与执行核心

这个组件把“运行原生 CLI”收进一个独立进程：输入是本回合固定的任务、工作区、原生 Session 和 Skill；输出是完整工作区、原生 Session 和回答。它不读取 Worker 配置或连接平台，也不选择宿主路径。[INT-72](https://linear.app/intsheep/issue/INT-72)

`guest-execute` 本身不建立隔离。部署方必须把它放进指定沙箱，独立配置网络和资源，不挂入平台身份或可信状态。当前 `untrusted-isolated.v1` 任务入口仍然拒绝执行；本组件不是本机运行客户代码的入口。

## 一次执行怎样交接

1. 受信调用方固定适配器、版本、CLI 绝对路径和新目录。stdin 提供 `native-guest.v1` 请求 tar。
2. Guest 完整验证输入后才探测原生版本、安装 Skill 或启动 CLI。每轮使用新的 workspace 和 HOME，只恢复选定的原生 Session，重新安装固定 Skill，不恢复旧 HOME。
3. 通过现有适配器接口执行首轮或原会话追问；原生命令和原生格式规则继续留在各自适配器中。
4. 捕获工作区与当前会话的原始字节，完整封存结果后才写 stdout。失败不会输出半份成功结果，也不重跑原生命令。

```sh
agentmoor guest-execute \
  --adapter codex --runtime-cli /absolute/operator-selected/codex \
  --root /absolute/new-guest-directory \
  < request.tar > result.tar
```

该命令供沙箱 transport 使用，不是 SaaS API。父目录须存在，目标须为新路径；错误写 stderr，stdout 只承载结果 tar。

## 字节与身份约束

| 项目 | 约束 |
| --- | --- |
| 外层归档 | 固定顺序的 USTAR 普通文件，固定权限和身份；拒绝扩展头、链接、重复/额外条目和尾随字节。必须恰有两个 512 字节终止块后到 EOF。 |
| 请求 | `request.json`，可选 `workspace.tar`、`parent.session`，随后固定编号的 Skill 包。没有平台 Scope、token、URL 或宿主路径。 |
| 结果 | `result.json`、`workspace.tar`、`session.blob`。会话身份和适配器/运行时版本必须匹配原请求；续问不能换原生会话或格式。 |
| 上限 | 每个方向的完整 envelope 最多 64 MiB，可通过命令参数调低；请求 JSON 128 KiB，回答 2 MiB，结果 JSON 16 MiB。工作区和会话捕获共用输出预算。 |
| 原始包 | 校验大小、SHA256、format 和 media type。内部树另校验条目数、展开大小、类型和相对链接，拒绝越界与稀疏扩展。 |
| 可信侧 | `ReadResult` 必须匹配受信的预期请求并完整校验。后续还须核对平台封存的整个 envelope 摘要，不能只信归档里的自报摘要。 |

平台凭据环境变量的拒绝检查只用于发现误用；不能证明所有秘密已不存在。强隔离仍依赖受信 Pod 模板、实际运行时、挂载、网关、网络及资源限制。Guest 的原生进程可访问自己被分配的所有文件和权限。

## 验证边界

`make check` 含明确命名的合成适配器和真实测试子进程：首轮、文件、续问、固定 Skill、原始 Session 导入、取消、错误版本/路径/归档、输出超限，以及无成功输出、不重复执行。它们验证协议和驱动，不冒充供应商 CLI 或沙箱隔离实测。

## 真实 Codex 与三个新 gVisor Pod

```sh
make test-k8s-native-guest

# 保留完整证据，文件须尚不存在、父目录已存在：
NATIVE_GUEST_OUTPUT=/absolute/new-native-guest-evidence.json make test-k8s-native-guest

# 复用固定官方包，仍校验固定 SHA512：
python3 tests/kubernetes/native_guest_smoke.py \
  --bundle /absolute/gvisor-aarch64.tar.bz2 \
  --output /absolute/new-native-guest-evidence.json
```

依赖沿用[运行时探针](gvisor-feasibility.md#怎样运行)，另会构建 Go 与固定 Linux Codex 镜像，首次下载可能较慢。只创建自己的临时集群、安装其中的 runsc 并清理；不会修改已有集群或宿主运行时。模型是脚本端点，使用合成输入和占位身份，不需要真实模型密钥。

本参考运行时需要在受信 handler 的 `/etc/containerd/runsc.toml` 的 `[runsc_config]` 下设置 `overlay2 = "all:memory,size=512m"`。gVisor 的内存覆盖层使本例的工作挂载支持原子无覆盖目录发布；仅使用 Kubernetes `emptyDir.medium=Memory` 在本次环境仍失败。该配置只能由部署方选择，任务不能更改。[官方文件系统说明](https://gvisor.dev/docs/user_guide/filesystem/)

内存覆盖层占用容器内存，`size` 是覆盖层上限；不能将它当作 Pod 总内存、磁盘或并发耗尽已通过的证据。执行完成后必须通过封存输出交接，不能依赖宿主直接读取下层 emptyDir 上的变化。

### 2026-11-27 实际结果

核心源码 `b45250b4ebb85f94c056df4d4d6bbb2c1bd1d0ad`，实际未提交夹具 SHA256 `1a1b890440ff0438cd74cae6a9198955f3bef792645b3541edc3084b6510c926`，UTC `04:57:30–04:58:39` 的最终运行退出 **0**。[完整原始结果](evidence/native-guest-gvisor-2026-11-27.json)

三个全新 runsc/systrap Pod 使用真实 Codex `0.171.0-alpha.13`，各执行一次，依次生成、修改及核验真实工具报告；两个追问保留同一个原生会话。验证独立金额、工作区专属标记、工具记录、原始会话归档和旧冻结字节；实际镜像、Pod UID、CRI sandbox、同 sandbox Sentry 参数和各输入/输出摘要均保留。

每个 Pod 另外实测原子发布成功、目标碰撞返回 EEXIST 且原内容保留；只读镜像根目录和只读 DownwardAPI 挂载均以 EROFS 拒绝写入，而不是碰巧依靠非 root 文件权限。集群、私有 kubeconfig 与运行镜像标签已清理，依赖镜像缓存可保留。

前两轮失败保留在[诊断记录](evidence/native-guest-filesystem-diagnostics-2026-11-27.json)：默认 Gofer 工作挂载拒绝原子目录发布；第二轮明确记录原生命令计数不存在、输出为零，首轮仅记录相同错误。对照证实宿主内存卷仍失败，Sentry 内部 tmpfs 成功；修复受信运行时配置后通过，没有改动公共快照的原子不覆盖规则。

同一核心源码的完整四 CLI 默认 `make test-k8s` 退出 0：交付、两个新 Pod 续问和两种取消均通过，实际 CLI 版本独立读回确认，集群与运行镜像标签清理。[回归记录](evidence/native-guest-regression-2026-11-27.json)。该矩阵仍是受信文本路径。合入最新存储主分支后的集成源码 `4aef8c848db19730c1a46404cc29055728a6c86d` 的 `make fmt/check/demo` 也通过；原生 Guest 的 Go 代码未变，不把历史运行改标为集成头重新执行。

验收 transport 是每个新沙箱一次受信 `kubectl exec`，尚未连接实际 Pod 控制器、能力/模型网关、平台发布或 SaaS checkpoint。这些部分、真实 Skill 沙箱加载、取消和资源/网络对抗仍属于 [INT-71](https://linear.app/intsheep/issue/INT-71)；应用门禁继续关闭。真实模型继续按用户安排后补。
