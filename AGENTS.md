# AgentMoor 协作说明

默认用简单、好懂的话与用户交流。先说结果，需要时用一个具体例子解释。用户追问时再展开技术细节。

## 在哪里改

| 位置 | 职责 |
| --- | --- |
| `cmd/agentmoor` | CLI 参数、平台启动、注册内置适配器 |
| `internal/adapter`、`internal/adapters/<runtime>` | 能力接口、按固定版本识别环境、读取运行时自己的格式 |
| `internal/adapters/workspace`、`internal/adapters/sessionfile` | 共用目录扫描和原生会话标识读取 |
| `internal/kubejobs`、`deploy/kubernetes` | 将持久化的追问调度为独立 Job，保持受信 Pod 模板、命名空间权限和幂等 Job 身份 |
| `internal/task` | SaaS 任务的数据格式、状态及可选运行时执行接口 |
| `internal/asset` | 平台与 Worker 共用的身份、版本、元数据与操作格式 |
| `internal/worker` | 授权后发现资产、冻结待发内容、同步与恢复、持久游标 |
| `internal/snapshot` | 有界打包、校验、分阶段恢复、无覆盖发布 |
| `internal/platform` | HTTP 鉴权、元数据事务、不可变对象存储 |
| `tests`、`testdata` | 真实 CLI 往返测试、独立存储服务测试、合成工作区 |

## 修改时保持的约定

- 新增 Agent 时在自己的包实现所需的能力，在 CLI 入口注册。声明实际支持的操作、模式和验证过的版本。共用流程通过接口调用适配器。
- 保留 Session 和 Skill 的原始内容。目录规则、文件字段、原生 CLI 参数属于对应适配器。工作区发现依赖专属标记，共享 Skill 目录只用于候选发现。
- 在受信的控制面分配范围里读取资产。通过 `os.Root` 处理工作区相对路径。凭据、平台目录、Worker 游标独立于 Agent 工作区。原生 CLI 只继承明确允许的环境变量。
- 上传冻结后的字节，校验通过后再提交元数据和游标。发布与重试共用操作键。目标版本变化时保留双方的内容，等待明确选择。
- 恢复目标必须是新路径。先在同一文件系统的临时位置完成校验或导入，再原子发布。保持读取旧版本、重复投递和错误恢复的语义。

## 怎样确认改动有效

运行 `make fmt`、`make check` 和 `make demo`。改变数据库或对象存储行为时运行 `make test-services`。改动原生会话格式或原生命令时，用对应版本执行 `python3 tests/native_roundtrip.py <adapter> <可执行文件绝对路径>`，并更新[验收矩阵](docs/mvp.md)。本仓库只提交合成数据。

设计依据、启动方法及完整命令见 [README](README.md) 和[运行手册](docs/operations.md)。

接入一个新 CLI 时，先用 `make test-k8s AGENT=<适配器 ID>` 跑通报告交付、执行中取消、SaaS 触发新 Pod 后按原会话 ID 连续追问两次，再接入下一个。改动任务接口、原生执行或集群交接后，用 `make test-k8s` 跑完整矩阵。SaaS 测试客户端只使用公开 HTTP 接口，运行时设置和原生命令放在各自适配器或集群测试配置里。[任务契约](docs/task-contract.md)
