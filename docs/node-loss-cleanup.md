# 节点失联时的沙箱停止证明

INT-90 修复了一个真实误判：Kubernetes API 返回 Pod 不存在时，客人程序可能仍在失联节点里运行。平台现在分别保存“受信运行时已报告 guest 停止”和“原 Pod API 对象已删除”，不会再用后者代替前者。保持原 Run、attempt、UUID、截止时间与旧成功检查点，不自动换节点重新执行。

## 两个事实与最终锁

新建的固定 Pod 模板带 `agentmoor.io/guest-stop` finalizer。控制器撤销本轮能力并请求按原 UID 删除后，保留对象，直到取得下表中一种停止证明。控制器仅有自己命名空间的 Pod get/create/delete/patch；客人不持有此权限。

| 证明 | 验证与提交顺序 |
| --- | --- |
| `terminated.v1` | 原 Pod UID、资源版本、冻结的节点与 `containerd://` 容器身份；唯一 `guest` 的当前 `state.terminated`、restartCount=0 和真实退出码。先持久化证明、撤销权限和审计，再条件移除本平台 finalizer，最后确认原 API 对象不存在。 |
| `never-scheduled-cas.v1` | 删除时间已存在、nodeName 仍为空、无容器状态；以 UID、resourceVersion、删除时间、完整 spec 和 finalizer 列表做精确 JSONPatch CAS。只有 PATCH 的成功应答返回后才保存证明。 |

每次 PATCH 只移除本平台的一个 finalizer，保留其它控制器的 finalizer。待调度 Pod 的 CAS 同时防止读取到空节点后被调度器绑定；Kubernetes 1.35 的 [BindingREST 实现](https://github.com/kubernetes/kubernetes/blob/v1.35.0/pkg/registry/core/pod/storage/storage.go)也在同一存储更新中拒绝绑定正在删除的 Pod。PATCH 应答丢失时，未调度路径保守保持未知，不把一次无法确认的请求当成证明；真实终态路径可用此前已提交的证明恢复后续删除确认。

`PhaseFailed`、`PhaseSucceeded`、NodeReady 和 GET 404 都不单独提供停止证明。特别是 Kubernetes [PodGC](https://github.com/kubernetes/kubernetes/blob/v1.35.0/pkg/controller/podgc/gc_controller.go)可以在没有 kubelet 终态的情况下将 phase 改成 Failed。任务失败分类与 guest 停止证明是两个不同字段；infra 失败不会因一个退出码为 0 的片段变成成功交付。若运行时报告的节点或容器身份与冻结绑定冲突，旧证明也不能继续授权读取、发布、确认删除或 GC。

证明表示受信运行时报告**这一 guest 执行已经停止**，不从一个 Kubernetes 字段推导所有 Sentry、gofer、shim PID 或节点资源均已释放。本次固定组合另有独立 CRI/进程自然回收观察，不增加通用硬件 fence 或生产多节点 HA 承诺。

## 发布、保留与旧记录

Worker 下载封存输出、公开 HTTP 首次成功 Finish 的服务端来源检查，以及新的原始 sandbox 对象 GC 都要求有效停止证明。成功输出还必须是 `terminated.v1` 且真实退出码为 0，并同时满足原先封存、状态、Pod 删除、profile、claim 和原始 deadline 检查。execute 凭据直接发 Finish 也不能跳过这条规则。

`sandbox.runtime_bound`、`sandbox.guest_stop_proven` 和 `sandbox.cleanup_unconfirmed` 使用现有固定字段与可信 Actor，和对应 Run 修改、能力撤销在同一持有写租约的事务里提交。审计失败或写租约失效则整笔回滚；相同证明的资源版本重放保留首次证明，不额外增加事件。日志与审计不复制 capability、原始 Pod、终止消息、任务正文或原始错误。

`platform-audit.v1` 继续读取此前事件；旧版严格读取器遇到新增 action 会拒绝，运维读取程序应与写入端一同升级。

没有新证明的旧 JSON 行仍可读取和列举，不回填虚构历史，不改变已经成功的 Finish ACK 或历史资产版本读取。旧 `PodTerminal`、`PodDeleted`、`Ready` 或 `Collected` 标记也不会被自动补成新证明。新的保护操作拒绝无证明历史，不能因一个旧 Run 阻止其它 Run 的协调。

如果对象因管理员移除 finalizer、旧部署或其它越界操作而消失，但未保存证明，控制器设置 `CleanupUnknown`、撤销能力、保留执行 fence 和私有对象。后来的 NodeReady、API 404 或重复 Start 都不会使它变成已清理，也不会重新 POST 旧任务。即使已有旧证明，标记 Unknown 后该证明也失去授权效力，不能靠重放清除 Unknown。

运维应优先恢复原 kubelet，让保留的 Pod 得到真实终态后由控制器完成删除。无 API 终态的历史未知记录没有自动“修好”或手工补造证明的公开接口；需在可信运维侧查明原节点和执行是否停止，再按部署恢复政策处理。本项不提供自动跨节点接管、强制清除锁的快捷命令或特权宿主回收器。旧成功检查点和精确资产仍可读取，不能把重投原 UUID 当作再次执行的办法。

参考 [ResourceQuota](../deploy/kubernetes/sandbox-quota.yaml)增加 `count/pods`。普通 `pods` 额度可在 phase 变 Failed 后减少，而 `count/pods` 仍计入被 finalizer 保留的对象。对象额度可以防止这一情形绕过对象并发限制，但不等于跨节点物理 CPU/内存总账。物理限制仍依赖已验证的每 guest cgroup、NoSwap、受信节点配置及对残留的保守处理。

## 复验与实际结果

```sh
make fmt
make check
make demo
make test-services
make test-k8s

# 仅创建全新的专属 kind 节点；不对已有集群注入故障。
NODE_LOSS_OUTPUT=/absolute/new/evidence.json make test-k8s-node-loss
```

`GVISOR_SECURITY_BUNDLE`、`GVISOR_SECURITY_CALICO_MANIFEST` 和 `GVISOR_SECURITY_CONTAINERD_CACHE` 可指向已经下载的固定包/清单/缓存目录，仍强制校验官方摘要。不提供 `NODE_LOSS_OUTPUT` 时只打印摘要，证据路径必须是尚不存在的新文件。节点探针使用官方 containerd 2.4.2、gVisor `release-20261123.1`/systrap 和 Calico 3.33.0；只在其创建的节点上配置运行时与 CNI，只停止该节点 kubelet，API 与 containerd 保持运行。finally 恢复 kubelet并删除专属节点、本轮镜像标签与私有临时凭据。

旧源 R2/R3 保留为失败证据：GET 404 后，旧 Catalog 已写 PodDeleted=true，但同一 sandbox/container/Sentry 启动身份和真实单 child 仍发出至少三条新心跳。不是暂停整个节点，也不是用网关断连代替程序证据。R3 另保留 Pending、正常取消与失联的真实 API 状态序列。

修复后的 R5 在冻结源码 `bc9b29f7e37fd7363cb69dc5daf71f18ff5e0962` 通过四场矩阵：

| 场景 | 实际观察 |
| --- | --- |
| 从未调度 | cordon 后由调度器确认 Pending/noNode/noCRI；设置删除后，有权限的管理员绑定请求返回 409；精确 CAS 后保存 never-scheduled 证明并删除。 |
| 正常取消 | 唯一 guest 真实 terminated143/restart0；先保存 term 证明，再释放 finalizer；API 消失且独立观察无同归属 Sentry/gofer/shim、Ready CRI 与 Running child 容器。 |
| kubelet 失联 | UID force-delete 被 finalizer 保留；同一 child seq6→9；管理员模拟 PodGC 仅改 phase=Failed，容器状态仍 Running，同一 child seq9→12。两窗 GuestStopped/PodDeleted 仍 false；恢复 kubelet 后得到真实 term143 证明才完成删除和自然回收。 |
| 管理员越过 finalizer | 仅测试管理员移除 finalizer 并按 UID 删除；API 404 后真实 child 仍运行，Catalog 保持 CleanupUnknown/PodDeleted=false。恢复 kubelet后自然回收，但 Catalog 仍不虚造缺失的 API 终态。 |

每个活跃场景先验证相同受控目标可达：另一个不被策略选中的普通容器持续连到同一双端口 gateway Service、Kubernetes Service 443、另一租户夹具 8123；gateway 7443 使用真实 CA/TLS 校验，同一进程监听 7443 与 7401。隔离 guest 心跳每轮直接 TCP 连接；故障前正向 7443 可达，7401/443/8123 超时，两个保留对象故障窗口又各有至少三条新的同进程心跳和同目标可达对照，未出现网络放开。没有访问真实公网或云 metadata。

此组件使用合成 host TLS input handler，策略额外放行**一个固定 host IP 与本轮精确端口**；这是测试例外，不写入生产 NetworkPolicy。有限目标的通过不能推导任意 Internet 隔离。合成 child 不是 vendor CLI；本探针不替代实际 Codex、模型、Skill、SaaS 提供方验收。

R5 中 PhaseFailed 且 finalizer 保留时，`pods=2` 而 `count/pods=3`，新 Pod admission 返回 403 并指明 count/pods；真实删除后 count/pods=2，同一额度下新见证对象返回 201，分配不存在节点确保无 CRI 执行。全部四模式控制器原 POST 数均为 1，旧检查点、身份与原 deadline 不变。本轮专属节点、标签、私有临时凭据已清理；公开固定依赖缓存明确保留。

独立源码 `3564b831ab536fc8bd5d5426545a102b7a82de22` 另通过真实 Codex 的生产 SaaS 文本三轮与运维审计链：三个新 gVisor Pod 沿原生会话接续，11 份固定资产重新下载字节相同，原 UUID 回放不增执行或事件。73 条事件包含六条 runtime 绑定和三条 guest stop-proof；独立运维只读 SQLite 白名单观察取得三个真实 termproof exit0，UID/node/CID 与运行时观察一致，封存输出 SHA 与 accepted 一致，并核对 terminated/stop-proven < UID-deleted < result-accepted 顺序。[实际原始链路](evidence/node-loss-native-audit-2026-11-27.json) · [白名单状态观察](evidence/node-loss-native-metadata-2026-11-27.json) · [交叉核对与实际清理](evidence/node-loss-native-verified-2026-11-27.json)

该原生回归仍用脚本模型与合成数据，没有真实模型密钥；仅文本分配，没有由此扩成文件/Skill 全组合或 PostgreSQL/S3 的完整隔离链路验收。本次节点、outside 容器及三个本轮标签已独立 Docker inspect 确认不存在，公开 CLI 依赖缓存保留。运维观察器/核对器的原始单次脚本以文本证据保留，内含当时专属节点路径，不作为通用生产运维命令。

同一 `3564b83` 的 fmt/check/demo、真实 PostgreSQL/S3 services 和完整四 CLI 默认受信文本 `make test-k8s` 均退出0；16组契约、8次追问通过，原生取消分别4.5/3.0/3.7/3.8秒，临时节点与四个本轮镜像标签已独立确认清理。完整来源、原始文件摘要、精选反例与代码级验证见[证据索引](evidence/node-loss-cleanup-2026-11-27.json)。R5 原始记录保持原 source；`3564b83` 相比它只修正旧 gateway 审计成功单元夹具，不改生产或故障脚本，不把 R5 改标签到新提交。后续主线组合验收另列来源。

合入 INT-91 完整头 `4c2462d` 后，组合源码 `c383c8525c024d7e9fd55a05dbeab1a01f19b7dd` 再次通过全部五项。真实 PG/S3 同时运行 guest stop-proof 与期限巡检检查，43.865秒通过；四 CLI 的16组/8次追问通过，取消分别3.0/3.1/3.8/2.3秒。专属节点、四运行标签及存储服务容器/网络已独立核实不存在。[本轮原日志压缩件](evidence/node-loss-combined-checks-2026-11-27.log.gz)保留完整原始字节的摘要，未将早期故障或原生验收改成此组合来源。
