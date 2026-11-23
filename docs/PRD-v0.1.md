# PRD：Agent Asset Adapter

**版本：** v0.1

**状态：** Draft

**项目定位：** 多种 Agent Runtime 与外部资产管理平台之间的适配层

## 1. 项目背景

随着通用 Agent Runtime 的发展，平台可能同时运行多种 Agent，例如 OpenClaw、Claude Code、Codex，以及其他开源或自研 Agent。

不同 Agent 的内部实现存在明显差异：

- Session 的存储格式不同。
- Skills 的组织和加载方式不同。
- Workspace 的目录结构和生命周期不同。
- Artifact 的生成方式不同。
- 部分 Agent 提供生命周期 Hook 或 API，部分只能通过文件系统进行交互。

如果直接将 Agent 的 VM 或文件系统打包到对象存储，虽然能够保存执行环境，却无法解决平台层面的资产管理问题。

例如：
- Session 需要进入平台 Session Service。
- Skills 需要进入平台 Skill Registry。
- Artifact 需要进入平台 Artifact Service。
- 报告等业务数据需要进入对应的业务数据库。

因此，需要一层独立的 Agent Asset Adapter，负责识别不同 Agent 的数据结构和生命周期，将 Agent 内部资产转换为平台能够管理的资源。

## 2. 项目目标

建设一个可扩展的 Agent 适配框架，使平台能够在不强制统一 Agent Runtime 实现的情况下，管理不同 Agent 产生和使用的工作资产。

核心目标：

1. 支持多种 Agent Runtime 的差异化适配。
2. 将 Agent 内部资产映射为平台统一的资源模型。
3. 支持资产的发现、读取、持久化、恢复和变更同步。
4. 支持多租户身份和资源权限隔离。
5. 尽可能兼容不支持平台原生接口的第三方 Agent。
6. 支持未来通过 Runtime 原生 Hook / Provider 提高集成可靠性。

**非目标：**

- 不实现 Agent Loop。
- 不负责 LLM 调度和模型路由。
- 不负责 Agent Sandbox 本身。
- 不重新实现完整文件系统。
- 不试图统一所有 Agent 的内部 Session 数据结构。
- 不负责解释所有业务数据的语义。

## 3. 核心设计原则

### 3.1 Adapter Pattern

参考 Magpie 的 Agent-specific 实现。

每一种 Agent 独立实现 Adapter，不在核心逻辑中堆积大量 Agent 名称判断。

例如：

```text
Agent Asset Adapter
  |
  +-- Core
  |    +-- Adapter Registry
  |    +-- Asset Model
  |    +-- Sync Engine
  |    +-- Identity Context
  |
  +-- Adapters
       +-- OpenClaw
       +-- Claude Code
       +-- Codex
       +-- Custom Agent
```

不同 Agent 的实现负责处理自身的数据格式和行为差异。

### 3.2 统一资源模型，不统一内部实现

平台只定义必要的公共资源语义。

例如 Session 统一表达为一个可识别、可归属、可持久化的会话资源，但不要求所有 Agent 的消息、Tool Call、Checkpoint 都采用相同内部结构。

对于无法标准化的字段，保留原始数据或扩展字段。

### 3.3 业务语义由平台解释

Adapter 不直接理解业务系统的所有对象。

例如 Agent 生成 `report.pdf`：

Adapter 可以识别文件产生、内容类型和来源 Session，并将其提交给 Artifact Service。

但它不负责判断这是一份漏洞报告、财务报告还是代码审计报告。

具体业务类型由上层业务系统声明和解释。

### 3.4 能力必须显式声明

不能假设所有 Agent 都支持相同能力。

每个 Adapter 需要声明自己的能力，以及能力的实现方式和可靠性等级。

例如：

- Session：原生 API / 文件解析 / 不支持。
- Skill：原生 API / 文件目录 / 只读。
- Artifact：Hook 捕获 / 文件扫描 / 不支持。
- Workspace：文件同步 / 原生 Provider / 不支持。

## 4. 系统架构

```text
                  Control Plane
                       |
                Tenant / Project
                       |
                Agent Session
                       |
              Runtime Allocation
                       |
        +--------------+--------------+
        |                             |
   Agent Runtime A               Agent Runtime B
        |                             |
   OpenClaw Adapter              Claude Code Adapter
        |                             |
        +--------------+--------------+
                       |
                Adapter Core
                       |
        +--------------+--------------+
        |              |               |
    Discovery      Asset Mapper     Sync Engine
        |              |               |
        +--------------+--------------+
                       |
                 Platform API
                       |
        +--------------+--------------+
        |              |               |
   Session Service Skill Registry Artifact Service
        |              |               |
        +--------------+--------------+
                       |
                 Data Plane
                       |
              PostgreSQL / S3
```

Adapter Core 负责统一流程，具体 Agent Adapter 负责差异化处理。

## 5. Adapter 设计

### 5.1 Adapter Registry

负责注册、发现和管理 Agent Adapter。

每个 Adapter 需要提供：

```go
type AdapterDescriptor struct {
    ID          string
    Name        string
    Version     string
    Capabilities []Capability
}

type Capability struct {
    AssetType   string
    Read        bool
    Write       bool
    Sync        bool
    Restore     bool
    Mode        string
}
```

支持：
- 注册内置 Adapter。
- 查询已支持的 Agent。
- 查询 Agent 能力。
- 根据 Agent 类型选择 Adapter。
- 检查 Adapter 版本兼容性。

### 5.2 Adapter Interface

建议采用能力接口组合，不强制每个 Agent 实现所有方法。

```go
type AgentAdapter interface {
    Descriptor() AdapterDescriptor

    Detect(ctx context.Context, env RuntimeEnv) (
        DetectionResult, error,
    )

    Inspect(ctx context.Context) (
        AgentInfo, error,
    )
}

type SessionAdapter interface {
    ListSessions(ctx context.Context) ([]SessionRef, error)
    ExportSession(ctx context.Context, id string) (
        SessionSnapshot, error,
    )
    RestoreSession(ctx context.Context, snapshot SessionSnapshot) error
}

type SkillAdapter interface {
    ListSkills(ctx context.Context) ([]SkillRef, error)
    ExportSkill(ctx context.Context, id string) (
        SkillSnapshot, error,
    )
    InstallSkill(ctx context.Context, skill SkillSnapshot) error
}

type ArtifactAdapter interface {
    DiscoverArtifacts(ctx context.Context) ([]ArtifactRef, error)
    ExportArtifact(ctx context.Context, id string) (
        ArtifactContent, error,
    )
}

type WorkspaceAdapter interface {
    ExportWorkspace(ctx context.Context) (WorkspaceSnapshot, error)
    RestoreWorkspace(ctx context.Context, snapshot WorkspaceSnapshot) error
}
```

以上是接口设计草案，最终应结合不同 Agent 的实际能力确定。

不支持某项能力的 Agent 不需要实现对应接口。

### 5.3 Agent-specific 实现

每种 Agent 独立实现自己的适配逻辑。

例如：

```text
adapters/
  openclaw/
    detect.go
    session.go
    skills.go
    workspace.go
    artifacts.go
    tests/

  claude-code/
    detect.go
    session.go
    skills.go
    workspace.go
    artifacts.go
    tests/

  codex/
    detect.go
    session.go
    skills.go
    workspace.go
    artifacts.go
    tests/
```

每个 Adapter 应负责：

1. 识别 Agent 的版本和运行环境。
2. 识别资产所在位置。
3. 解析其内部数据格式。
4. 将数据转换为平台资源。
5. 处理增量变化。
6. 实现必要的恢复操作。
7. 维护对应版本的兼容性测试。

不能将某个 Agent 的路径、文件格式、特殊字段写死在 Core 中。

## 6. 资产模型

第一版关注四类资产。

### 6.1 Session

Session 是 Agent 的会话资源。

平台保存：

- TenantID
- ProjectID
- AgentID
- ExternalSessionID
- AssetVersion
- SourceAdapter
- OriginalFormat
- SnapshotLocation
- UpdatedAt

消息内容是否能够结构化解析，取决于 Adapter 的能力。

原始 Transcript 应允许保留，避免统一模型丢失 Agent 特有信息。

### 6.2 Skill

Skill 是可复用的能力资产。

平台保存：

- SkillID
- TenantID
- ProjectID / Global Scope
- Name
- Version
- Content
- Dependencies
- SourceAgent
- Checksum

需要区分平台管理的 Skill 与 Agent 原生 Skill。

第一版支持发现、导入、导出，不要求所有 Agent 共享同一种 Skill 格式。

### 6.3 Artifact

Artifact 是 Agent 产生的可交付文件。

平台保存：

- ArtifactID
- TenantID
- ProjectID
- SessionID
- Name
- ContentType
- Size
- Checksum
- ObjectLocation
- CreatedAt

文件内容放在对象存储，元数据进入平台数据库。

一旦 Artifact 发布，内容不可原地修改；修改应生成新版本。

### 6.4 Workspace

Workspace 是 Agent 的可变工作环境。

平台保存：

- WorkspaceID
- TenantID
- ProjectID
- SnapshotID
- StorageLocation
- Version
- UpdatedAt

第一版优先实现文件系统状态的导入、导出和恢复，不要求实现完整 POSIX 远程文件系统。

## 7. 资产同步机制

需要支持三种同步模式：

| 模式 | 机制 | 适用情况 |
|---|---|---|
| Event-driven | Runtime Hook / API 事件 | 原生支持事件的 Agent |
| Watch-based | 文件系统监听、数据库变更检测 | 能观察外部文件的 Agent |
| Periodic | 定期扫描和全量/增量比对 | 无事件能力的 Agent |

同步过程：

```text
Detect Change
    |
Identify Asset
    |
Read / Export
    |
Normalize Metadata
    |
Generate Idempotency Key
    |
Check Tenant Permission
    |
Upload Content (if needed)
    |
Persist Metadata
    |
Update Sync Checkpoint
```

要求：

- 重复事件不能生成重复资产。
- 失败任务支持重试。
- 同步中断后可以继续。
- 内容通过 Checksum 判断是否变化。
- 不允许未经授权跨 Tenant 同步。
- 保存同步游标和最后成功状态。

对于文件和业务数据库，不能假设存在跨系统事务。

采用先上传对象，再写入元数据的流程；失败时通过重试和孤儿对象清理进行恢复。

## 8. 多租户隔离

所有资产必须具备 Tenant 归属。

```text
Tenant
  └── Project
       ├── Agent
       ├── Workspace
       ├── Sessions
       ├── Skills
       └── Artifacts
```

隔离要求：

1. Adapter 从受信任的 Runtime Context 获取 TenantID，不接受 Agent 自行指定的租户身份。
2. 每次资产读写都执行权限校验。
3. 对象存储采用租户隔离的 Key Prefix。
4. Runtime 使用限定范围的短期凭据。
5. Adapter 不得获得整个对象存储的管理权限。
6. 跨租户资源引用必须显式授权。
7. 原始 Session、日志、Artifact 中的敏感信息应受访问控制保护。

## 9. 运行模式

支持两种集成模式。

### Mode A：Native Integration

Agent Runtime 主动调用 Adapter 提供的接口，或通过 Provider / Hook 暴露生命周期事件。

优点：
- 能获取明确的生命周期。
- 能准确识别 Session、Tool Call 和 Artifact。
- 更容易保证同步时序。

### Mode B：Compatibility Integration

不修改 Agent Runtime，通过外围机制完成适配：

- 文件系统扫描。
- 文件系统 Watcher。
- 数据库只读解析。
- HTTP API。
- MCP / Tool Proxy。
- 生命周期前后 Hook。

需要明确：外围机制只能获取它能够观察到的信息。

例如，不能保证通过文件监听获取内存中的最新 Session 状态。

因此 Adapter 应对能力和数据完整性进行标记，不得将推测状态标记为已可靠同步。

## 10. MVP 范围

第一阶段不追求支持大量 Agent，而是验证 Adapter 架构是否成立。

### 首批支持

**OpenClaw**

优先验证：
- Session 文件与会话元数据。
- Skills 目录。
- Workspace。
- 生成文件的发现。

**Claude Code**

优先验证：
- Session Transcript。
- Project Workspace。
- Skills。
- 文件产物。

选择这两个 Agent 是为了验证不同的内部存储结构，而不是假设它们拥有相同的资产语义。

### MVP 功能

1. Adapter Registry。
2. Agent Detect。
3. Session Export / Restore。
4. Skill Discover / Export。
5. Artifact Discover / Upload。
6. Workspace Snapshot / Restore。
7. Tenant Context。
8. 增量同步和幂等。
9. 同步状态与错误记录。

### 暂不实现

- 通用 Runtime Hook 协议的完整生态。
- 多 Agent 同时写入同一 Workspace。
- 完整 Git Branch / Merge。
- VM 内存级 Snapshot。
- 自动理解业务报告。
- 全量 Agent 自动发现和支持。

## 11. 技术方案建议

优先使用 Go。

建议初步技术选型：

| 模块 | 技术 |
|---|---|
| Adapter Core | Go |
| Adapter SDK | Go Interfaces |
| 文件监控 | fsnotify |
| 数据持久化 | PostgreSQL |
| 对象存储 | S3 / MinIO |
| 事件传递 | NATS / HTTP |
| 文件校验 | SHA-256 |
| 增量同步 | Manifest + Checksum |
| 运行隔离 | 独立 Adapter Worker / Sandbox |

Adapter Core 不应该与具体业务数据库实现强绑定。

使用平台 API 作为数据写入边界。

## 12. 验收标准

### 功能验收

- 能够识别不同 Agent。
- 不同 Agent 使用独立 Adapter。
- 能将 Session 导出并恢复。
- 能发现 Skills 并导入平台。
- 能捕获 Agent 生成的文件并上传对象存储。
- Runtime 销毁后，资产仍然存在。
- 新 Runtime 能恢复之前的 Workspace。
- Tenant A 无法访问 Tenant B 的资产。

### 一致性验收

- 同一事件重复投递不会产生重复 Artifact。
- 中途断网后能够继续同步。
- Runtime 异常退出后能够识别未完成的同步。
- 对象上传成功、数据库写入失败后可以恢复。
- Agent 原生数据与平台资产不一致时能够发现并记录冲突。

### 扩展性验收

新增一种 Agent 时：

- 不需要修改 Core 的业务逻辑。
- 只需要实现相应 Adapter。
- 必须通过该 Agent 的独立测试。
- 不支持的能力能够明确报告，而不是静默忽略。

## 13. 核心风险

1. 不同 Agent 的内部数据结构可能频繁变化，需要版本化 Adapter。
2. 文件系统监听不保证完整捕获所有状态，需要周期性对账。
3. Session 的恢复可能涉及 Agent 自己的索引、路径和元数据，不能只恢复 Transcript。
4. Skills 的依赖和权限可能因 Agent 而异。
5. 外部业务数据写入无法通过通用文件同步实现，需要业务 API。
6. 恢复操作必须避免覆盖 Runtime 中尚未同步的更新。

## 14. 后续演进

第一阶段验证不同 Agent 的资产提取和恢复。

第二阶段引入统一的 Native Adapter Protocol，使愿意修改 Runtime 的项目能够主动暴露资产生命周期。

第三阶段探索：
- 多 Runtime 之间迁移 Session。
- 跨 Runtime Workspace 共享。
- 多租户资产权限与审计。
- Artifact 与业务系统集成。
- 资产版本和分支管理。

最终目标不是让所有 Agent 变成同一个 Agent，而是让它们可以使用同一套平台资产管理能力。
