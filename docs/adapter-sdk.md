# Go Adapter SDK 预览

`github.com/intSheep/AgentMoor/sdk` 让仓库外的 Go 模块实现当前 Adapter 接口，不需要直接导入 `internal/`。首个 SDK 发行版本为 **`v0.1.0-alpha.2`，0.x 预览**；先积累独立接入反馈，再决定何时冻结 1.0。升级前查看[发布说明](releases/v0.1.0-alpha.2.md)，并在自己的模块固定确切版本。

已有 `v0.1.0-alpha.1` 标签保持原样，其中没有 SDK，Go 模块路径也不同。不要用旧版本或 `@latest` 代替下面的确切版本。远程下载、本地替换和外部工程师接入分别记录，不能互相代替。

## 安装固定的预览版本

在仓库外的独立目录使用本机 Go `1.27.4` 工具链：

```sh
go mod init example.com/my-agentmoor-adapter
GOWORK=off GOTOOLCHAIN=local go get github.com/intSheep/AgentMoor/sdk@v0.1.0-alpha.2
GOWORK=off GOTOOLCHAIN=local go list -m -json github.com/intSheep/AgentMoor
```

最后一条输出应包含 `Path: github.com/intSheep/AgentMoor`、`Version: v0.1.0-alpha.2`，且没有 `Replace`。这是根模块内的子包，不是名为 `sdk` 的独立 Go 模块；导入路径仍是 `github.com/intSheep/AgentMoor/sdk`。首次获取需要网络。按下方接口实现后运行 `go test -mod=mod ./...`；需要完整资产验收时再导入 `sdk/contracttest`。

维护者可以从 checkout 重放远程发布验收：

```sh
python3 scripts/check-sdk-release.py v0.1.0-alpha.2 --output /absolute/new/sdk-release-evidence
```

该命令使用专属空模块缓存和公共 Go 代理/校验服务，从指定版本下载，不使用本地 `replace` 或工作区。它实际运行仓库外的合成原生适配器契约、无 CGO 核心构建和内部包拒绝反例，留下模块校验和及日志。需要 C 编译器、CGO 和本地 HTTP 监听权限；输出目录必须是新路径。它验收已发布模块的获取与使用，不代表真人第三方接入或新的厂商 CLI 支持。

## 0.x 兼容约定与 1.0 门槛

0.x 期间公开接口仍可能调整，不承诺 v1 API 稳定性或固定兼容维护窗口。每次预览使用新标签，保留旧标签；发布说明列出接口、工具链和行为变化，发生不兼容调整时给出迁移方法。消费者固定版本，升级后重跑自己声明的能力测试。

公开 `sdk` 类型目前是内部类型的别名，内部字段或方法变化也可能改变公开 API，因此必须检查其对 SDK 消费者的影响。`sdk/contracttest` 作为可选测试包同样处于预览阶段。

冻结 1.0 前，需要真实外部接入者仅凭公开文档和已发布模块完成接入，处理其中的阻碍，并明确公开 API、升级方法及兼容维护规则。声明原生任务能力的接入还须通过对应固定 CLI 版本和规定的集群验收。维护者测试与独立 agent 演练用于提前发现问题，不能替代真实第三方接入；当前不预设参与者数量或冻结日期。

## 从可运行示例开始

[独立接入起步包](../examples/sdk-adapter/README.md)提供完整的文件机制 Adapter 和公共资产测试，固定已发布 `v0.1.0-alpha.2`，没有本地 `replace`。可将整个示例目录复制到仓库外运行，然后替换为自己的格式。它只声明资产能力，不调用原生 CLI 或模型；任务能力按下方接口和相应验收另行增加。

试用后用 [Adapter SDK 接入反馈模板](../.github/ISSUE_TEMPLATE/adapter-integration.md)记录确切版本、实际命令、来源与失败；GitHub New issue 中也可以选择同名模板。公开 SDK 和示例的工程测试均不等于已经获得真实第三方接入证据。

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

## 公开字段与方法速查

下面把别名展开成可读的公开签名，供实现时查阅；这些类型已经由 SDK 提供，不需要在自己的包中重新定义。只实现所声明的能力，发现专用 Adapter 只需第一项接口。

```go
// sdk
type AssetKind string

type Capability struct {
    Operation, Mode, Reliability string
}

type Descriptor struct {
    TaskModes     []string
    TestedRuntime string
    ID, Version   string
    Capabilities  map[AssetKind][]Capability
}

type Adapter interface {
    Descriptor() Descriptor
    Detect(context.Context, *os.Root) ([]string, error)
}

type SkillRef struct { Manifest string }

type SkillDiscoverer interface {
    DiscoverSkills(context.Context, *os.Root) ([]SkillRef, error)
}

type Entry struct { Source, Name string }

type NativeSession struct {
    ID, Format string
    Entries    []Entry
    Raw        bool
}

type SessionSource interface {
    Sessions(context.Context, *os.Root) ([]NativeSession, error)
}

type NativeSessions interface {
    ExportNative(ctx context.Context, executable, workspace, directory string) error
    ImportNative(ctx context.Context, executable, workspace, source, freshState string) error
}
```

`SkillRef.Manifest` 相对于传入的工作区根。`Entry.Source` 相对于分配的 Session 根，`Entry.Name` 是归档中的相对名称；两者保持安全相对路径。`NativeSession.ID` 是格式中的原生身份，`Format` 由适配器明确命名。`Raw=true` 表示一个原始普通文件，要求恰好一个 Entry；`Raw=false` 表示文件树，可包含实际支持的关联文件。原生导出写入共用流程准备的目录，原生导入接收已经校验的 source 和新的状态目录。

注册、版本观测和受信 CLI 工具的签名为：

```go
// sdk
type RuntimeInfo struct {
    Descriptor     Descriptor
    Evidence       []string
    RuntimeVersion string
    Tested         bool
}

func NewRegistry(adapters ...Adapter) (*Registry, error)
func (r *Registry) Lookup(id string) (Adapter, error)
func (r *Registry) All() []Adapter

func Inspect(ctx context.Context, a Adapter, root *os.Root, executable string) (RuntimeInfo, error)
func RunCLI(ctx context.Context, executable, workspace string, env []string, out io.Writer, limit int64, args ...string) error
func RunCLIInput(ctx context.Context, executable, workspace string, env []string, input io.Reader, out io.Writer, limit int64, args ...string) error
```

`Registry` 没有需要调用方填写的公开字段，使用构造函数创建。`RunCLI` 的可执行路径必须是绝对路径，`limit` 限制 stdout 字节数；`RunCLIInput` 另外提供 stdin。环境参数为显式的 `NAME=value` 列表，调用者负责其中的值。命令不经过 shell；若显式选择 shell，可执行的 shell 仍须属于受信运维设置。

任务可选接口及全部参数类型为：

```go
// sdk
type LoadedSkill struct { Name, Manifest string }

type Environment struct {
    Executable, Workspace, Home string
    Skills                      []LoadedSkill
}

type Spec struct { Mode, Instruction, Input, Output string }
func (s Spec) Valid() bool
func (s Spec) Prompt() []byte

type Execution struct { Text, SessionID, HistoryDir string }

type RestoreHistory interface {
    Files(destination string) error
    NativeDatabase(destination string) error
}

type Executor interface {
    ExecuteTask(context.Context, Environment, Spec) (Execution, error)
}

type Continuation interface {
    ContinueTask(ctx context.Context, env Environment, sessionID, message string, restore RestoreHistory) (Execution, error)
}

type FileExecutor interface {
    ExecuteFile(ctx context.Context, env Environment, spec Spec, result, session string, restore RestoreHistory) (Execution, error)
}

type SkillInstaller interface {
    SkillDirectory(env Environment, name string) (string, error)
}
```

`Environment` 的目录由受信共用流程分配，`LoadedSkill.Manifest` 指向已校验的私有原生 manifest。`Spec.Output` 是交付文件名；`Execution` 返回文本、真实原生会话 ID 和对应历史目录。继续执行通过 `RestoreHistory` 将固定父版本恢复到新路径；文件执行的 `result` 是新建的工作区相对路径，`session` 在继续执行时传递原会话 ID。`SkillDirectory` 返回该 Skill 在私有 Home 内的原生安装目录。具体调用与状态约定见[任务契约](task-contract.md)。

## 开发时使用指定 checkout

在仓库外建立一个目录，将下面的绝对路径换成实际 checkout：

```go
module example.com/my-agentmoor-adapter

go 1.27.4

require github.com/intSheep/AgentMoor v0.0.0

replace github.com/intSheep/AgentMoor => /absolute/path/AgentMoor
```

这里的 `v0.0.0` 是本地替换所需的占位版本，不是发布标签。模块路径大小写使用 `github.com/intSheep/AgentMoor`。准备好下面的实现与测试文件后，从这个独立模块运行。离线验证直接构建所需包，使用本机工具链和已有依赖缓存：

```sh
GOWORK=off GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off \
  go test -mod=mod -race -count=1 ./...
```

`GOWORK=off` 避免本机工作区配置替代上述依赖关系，`-mod=mod` 允许生成本模块需要的 go.mod/go.sum 条目。离线运行仍需缓存实际构建依赖；缺少它们时先在可联网环境准备依赖。可联网开发可以另行运行 `go mod tidy`，它还会检查依赖的测试，可能需要本次构建用不到的额外模块，因此不作为这里的离线前置步骤。只编译 SDK 核心、关闭 CGO 时可去掉 `-race`；完整资产 runner 需要下述 CGO 环境。

不要直接导入 `github.com/intSheep/AgentMoor/internal/...`：Go 的内部包边界仍然有效。

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

这些测试类型的完整字段如下：

```go
// sdk/contracttest；sdk 指 github.com/intSheep/AgentMoor/sdk
type Mechanism string

const (
    Filesystem Mechanism = "filesystem"
    NativeCLI  Mechanism = "native-cli"
)

type Paths struct { Workspace, Sessions, Runtime string }
type Calls struct { Version, List, Export, Import int }

type NativeFixture struct {
    Executable     string
    Calls          func(*testing.T) Calls
    SetVersion     func(*testing.T, string)
    AssertImported func(*testing.T, string)
}

type Fixture struct {
    Adapter                  sdk.Adapter
    Mechanism                Mechanism
    SessionID, SessionFormat string
    SkillManifest            string
    SessionFiles             map[string][]byte
    Native                   *NativeFixture
}

type Case struct {
    Name    string
    Prepare func(*testing.T, Paths) Fixture
}

func Run(t *testing.T, c Case)
func ValidateDescriptor(a sdk.Adapter) error
```

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

## 离线外部测试的依赖准备

仓库里的自动验收把当前 SDK `go.mod` 已显式锁定的 require 根加入临时外部主模块，使用相同版本准备正例与内部包拒绝反例。它不继承 module/workspace/replace/exclude 等其它配置，不修改生产依赖；普通可联网消费者不需要手动执行这一步。`GOENV=off` 同时隔离用户持久保存的代理配置。

最初两个 Linux CI 在 `GOPROXY=off` 下需要未缓存的旧间接依赖元数据，失败记录保留。相同的稀疏缓存只含当前 22 个 require 版本：旧引导准确失败，修复后通过，旧版本元数据始终没有出现。`11c4a7d` 的 fmt、SDK race、完整 check、demo 通过。这里证明的是显式锁定依赖根后的离线验收，不宣称单条 SDK require 能在缺少旧模块元数据时自行离线完成引导。

## 2026-11-27 实际验证

干净源码 `02e6fbd01f129b4dec6b3939b736d3a981e035f4` 的 fmt、SDK/外部消费者/CLI helper 聚焦 race、无 CGO 核心构建与依赖检查、完整 check、demo 均退出 0。外部无关模块实际运行注册、版本正反例及原生资产契约，合成命令 list/dump/load 的调用计数为 3/3/1；可选任务接口只验证类型闭合。另一份直接导入内部包的外部模块得到真实 Go 编译拒绝，保留原文。

迁移提交 `d11d12b` 只改变 156 个 Go 文件的 348 条 import 和 module 指令，独立去除 import 后函数体完全一致。随后 `a7b2268` 单独修复原 CLI helper 在零参数且执行失败时索引越界的问题：旧 panic 与精确回归来源保留，固定错误名回退后返回原错误，零参数成功和原有带参数调用语义保持。

另一个未参与实现的 agent 仅依据公开文档、公开 SDK 与反射信息，新写文件机制适配器 `aurora-notes` 并实际通过公共资产 suite。初次文档未完整展开别名字段/签名，离线 tidy 也触发未缓存的依赖测试；这些诊断与反射辅助均如实保留。本页已据此补全速查及 `-mod=mod` 离线步骤，Go 源码未因此改变。该演练不是真人第三方认证。

[冻结来源、迁移核验、真实拒绝、独立演练及无损日志](evidence/public-go-sdk-2026-11-27.json)。以上 2026-11-27 记录没有发布新 tag；新版本发布按本页开头的流程单独验收，不重标这些历史结果或既有真实 CLI、数据库、集群验收。

## 2026-11-28 预览发布与真实远程验证

PR32 的两套 CI 通过后合并，`v0.1.0-alpha.2` 已作为 [GitHub prerelease](https://github.com/intSheep/AgentMoor/releases/tag/v0.1.0-alpha.2) 发布，固定到 `c6ffc92e3ecc3b641b0843d4e0a17c64064251f6`。旧 `alpha.1` 仍指向原提交。发布准备源码 `62ffe93` 的 `make fmt/check/demo` 均通过。

发布后，真实公共 Go 代理在专属空缓存中下载了确切 `alpha.2`；模块校验和、标签来源、无 `Replace`、无工作区、无 CGO 核心构建通过。仓库外合成消费者在 race 下实际通过注册与版本观测、原生资产 suite 的具名子 case，观察到 list/dump/load 的 3/3/1 次调用；直接导入内部包得到特定编译拒绝。私有模块缓存清理成功。

发前另用远程 main 的 Go 伪版本跑通脚本；它与发后 `alpha.2` 证据分开。旧 `alpha.1` 的真实拒绝发生在旧 module 名与 canonical 路径不符时，不冒称为 SDK 包缺失诊断。脚本聚焦检查保留了两次 harness 诊断及修正后结果，并记录了 Go telemetry 临时 Home 后台重建与后续清理的观察边界。[发布、校验和、实际输出与完整来源](evidence/sdk-preview-release-2026-11-28.json)。这些仍是维护者的合成验收，真实第三方接入门槛保持开放。

独立起步包在干净源码 `3a689d5` 上通过 `make fmt/check/demo/test-sdk-example`，另将完整目录复制到仓库外，以确切已发布 SDK、无替换和只读依赖文件实际通过 race 测试。原始 Session/Skill 往返、旧版本及大小/路径/取消拒绝均有具名结果；这次示例测试使用已有依赖缓存，不重标为空缓存下载。[源码摘要与完整输出](evidence/sdk-starter-2026-11-28.json)。下一项 [INT-99](https://linear.app/intsheep/issue/INT-99) 收集真实外部接入反馈后，再评审 1.0 冻结。
