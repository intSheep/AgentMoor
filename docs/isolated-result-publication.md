# 隔离结果的可信发布

[INT-79](https://linear.app/intsheep/issue/INT-79) 增加 Worker 内部的结果发布组件。它接收已封存的 `native-guest.v1` 结果，校验后把工作区、Session、Skill 和交付文件发布到平台。整个过程只读写数据，不在可信 Worker 上探测或启动原生 CLI，也不导入原生会话。[发布实现](../internal/worker/sandbox_publication.go) · [HTTP 故障测试](../internal/worker/sandbox_publication_fault_test.go)

本次交付没有把该组件接到 `serve` 或 `run-task`。`untrusted-isolated.v1` 门禁继续关闭；完整提供方、真实网络和资源限制、SaaS 到真实 Codex 的整体验收仍需完成。四 CLI 回归使用现有受信开发路径，不能据此开放不可信执行。

## 发布前先核对什么

调用入口是内部方法 `publishSandboxResult(ctx, record, status, source)`，由后续隔离提供方调用。调用方须用同时受原 deadline 和任务取消控制的 `runCtx` 从受保护的 `Client.SandboxOutput` 接口取得结果流，并负责关闭它；普通 `io.Reader` 本身不保证中断已阻塞的读取。本组件不领取任务、不启动 Pod、不执行 Agent，也不提交任务完成或更新成功 checkpoint。

| 核对项 | 依据 |
| --- | --- |
| 当前身份 | 平台实际授权的 Scope 必须与 Worker 配置一致，同时有 Read、Write、Execute；公开任务仍为 running。 |
| 固定分配 | Run 的 Scope、turn、原始 Spec、Parent、Skill pins、deadline 和 `untrusted-isolated.v1` 必须与真实任务记录一致。 |
| 固定版本 | Request 和 Result 必须匹配已注册适配器的静态 ID、版本及已测 runtime 版本，同时满足部署配置的预期版本。 |
| 清理完成 | 状态必须为 Ready，输入和输出均封存；创建已围栏，固定 Pod UID 正常退出 0，并已确认删除。 |
| 继承关系 | 追问必须沿用固定 Session ID、父 Workspace/Session 摘要及 Skill 名称/摘要，不能改成新会话或当前 head。 |

输入的状态来自可信平台接口。Guest 只交回结果数据，不能选择平台 Scope、操作键、Worker journal、宿主目标路径或原生命令。

## 整包通过后再发布

结果封套只接受固定顺序的三个普通文件：`result.json`、`workspace.tar`、`session.blob`。Worker 在自己的 StateDir 下创建私有暂存目录，读取到 EOF，并独立比对平台封存的整包 SHA256 和大小；内部两个 blob 也各自核对摘要、大小和格式。只改一字、长度不变且内部校验仍合法的结果，同样会因整包摘要不符而失败。

Workspace 经有界路径/类型检查后，用 `snapshot.Restore` 原子恢复到配置指定的新路径，已有目标保持不变。Session 保留原始字节；声明为 `application/x-tar` 时还做通用归档安全检查，不识别或导入原生会话内容。可信 Worker 不调用 `Inspect`、Execute、Continue、原生 Import/Export 或 SessionSource。

发布前会先检查整个 `out`：它必须是真目录，所有交付文件必须是普通文件、资产 Key 安全、数量不超过 128，且包含 Spec 指定的主输出。目录链接、文件链接、缺主文件或超量文件都在第一笔发布之前失败。固定文件解析、完整校验和交付预检也先于旧 pending 操作的恢复，因此损坏的新结果不能触发旧上传。

成功校验的 `workspace.tar` 和 `session.blob` 会原样复制、再次核对摘要后冻结发布，不重新打包。Workspace 内 Skill 由适配器的数据扫描接口发现，Skill 包和 Artifact 沿用既有发布流程；Skill 不会在宿主执行。平台 Scope、turn、基础版本、操作键和版本标记由可信配置及 journal 产生。

## 有界输入与可靠清理

| 输入 | 当前限制 |
| --- | --- |
| 完整输入或结果封套 | 各最多 64 MiB；结果还受平台封存大小约束。 |
| JSON | Request 最多 128 KiB，Result 最多 16 MiB，结果文本最多 2 MiB；深度最多 64，键和值节点总数最多 65,536。 |
| 归档内容 | 常规文件展开总字节有界；禁止稀疏、GNU 扩展和非普通文件/目录/安全相对链接。 |
| 目录节点 | 最多 65,536 个不同节点，包含逻辑根目录及省略 tar 条目但恢复时会创建的父目录。共享父目录只计一次。 |
| 路径与链接目标 | 各最多 4,096 字节；不能越过分配目录。普通 PAX 长路径继续支持。 |
| 交付 | 最多 128 个普通文件，必须有主输出。 |

新增 host 解码入口时补了两个可复现的资源漏洞：逐 Token 的 JSON 递归不能依赖普通 Decode 的深度限制；约 0.54 MiB、1,100 条 tar 文件头也能隐式产生超过 65,536 个文件系统节点。测试只验证恶意 tar，不实际展开大目录树。限制检查不改写原始归档。

另一个小包把目录权限设为 `000`，会使普通用户的失败清理遇到权限拒绝。`snapshot.RemoveDirectory` 现在仅对自己拥有的私有目录逐层恢复 owner 访问权，再删除目录；遍历不跟随链接，目录内 chmod 通过 `os.Root` 完成，清理错误合并返回。它同时用于 Restore 的失败暂存目录和结果预检失败的 Workspace。旧目标和链接指向的外部目录及权限保持不变。原始归档字节不变。

输入验证失败会清理本次暂存及新 Workspace。进入发布后发生故障，已验证的 Workspace 留作检查，pending 的冻结文件及 journal 保留以便恢复。正常结束会清理校验暂存；这些限制不等于整台宿主或整个 Pod 的磁盘配额。

## 发布中断怎样恢复

组件共用现有授权、journal 独占锁、head 比对、冻结发布和有限重试。它只把数据捕获部分替换为已校验的原始 blob；普通受信 Sync 的原生行为不变。专用 journal 位于原 StateDir 的 `publish` 子目录。

Begin、Upload、Commit 前后遇到 503 或真实 TCP 应答丢失时，最多三次重试使用同一操作和冻结字节。即使测试在故障后改动已恢复的交付文件，重试仍上传原字节。耗尽、取消或截止后保留 pending；后续调用须使用新 Workspace，并先继续原冻结操作，不重跑 Agent。部分资产版本可能已经提交，但成功 checkpoint 只由后续完整 Finish 推进，不能提前移动。

## 实际验证与复验

实现来源为 `e79066bfbd217dd4aa64654fe537dad408991139`。真实 HTTP/Catalog 测试使用明确的合成适配器：任何原生方法调用都会计数并报错，另设可执行文件哨兵；成功与失败路径均断言宿主原生命令调用为零。首轮和追问在两个新 Workspace 中沿用同一 Session，逐字节检查原始 Session/tar、多份 Artifact、完整 Skill 及固定版本引用。

测试覆盖作用域、权限、来源、版本、父状态、摘要、封套、路径、输出和资源限制的拒绝；503/TCP 应答丢失的六个发布位置；耗尽、取消、截止、冻结恢复及旧 checkpoint 保护。JSON、隐式目录和权限清理各有旧实现负例，记录真实失败，不把失败结果改写成通过。

```sh
make fmt
make check
make demo
PATH=/Applications/OrbStack.app/Contents/MacOS/xbin:$PATH make test-k8s
go test -race -v ./internal/worker ./internal/nativeguest ./internal/snapshot \
  -run '^Test(Sandbox|Metadata|NativeGuestTree|RemoveDirectory|RestoreFailure)' -count=1
```

该实现通过 `make fmt/check/demo` 和完整四 CLI `make test-k8s`，16 组断言及 8 次追问全部通过，原生取消约 3.0/3.1/3.7/3.0 秒。四份镜像中的实际 CLI 版本分别为 Codex `0.171.0-alpha.13`、Gemini `0.70.0`、OpenCode `1.19.6` 和 Claude Code `2.1.330`；专属集群和任务镜像标签已清理，明确保留固定依赖的缓存标签。一次额外版本采集遗漏可写 HOME，导致 OpenCode 探针失败；原诊断保留，随后在相同镜像 ID 和新私有 HOME 下复核四项，全部成功。集群契约本身未因此失败或重跑。

合入 main `c18e3bd` 后，干净整合源码 `16985473d4a7c6053ba63b220f854d087cce8571` 再次通过 fmt/check/demo。本组件源文件未变；没有把前述四 CLI 结果改写为整合后重跑。完整命令退出码、源码、原始日志摘要、四 CLI 版本和清理结果见[结构化证据](evidence/isolated-result-publication-2026-11-27.json)。本次没有新增数据库或对象存储协议；生产 CI 仍执行服务回归。合成组件测试与受信四 CLI 矩阵各有自己的范围，不代表真实模型、gVisor 全链路、恶意网络/资源对抗或生产 HA 已验收。
