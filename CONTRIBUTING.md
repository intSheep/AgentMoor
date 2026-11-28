# 参与 AgentMoor

欢迎带一个真实业务场景来试跑。例如，让 SaaS 派给 Agent 一份 CSV，取回报告，删除执行 Pod，再向原会话追问一个只有读过报告才能回答的问题。

如果遇到问题，请在 GitHub Issues 写下 Agent 名称、CLI 版本、部署方式、期望结果、实际结果和最短复现步骤。用合成数据替换真实会话、客户文件和凭据。

## 试接入 Go SDK

从[独立模块起步包](examples/sdk-adapter/README.md)开始：它固定已发布的 0.x SDK，无需复制内部包或配置模型密钥，运行一个文件机制 Adapter 的公共资产测试。接口与预览兼容约定见 [SDK 指南](docs/adapter-sdk.md)。

请通过 GitHub 的“Adapter SDK 接入反馈”Issue 模板记录结果，也可以直接填写[模板文件](.github/ISSUE_TEMPLATE/adapter-integration.md)。记录确切版本、实际命令、实现者来源和遇到的阻碍。只试到某一步也可以提交问题；未执行项写清楚即可。真实外部接入反馈将帮助确定 1.0 接口，维护者与 agent 演练单独记录。

## 提交改动

1. 在 macOS 或 Linux 上按 [README](README.md) 配置环境，用 `make demo` 检查资产保存和恢复。
2. 提交前运行 `make check`。修改数据库或对象存储时，加跑 `make test-services`。修改任务接口、原生执行或跨 Pod 恢复时，加跑 `make test-k8s`。
3. 在 PR 里说明解决的场景、改动和实际运行的测试。新增适配器时，请附上 CLI 版本和对应的集群验收结果。

目录分工见 [AGENTS.md](AGENTS.md)。SaaS 请求示例、运行时设置和接入新 Agent 的步骤见[任务契约](docs/task-contract.md)。
