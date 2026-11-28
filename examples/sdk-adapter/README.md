# 仓库外 Adapter 起步示例

这个独立 Go 模块固定使用已发布的 `github.com/intSheep/AgentMoor v0.1.0-alpha.2`，没有 `replace`。它展示一个合成 Notebook 格式如何仅通过公开 `sdk` 和 `sdk/contracttest` 接入资产流程，不需要导入 AgentMoor 的 `internal/`。

这是维护者编写的起步包，不是真人第三方接入证据，也不表示支持一个新的厂商 CLI。它不执行模型或命令、不实现任务接口，没有 CLI/集群隔离支持声明。

## 先跑一次

需要 macOS 或 Linux、Go `1.27.4`、可用的 CGO/C 编译器和本地 HTTP 监听权限。完整契约会启动自己的 HTTP 与 SQLite 夹具；不需要 Docker、PostgreSQL、S3 或模型密钥。首次下载 Go 模块需要网络。

从 AgentMoor checkout 根目录把整个示例复制到仓库外的新目录，再独立运行：

```sh
adapter_example_dir=$(mktemp -d /tmp/agentmoor-sdk-example.XXXXXX)
cp -R examples/sdk-adapter "$adapter_example_dir/"
cd "$adapter_example_dir/sdk-adapter"
GOENV=off GOWORK=off GOTOOLCHAIN=local go list -m -json github.com/intSheep/AgentMoor
GOENV=off GOWORK=off GOTOOLCHAIN=local CGO_ENABLED=1 go test -mod=readonly -race -count=1 -v ./...
```

第一条 Go 命令应报告 `Path` 为 `github.com/intSheep/AgentMoor`、`Version` 为 `v0.1.0-alpha.2`，且没有 `Replace`。测试应真正执行并通过：

- `TestNotebookRegistryAndDiscovery`：注册、专属工作区标记及 Skill 发现；只有共享 Skill 目录时不识别为 Notebook。
- `TestNotebookAssetContract/notebook-files`：公共资产 suite，经 HTTP 验证 Session、Skill、Workspace、Artifact 原字节往返、固定旧版本、新路径恢复与公共拒绝反例。日志应含 `ADAPTER_CONTRACT mechanism=filesystem`、`four_assets=true old_versions=true`。
- `TestNotebookRejectsInvalidSessions`：坏 JSON、缺失/非法 ID、过大文件、越界符号链接及目录条目超限均拒绝。
- `TestNotebookCancellation`：在调用前已取消时返回取消错误；这不是执行中 CLI 取消验收。

结尾应有 `PASS` 和 `ok example.com/agentmoor-notebook-adapter`；仅编译成功或 `no tests to run` 不能算完成。测试后可移除刚创建的示例目录：`cd /tmp && rm -rf "$adapter_example_dir"`。

若下载失败，先检查网络与 Go 代理配置；若出现 C 编译器或 SQLite 错误，检查 `CGO_ENABLED` 与 C 工具链；若本地 HTTP 监听被拒绝，在允许 loopback 监听的开发环境运行。不需要用模型凭据解决这些错误。

## 换成自己的格式

`adapter.go` 只处理 Notebook 自己的规则：`.notebook/marker`、一个固定 Skill manifest，以及分配的 Session 根中的 JSON 文件。Session 身份来自 `session_id` 字段而不是文件名；示例只允许字母、数字、下划线和连字符，最多 128 字节。它最多扫描 32 个目录条目，每个 Session 最多 64 KiB；扫描到的 JSON 条目必须是普通文件，符号链接被拒绝。未知 JSON 字段与空白保持原始文件字节，不重新编码。

实现自己的格式时替换这些规则、ID、版本与 `Prepare` 夹具，声明实际支持的能力。`TaskModes` 和 `TestedRuntime` 在本例中为空；不要为了通过编译添加未实现的任务方法。资产保存和恢复由公共 runner 检查，不由 `Prepare` 返回通过标记。

SDK 当前仍是 0.x 预览。固定版本并查看[SDK 指南](https://github.com/intSheep/AgentMoor/blob/main/docs/adapter-sdk.md)后再升级；此模块测试的是已发布版本，不是 checkout 中尚未发布的 SDK 改动。测试使用合成数据；不得把真实会话、客户文件或凭据提交到仓库。

跑通后按[独立接入报告模板](https://github.com/intSheep/AgentMoor/blob/main/.github/ISSUE_TEMPLATE/adapter-integration.md)记录自己的版本、实际命令、结果和遇到的阻碍。示例的通过记录不能代替你自己的能力验收。新增真实 CLI 任务能力时，还需要按[任务契约](https://github.com/intSheep/AgentMoor/blob/main/docs/task-contract.md)与仓库规定完成固定版本原生往返及集群测试。
