# Go Adapter SDK 预览

`github.com/intSheep/AgentMoor/sdk` 让仓库外的 Go 模块实现当前 Adapter 接口，不需要直接导入 `internal/`。这是源码预览：先固定一个 checkout，通过本地 `replace` 验证；尚未发布新的 SDK 版本，也未承诺 v1 API 稳定性或兼容维护窗口。

已有 `v0.1.0-alpha.1` 标签保持原样，不能用它或 `@latest` 代表本文的 SDK。本文的本地替换证明使用了指定 checkout，不证明远程版本下载、发布流程或外部工程师接入已经验收。

## 两个包各管什么

| 包 | 用途与边界 |
| --- | --- |
| `github.com/intSheep/AgentMoor/sdk` | 实现 Adapter 所需的公开类型、可选能力接口，以及 Registry、Inspect 和显式原生 CLI 调用工具。类型与当前内部契约保持同一身份，不复制另一套业务模型。核心依赖不包括 Worker、平台服务、SQL 或 S3。 |
| `github.com/intSheep/AgentMoor/sdk/contracttest` | 测试时复用[资产契约验收](adapter-contract.md)：准备合成 case，经实际 HTTP 和 Worker 检查四类资产。它会引入 Worker 和平台测试依赖，单独导入，不作为 SDK 核心的运行依赖。 |

当前工具链为 Go `1.27.4`。原生进程组控制与文件系统发布沿用现有 Unix 实现；当前面向 Linux/macOS，不承诺 Windows 或其它系统的可移植性。公开包没有自动注册、动态加载、RPC 或 Native Hook 协议。实现被编译进程序后，仍由受信启动代码显式注册。

公开名称与内部接口保持一致：

| 类别 | `sdk` 中的名称 |
| --- | --- |
| 描述与发现 | `Adapter`、`Descriptor`、`Capability`、`AssetKind`、`SkillRef`、`SkillDiscoverer` |
| Session | `SessionSource`、`NativeSession`、`Entry`、`NativeSessions` |
| 注册与观测 | `Registry`、`RuntimeInfo`、`NewRegistry`、`Inspect` |
| 可选执行接口及参数 | `Executor`、`Continuation`、`FileExecutor`、`SkillInstaller`、`Environment`、`Spec`、`Execution`、`LoadedSkill`、`RestoreHistory` |
| 标识常量 | 四种资产 `Session`、`Skill`、`Artifact`、`Workspace`；三种任务模式 `TextTurn`、`ContinueTurn`、`FileTurn` |
| 受信 CLI 工具 | `RunCLI`、`RunCLIInput` |

例如 `NativeSession.Entries` 可以直接写成 `[]sdk.Entry`，任务方法的全部参数也能用公开名称表达。`Inspect` 负责观测：版本不符时返回 `RuntimeInfo.Tested=false`，不单独把它当作调用错误；使用方仍须应用自己的版本门槛。

## 在独立模块使用指定 checkout

在仓库外建立一个目录，将下面的绝对路径换成实际 checkout：

```go
module example.com/my-agentmoor-adapter

go 1.27.4

require github.com/intSheep/AgentMoor v0.0.0

replace github.com/intSheep/AgentMoor => /absolute/path/AgentMoor
```

这里的 `v0.0.0` 是本地替换所需的占位版本，不是发布标签。模块路径大小写使用 `github.com/intSheep/AgentMoor`。从这个独立模块运行：

```sh
GOWORK=off go mod tidy
GOWORK=off go test ./...
```

`GOWORK=off` 避免本机工作区配置替代上述依赖关系。测试夹具还会关闭代理和校验服务、使用本机工具链与已有依赖缓存；普通开发环境可以正常准备所需依赖。不要直接导入 `github.com/intSheep/AgentMoor/internal/...`：Go 的内部包边界仍然有效。

一个只提供发现能力的最小实现可以写成：

```go
package myadapter

import (
    "context"
    "os"

    "github.com/intSheep/AgentMoor/sdk"
)

type Adapter struct{}

func (Adapter) Descriptor() sdk.Descriptor {
    return sdk.Descriptor{ID: "my-agent", Version: "0.1.0-preview"}
}

func (Adapter) Detect(ctx context.Context, root *os.Root) ([]string, error) {
    if err := ctx.Err(); err != nil {
        return nil, err
    }
    if _, err := root.Stat(".my-agent"); err != nil {
        if os.IsNotExist(err) {
            return nil, nil
        }
        return nil, err
    }
    return []string{".my-agent"}, nil
}

var _ sdk.Adapter = Adapter{}
```

这只证明实现了发现接口。没有声明资产或任务能力，也不意味着已经能够导出会话、执行任务或通过完整资产 suite。专属标记用于运行时识别；共享 Skill 目录只用于候选发现。

宿主程序可以通过 `sdk.NewRegistry(Adapter{})` 显式注册它。接入现有 AgentMoor CLI 时，也需要在受信启动代码中构造、注册后重新构建；当前没有安装后自动加载外部 Adapter 的命令。

## 增加能力与复用测试

按实际需要实现 `SkillDiscoverer`、`SessionSource` 或 `NativeSessions`；文本执行、继续执行、文件交付和 Skill 安装也保持可选。只声明已经实现并验收的任务模式。原生 Session ID、格式、关联文件和 CLI 参数由自己的 Adapter 解释，原 Session 与 Skill 内容保持原始字节。

`Descriptor.Version` 是适配器版本；`TestedRuntime` 是精确的 CLI 输出字符串，不是 SemVer 范围。`Capabilities` 的模式与可靠性字段是描述字符串，没有通用的高低排序。具体生命周期、版本门槛和完整接口说明见[当前适配器契约](adapter-contract.md)。

`sdk/contracttest` 公开同一套 `Case`、`Paths`、`Fixture`、`NativeFixture`、`Calls` 与机制类型，并提供 `Run` 和 `ValidateDescriptor`。case 的 `Prepare` 只负责自己的合成文件和 CLI 夹具；共用 runner 负责授权、HTTP、Scope、Worker 状态和公共断言。外部 case 不需要复制 Worker 或直接导入内部包，也不能替 runner 返回一个通过标记。

完整 runner 会启动本地 HTTP 与 SQLite 夹具，需要可用的 CGO/C 编译环境；无需另起 PostgreSQL、S3 或集群。单独使用 `sdk` 核心没有这项 SQLite 测试依赖。

Session 预期文件用相对路径映射表示，单一原始文件使用空 key。CLI 计数和夹具状态放在独立的 Runtime 目录，避免修改 Workspace 或 Session。使用原生机制时，必须真的经过自己的导出/导入方法；读取一个预先生成的 JSON 文件不能代替该机制的验收。

runner 使用 `t.Setenv`，它及上层测试不能调用 `t.Parallel`。其取消检查是调用前取消的恢复，不代表执行中 CLI 取消已验收。若只实现发现能力，可以使用 `ValidateDescriptor` 和自己的发现测试；完整资产 runner 要求 Session、Skill、Workspace 与 Artifact 的合成往返夹具。

## 执行仍有原来的信任边界

SDK 本身不会把一个子进程变成强隔离环境。普通同步、导入与显式 CLI 工具是受信运维入口，调用者对所选可执行文件、分配目录与显式环境负责。适配器应通过提供的 `os.Root` 处理相对路径，原生命令只获得明确允许的环境；不把平台凭据或 Worker 状态放进 Agent 工作区。

客户代码和第三方 Skill 继续按不可信代码，通过既有[隔离提供方](sandbox-provider.md)运行。公开可选任务接口不改变部署 Profile、执行策略、模型网关、停止证明或成功发布门槛，也不表示任意新增 Adapter 已经得到隔离任务支持。

本片的外部消费者由维护者编写，使用合成数据和合成 CLI。它用于证明仓库外模块能编译这些公开类型并运行共同验收；不作为真实第三方工程师、新厂商 CLI、真实模型或新的集群隔离验收结论。实际命令、冻结源码与通过结果由本片证据单独记录，旧验收保留原来源。

仓库内可用下面的命令重放 SDK 检查。`TestExternalSDKConsumer` 会把 `testdata/sdkconsumer` 复制到仓库外的无关模块，确认解析到当前 checkout，运行真实的子进程与公共 suite，并验证直接导入 `internal/adapter` 被 Go 编译器拒绝。任务接口在该消费者中仅检查编译时类型完整性，未声明或执行这些任务模式。

```sh
go test -race ./sdk ./sdk/contracttest -count=1 -v
```
