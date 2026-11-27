# 固定模板的沙箱 Pod 控制器

[INT-74](https://linear.app/intsheep/issue/INT-74) 在[持久执行围栏](sandbox-execution-intents.md)之上增加可信 `sandboxkube.Controller`。部署配置固定范围、命名空间、镜像摘要、gVisor RuntimeClass、网关、模型和资源；不接受任务自带 Pod 模板、命令、环境变量或挂载。

本组件尚未连接 `serve` 或 SaaS Worker。当前不可信执行门禁仍关闭。下面的实际集群测试使用明确标记的合成 guest 与网关，只验控制器，不冒充真实 Codex 或完整生产模型网关。

## 创建与恢复

创建许可、命名空间和完整模板摘要先落库，然后只发一次 POST。超时、错误或控制面重开后，只查询该持久身份，找不到也不再创建。可能出现零次执行，不能称为恰好执行一次。

恢复会核对完整模板、历史能力摘要/用途和原 Pod UID，不能只信标签。只接受有限、明确的 Kubernetes 默认字段；模板漂移会停止授权。正常退出、封存输出和按原 UID 删除并确认不存在，三者齐备才返回 Ready。取消先撤销权限，再用独立短期限清理整个 Pod；回收后的墓碑仍可记录只增的清理结果，不能复活创建或能力。

单个坏 run 不阻塞其他 run 的恢复。配置更新后仍按已持久化的原命名空间和模板处理已有 Pod，不用新命名空间的 404 冒充旧 Pod 已消失。

## 部署方需要准备什么

| 项目 | 首版参考约束 |
| --- | --- |
| 节点运行时 | 固定官方 gVisor systrap，并实际验证 CRI handler、同 sandbox Sentry 和工作挂载的原子不覆盖语义。配置 Sentry `overlay2=all:memory,size=512m`；仅 Memory emptyDir 不够。[已验证来源](native-guest.md) |
| RuntimeClass | `agentmoor-gvisor` 指向 `runsc`。本模板要求不额外注入 overhead/scheduling；其它 admission 改动也必须通过模板核验。 |
| PriorityClass | 固定 `agentmoor-sandbox`，value 0、globalDefault false、preemptionPolicy Never。不能只在无 PriorityClass 的 Pod 上填 Never，否则实际 v1.35 admission 会拒绝。 |
| 镜像与文件 | 镜像必须用真实 manifest `@sha256` 固定。首版 Codex；CLI、公有 CA 和可选工具预装在只读镜像路径，不放进可写卷、proc/sys/dev。 |
| Guest | UID/GID 65532、只读根、drop ALL、无提权、RuntimeDefault seccomp；Never、原回合截止时间；只挂有限工作/临时内存卷和只读 DownwardAPI 能力文件，不自动挂 SA token。 |
| 网关 | 固定 HTTPS 名称与 IP，由 hostAlias 解析，独立 TLS 端口。模型客户端也须信任该 CA，不能只配置 Go 传输证书。 |
| 权限 | 可信控制器只获沙箱命名空间 Pod get/create/delete；没有 exec/attach/log、Secret、SA token、Job、RBAC 或节点权限。 |
| 网络与配额 | 部署方另建默认拒绝及仅网关出口的可执行网络策略、准入和配额，并用真实流量/资源耗尽验证；此组件不据 YAML 或 limits 宣称这些已验收。 |

[最小参考资源](../deploy/kubernetes/sandbox-rbac.yaml)由 operator 配置，不由控制器创建。控制面命名空间须先存在，按实际部署修改其中的 namespace/ServiceAccount 绑定，并核对账号在整个集群的实际权限。

## 实际集群证据

```sh
python3 tests/kubernetes/sandbox_controller_smoke.py \
  --output /absolute/new-controller-evidence.json

# 也可加 --bundle /absolute/gvisor-aarch64.tar.bz2，仍校验固定官方摘要。
```

依赖沿用 [gVisor 探针](gvisor-feasibility.md#怎样运行)，另需 Go 与 OpenSSL。证据文件须尚不存在。夹具只创建自己的临时 kind 集群、测试角色/凭据、合成镜像和本机临时 TLS 网关，最后删除自有集群与私有凭据；保留脱敏诊断目录，不修改已有集群。

源码 `d0ca9734ba36e8eaaf740b8ac12b1af2ba8e65ff` 的第二次实际运行通过：[完整原始结果](evidence/sandbox-pod-controller-2026-11-27.json)。正常、取消和缺 handler 三种模式各只有一次 POST；重复 Start 和新建 Controller 对象均不换 UID 或补建。正常模式封存完整结果、退出、按 UID 删除并观察 Gone 后为 Ready；取消模式读过一次输入、没有输出，清理后为 Cancelled；缺运行时模式明确两次拒绝、没有 CRI sandbox、输入/输出均为零。

正常和取消模式实际匹配 runsc/systrap、512m Sentry 内存覆盖层；每轮清理后额外验证 Pod Gone、无存活 Sentry、无 Ready CRI。服务账号真实权限矩阵为 3 项允许、9 项拒绝；镜像用实际 OCI manifest 摘要固定。测试凭据与集群均清理，证据没有能力令牌明文。

第一轮因 RBAC 夹具错误使用 `kubectl auth can-i get pods/log` 而失败；该命令检查的是名为 log 的 Pod。改用 `--subresource=log` 后通过，第一轮尚未运行 controller guest；[原失败记录](evidence/sandbox-pod-controller-failed-2026-11-27.json)保留，不把它解释成角色泄漏。

合入最新模型预算后的源码 `214fdeba1559df2fe0c498aadc917eb02c5ce87d` 的 fmt/check/demo、真实 PostgreSQL/S3 和完整四 CLI 默认 `make test-k8s` 均通过。[四 CLI 回归摘要](evidence/sandbox-pod-controller-regression-2026-11-27.json)保留日志摘要与专属集群/镜像清理确认；该矩阵仍是受信文本路径，不重标第二轮组件验收的来源。

真实模型、原生 CLI/Skill、生产网关、网络对抗、资源耗尽、SaaS 发布与生产 HA 均不在此组件的通过范围；真实 Codex 三 Pod 文件交接另由[原生核心验收](native-guest.md)证明。
