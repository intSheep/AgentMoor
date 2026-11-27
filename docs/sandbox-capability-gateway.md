# 独立能力网关

[INT-77](https://linear.app/intsheep/issue/INT-77) 提供专用 TLS Handler，与平台 API 分开。它只接受三条路由，guest 使用本回合的临时能力；平台身份、Kubernetes 身份及共享上游模型密钥不交给 guest。

| 路由 | 权限与限制 |
| --- | --- |
| `GET /input` | input capability；返回已封存的完整输入，带持久 SHA256/大小。Guest 在执行前仍须验证整个包。 |
| `PUT /output` | output capability；最多 64 MiB，按声明大小及 SHA256 完整校验和封存。重复上传须读到 EOF 后才返回同一 204，不重新执行。 |
| `POST /v1/responses` | model capability；真实作用域、profile、当前任务、截止时间与撤销均检查，再按固定模型策略预留持久预算。 |

其它方法、路径、查询、重定向及主 API 路径拒绝。TLS 服务构造器只接受本组件，不能传入平台 Handler；请求头、JSON、归档及响应大小各有硬上限。代理不继承环境代理，上游认证只使用受信密钥文件，guest 的认证、目的地和其它头不向上游透传。密钥文件可轮换，预算不因此重置。

## 首版模型范围

只支持无服务端状态引用的文本、reasoning 原文携带、function/custom 调用与 namespace 函数定义。不接受多模态、托管工具、云端会话/文件/容器/向量库/托管 prompt 等引用；普通文本 URL 和本地函数参数仍是数据。强制 `store=false`、`background=false`、默认服务等级和受信输出 token 上限（最多 16,384），拒绝重复 JSON key、超深结构及超大请求。

上游请求固定为配置里的 HTTPS `/v1/responses`，转发前先扣[持久次数/字节额度](sandbox-model-budget.md)。每次响应最多 4 MiB；失败不退款，不自动重放上游 POST。guest 自带的缓存键/客户端元数据被移除，缓存身份由平台作用域与 run 派生。

SSE 只转发核验过的事件和 JSON 数据，并重新编码事件帧，不原样转发评论、响应头或错误正文。实际源码是固定 Codex `0.171.0-alpha.13` 三轮请求采集后的合成结构 fixture，不把当前上游主分支当作固定版本验证。

## 撤销和资源边界

输入、输出和模型流共用原回合截止时间，并持续复查当前能力。撤销、到期或结果封存会中断相应活动流和真实套接字。正常结束也不会把 socket deadline 延长到原期限之后。

并发上限为每个 Handler 全局 32、每 profile 的输入/输出合计 2、模型 1–4；最多 1,024 个 profile。这是当前单实例内存闸，不是跨实例、握手前连接数或租户公平性保证。生产网络策略、节点/Pod 限额和耗尽验收另行推进。

上游 API 是受信的凭据接收方。网关不把密钥放进模型提示或 guest 请求；它也不宣称能过滤恶意上游在正常文本中任意编码、跨事件分片等隐蔽信道。

## 2026-11-27 验证

实现 `b35d721c8795e3906b2eccfddafe503cd4c56ca7` 的 fmt/check/demo 与真实 TLS 集成 race 通过，独立审阅无剩余 blocker。[证据](evidence/sandbox-capability-gateway-2026-11-27.json)

测试包括实际上下游 TLS、持久预算重开/并发、2 MiB 上传收到后丢 TCP 应答且重投不重执行、64 MiB 阻塞下载撤销、慢上传/模型 SSE 截止和撤销、结果封存取消模型、损坏 backing object 不执行、上游错误/重定向/断应答不重放及不反射。审阅发现混合 SSE 评论可能携带密钥，旧实际 TLS 负例复现失败；改为重编码核验后的 event/data 后，同一负例通过。另一处 TLS close-notify 会延长阻塞写等待，已用撤销后的持续 deadline 中断并实测。

合入控制器后的 `38faae999388b1725f547514716a344a5640ae83` 再次通过 fmt/check/demo。尚未连接 `serve`、实际 Codex 私有 CA 模型访问或 SaaS Worker；没有由本组件宣称网络/资源、真实模型或生产 HA 已通过，外层不可信任务门禁保持关闭。
