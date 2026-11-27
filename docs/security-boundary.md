# 不可信代码的执行边界

2026-11-27，产品决定：允许客户提交自己的代码和第三方 Skill，统一按不可信代码处理。它们可以读写分配给自己的工作区，不能因此得到平台身份、可信 Worker 状态、其他租户数据或宿主权限。

当前开发路径会在 Worker 容器里直接启动原生 CLI。环境变量白名单、独立 HOME、`os.Root` 和非 root 用户各自有用，但这条路径仍共用 Worker 的进程权限和挂载，尚不能承载上述不可信执行承诺。现有真实 CLI/文件/Skill 验收使用合成输入与受信工具，不是恶意程序逃逸验收。

## 必须分开的两侧

| 位置 | 持有内容与职责 | 不交给另一侧的内容 |
| --- | --- | --- |
| 可信控制面与 Worker | 授权、固定输入版本、恢复/校验、发布资产、维护操作身份与成功 checkpoint | 平台签名密钥、Write/Execute token、PG/S3 凭据、Worker journal/游标及其他分配状态 |
| 不可信执行区 | 本次原生 CLI、客户代码、第三方 Skill、自己的工作区和原生会话文件 | 不授予控制面身份、宿主挂载、其他租户目录或共享平台模型密钥 |
| 执行区外的模型网关 | 按租户/attempt 限权的模型访问、时限和取消 | 平台共享的上游模型密钥不放入 guest |

分离后，输入和输出必须有大小、路径、类型和摘要限制；guest 返回的数据再次校验，不能用它改写可信状态或选择任意宿主路径。Session 和 Skill 继续保留原生内容与格式。原生命令、会话识别及导入/导出规则仍由各适配器负责，不能在共用执行层拼装某一家 CLI 的参数。

执行策略属于受信部署配置。任务文本、模型回复或 Skill 不能把自己标成受信、选择可执行文件、改变 RuntimeClass 或扩大网络/挂载范围。本地直接执行保留给受信开发与批准工具；它不是不可信任务的退路。没有可用的隔离提供方、运行时或网络策略时必须拒绝运行。

## 首个参考运行时

2026-11-27 用户进一步确定：首版生产目标是可自行配置的 Linux Kubernetes 集群，部署方可以安装和配置 gVisor。先以 gVisor systrap 接入可复验后端；后续如果部署需要硬件虚拟机边界，再按节点条件接入相应提供方。gVisor 的系统调用隔离仍允许程序访问主动分配的文件和网络，因此不能把 Worker token 挂进沙箱后就认为它已被保护。资源上限与网络访问也需要各自的实际验证。[gVisor 安全模型](https://gvisor.dev/docs/architecture_guide/security/) · [平台条件](https://gvisor.dev/docs/architecture_guide/platforms/)

Kubernetes 的 RuntimeClass、Pod 限制、RBAC、网络策略与资源额度共同组成部署约束。普通容器限制或只有一份 YAML 不构成强隔离实证；须确认实际使用指定沙箱，并运行允许/拒绝流量与恶意程序探针。[Kubernetes 多租户说明](https://kubernetes.io/docs/concepts/security/multi-tenancy/)

## 需要逐项取得的证据

- 缺失或错误的隔离配置不能启动原生命令、领取任务、改写 checkpoint 或发布成功资产；禁止静默回退。
- guest 不能读取平台 token、可信 Worker 状态、其他租户数据或宿主文件；上游共享模型密钥不出现在 guest。
- 自己的必要代理可达；控制面、其他租户与 metadata 地址按策略拒绝。检查实际流量，而非仅检查标签或规则对象。
- CPU、内存、磁盘、并发与执行时间超限可判定；取消和实例丢失不会被误报为成功。
- 真实 Codex 文件与 Skill 工作流在沙箱运行，两个新 Pod 继续同一原生会话，旧版本、原 UUID 幂等、失败后的成功 checkpoint 保持原语义。其余 CLI 按同一矩阵逐个扩展。

凭据轮换和单凭据撤销属于独立防线。撤销阻止之后的鉴权请求，不会自动终止已接收的请求，也不撤销外部副作用。

## Roadmap 进度

| 工作 | 当前范围 |
| --- | --- |
| [INT-68](https://linear.app/intsheep/issue/INT-68) | 已合并：当前 22 条 HTTP 路由的角色及四维作用域矩阵。没有由此宣称代码隔离。 |
| [INT-69](https://linear.app/intsheep/issue/INT-69) | 已实现：可信控制面的密钥轮换、单凭据撤销与受信管理入口；[实际验证](authentication-policy.md#2026-11-27-实际验证)。 |
| [INT-70](https://linear.app/intsheep/issue/INT-70) | 已实现任务首轮/追问的显式策略与拒绝门禁；[运行手册](operations.md#任务执行的信任策略)。普通原生维护入口和真正 provider 仍待后续，门禁不是沙箱。 |
| [INT-71](https://linear.app/intsheep/issue/INT-71) | 进行中：gVisor systrap 的[独立可行性命令与正反例已通过](gvisor-feasibility.md)，真正执行区分离、模型网关、网络/资源及真实 CLI/Skill 对抗验收尚未完成。 |
| [INT-73](https://linear.app/intsheep/issue/INT-73) | 已实现[私有输入/输出与执行围栏的持久存储](sandbox-execution-intents.md)，真实 PostgreSQL/S3 故障测试通过；尚未连接控制器或开放不可信执行。 |
| [INT-72](https://linear.app/intsheep/issue/INT-72) | 已实现[有界原生 Guest 核心](native-guest.md)，真实 Codex 在三个新 gVisor Pod 中沿同会话完成文件生成、修改与核验；仍未接入生产控制器/网关或开放不可信任务。 |
| [INT-76](https://linear.app/intsheep/issue/INT-76) | 已实现[模型调用前的持久次数/字节预留](sandbox-model-budget.md)，并发、重启与真实 PostgreSQL 租约丢失测试通过；模型代理及计费/并发流控制另验。 |

各项完成时更新对应实际源码、命令、镜像和结果。真实模型按用户安排后补；不以本机沙箱成功宣称生产 HA、所有系统调用兼容或硬件侧信道防护。
