# AgentMoor

**v0.1 技术预览 · [MIT 许可证](LICENSE)**。欢迎用自己的 SaaS 任务试跑。当前验收使用本地 kind 集群、真实 Agent CLI 和模拟模型端点，覆盖任务交付、运行中取消、连续两次换 Pod 后续聊。接入真实模型时，请记录所用的 CLI 和模型版本，方便复现。[验收记录](docs/mvp.md) · [参与贡献](CONTRIBUTING.md)

AgentMoor 是业务 SaaS 和原生 Agent 之间的中间件。它能接收任务并把 Agent 的会话、Skill、交付文件和工作区保存到平台。一个运行实例销毁后，可以在新实例恢复工作区，再用原来的会话 ID 继续工作。

首批支持 Claude Code、Codex CLI、Gemini CLI 和 OpenCode。每个适配器负责读懂对应 Agent 的数据，平台统一处理归属、版本、校验和及访问权限。会话以原始格式保存。

四种 Agent 共用一套 SaaS 任务接口。SaaS 通过 HTTPS 提交、查询、取消任务，按固定版本下载交付物。一次性 Kubernetes Pod 负责执行。启用续聊调度后，原 Pod 删除也可以追问。SaaS 提交新问题，AgentMoor 自动拉起新 Pod 恢复会话，再把答案作为可下载的交付物交回。运行 `make test-k8s` 可重现完整流程。接口和示例见 [SaaS 任务契约](docs/task-contract.md)。

## 跑通第一个例子

在 macOS 或 Linux 上准备 Go 1.27.4、C 编译器、Make 和 Python 3，然后在仓库根目录运行。

```sh
make demo
```

已有构建好的 `bin/agentmoor` 时，可以直接运行 `sh examples/demo.sh`。

演示会在临时目录启动本地平台，保存四类示例资产，重复同步一次，再恢复工作区并比较文件内容。成功时会看到四条 `v1` 记录和 `Workspace restored and verified.`。演示退出时会清理临时数据。

要查看你自己的项目有哪些 Agent 和 Skill，可以直接运行。

```sh
./bin/agentmoor adapters
./bin/agentmoor detect testdata/codex
./bin/agentmoor skills codex testdata/codex
```

## 可以保存什么

| 资产 | 保存与恢复 |
| --- | --- |
| Session | 保留原始 Transcript、会话 ID 和适配器能识别的关联文件。文件型 Agent 可一次恢复整批会话。OpenCode 还能调用原生 CLI 导出、导入。 |
| Skill | 发现项目 Skill，把清单及同目录脚本、参考文件一起保存。登记名称、声明版本、依赖和管理来源，恢复到指定的新路径。 |
| Artifact | 扫描控制面指定的交付目录。每次内容修改发布一个新版本，历史版本可单独下载。 |
| Workspace | 保存目录树、普通文件、常规权限和受支持的相对符号链接。恢复时先校验 SHA-256，目标已存在时报告冲突。 |

同步可由运行时 Hook 调用 `sync`，也可运行 `watch` 监听变化并定期补扫。每个 Worker 只获得分配给自己的短期平台凭据。默认存储是 SQLite 与本地磁盘，也可使用 PostgreSQL 与 S3 兼容对象存储。

## 开发与验证

```sh
make check          # 格式、go vet、竞态检测、所有常规测试、编译
make demo           # 使用本地平台走通上传、重复同步、恢复
make test-k8s       # 用独立 kind 集群验证 SaaS、原生执行、取消和跨 Pod 恢复
make test-services  # 用 Docker Compose 验证 PostgreSQL 与 S3 兼容服务
```

真实 CLI 续聊测试需要对应版本的可执行文件。测试启动本机模拟模型，创建会话、移除源目录、恢复，再检查第二次模型请求是否包含首轮的随机标记。复现步骤见[运行手册](docs/operations.md)。

| 从这里开始 | 内容 |
| --- | --- |
| [运行手册](docs/operations.md) | 启动平台、配置 Worker、恢复各 Agent、处理版本冲突 |
| [v0.1 验收矩阵](docs/mvp.md) | PRD 条目、实测版本、测试范围和当前边界 |
| [适配调研](docs/research.md) | Magpie、CC Switch 和四种 Agent 的设计依据 |
| [AGENTS.md](AGENTS.md) | 仓库结构、扩展适配器的方法和提交前检查 |
| [原始 PRD](docs/PRD-v0.1.md) | 初稿归档，首批名单按本项目约定调整 |
