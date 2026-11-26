# 工作区增量上传

这项功能减少 Worker 向平台重复发送的文件内容。例如，只改一个文件时，Worker 发送缺少的文件内容块和一份压缩清单；其他文件的内容块可以复用。清单本身也有上传量，因此不能把“改了 4 KiB”直接写成“总共只传了 4 KiB”。

它默认关闭。启用后，公开的工作区资产仍是 `workspace-tar.v1`：`GET /v1/content` 返回原来完整 tar 的字节、长度和 SHA-256，恢复命令也不变。旧客户端仍可读取新版本，已有整包版本仍可下载和恢复。增量只是新的上传和内部存储方式，没有把公开内容换成清单文件。

## 开启与关闭

在原有受信 Worker 配置中加入以下字段，其他字段照常保留：

```json
{
  "workspace_transfer": "tar-parts.v1"
}
```

省略该字段或设为空字符串，后续新操作使用整包上传。其他值会被拒绝。该选项只影响工作区快照；Session、Skill 和 Artifact 的格式、上传方式保持各自的约定。SaaS 首轮 Worker 与追问 Job 如需都使用增量上传，须部署相应配置。

Worker 和平台都需要实现这项上传协议。旧平台不支持该选项，启用后请求会失败；Worker 不会自动退回整包上传。升级平台后再开启配置。

旧客户端兼容不代表旧平台二进制可直接降级。写入分片版本后，这些历史版本依赖新的存储读取器；关闭上传开关也不会把它们改回整包对象。平台回滚需保留该读取能力，或先按固定版本导出完整 tar 作为独立备份。

每个待发操作都保存自己的传输方式。开关只影响**之后创建的新操作**，不会改写已有 pending：保留同一 `state_dir` 再运行 `sync`，先按旧方式完成原来的冻结内容，再按当前配置扫描并发布新内容。原本省略传输字段的旧 journal 按整包方式恢复。不要通过删除 journal 或冻结文件来切换方式。

## 上传与恢复

Worker 先按现有规则读取工作区、生成确定性的完整 tar，并冻结到工作区之外。后续分片只读取这个冻结文件，不再读取活动工作区。原始 header、PAX、padding 和结束标记保留为清单中的内联字节；文件 body 单独按内容寻址，大文件的 body 再分成最多 4 MiB 的块。拼接这些字节必须与原 tar 完全一致，平台不重新编码 tar header。

发布沿用原来的资产元数据、base version 和操作键：

1. `POST /v1/changes` 建立或重放操作。
2. `POST /v1/changes/{op}/parts` 发送 gzip 压缩的 `tar-parts.v1` 清单，平台返回缺少的内容块。同一操作第一次接受的清单保持固定。
3. `PUT /v1/changes/{op}/parts/{sha}` 上传缺少的 body。重复 body 只需一份，已有 body 不再上传。每个请求仍使用原有鉴权与凭据轮换逻辑。
4. `POST /v1/changes/{op}/commit` 检查全部依赖、每个 body 块的长度与摘要、完整 tar 长度与摘要，再在同一元数据事务中提交版本、引用和操作状态。Worker 收到成功结果后才推进游标。

内容块只在同一个租户、项目、运行时和适配器作用域内复用。知道摘要不代表拥有其他作用域的访问权。对象写入保持不可变；部分上传、提交失败和丢失回复后，重试仍使用原操作键、冻结 tar 与已固定的传输方式。保留 Worker 状态目录是这种本地 pending 恢复的前提；计算实例与临时卷一起丢失时，仍以最近一次成功检查点为恢复边界。

下载时，平台按固定版本的清单顺序读取并拼接内容，客户端收到完整 tar。平台逐块检查内容，恢复端再核对整个 tar 的摘要与长度。恢复必须使用尚不存在的新目标路径：先在同一文件系统临时位置完成校验和展开，再原子发布；已有目标不会被覆盖。

## 文件变化与历史版本

| 变化 | 下一版工作区与上传行为 |
| --- | --- |
| 新建文件 | 清单加入路径和权限，上传平台尚无的 body 块。空文件只有 framing，没有 body 块。 |
| 修改文件 | 清单引用新内容；复用其他已知 body。较大文件的变化可能影响多个 4 MiB 块。 |
| 改名或权限变化 | 更新 tar framing；相同 body 可以继续复用。 |
| 删除文件 | 下一版清单不再包含该路径，不需要重传其他文件 body。旧版本仍保留该文件。 |
| 相对 symlink | 保留原有目标及边界检查，链接最后安装；链接变化体现在 framing 中。 |
| 没有变化 | 不创建新版本，也不上传清单或 body；鉴权和查询 head 等读取请求仍会发生。 |

这里的“删除”指工作区内的文件从下一次快照消失，不是远端资产删除接口，也不会删除历史版本。冲突仍按 base version 检查：本地冻结内容与平台内容都保留，须先查看平台版本，再按[冲突处理步骤](operations.md#5-处理失败或冲突)明确选择。

平台在对象上传之前持久记录候选及引用。GC 保护所有历史工作区版本引用的 body，不只保护最新 head；未过期 pending 的 body 也受保护。超过现有 24 小时期限、且没有已发布版本或其他 pending 引用的孤儿内容才可回收。删除失败的候选保留，下一次 GC 可继续处理；历史仍引用的对象不会因为本地删文件而被回收。

## 范围与成本

| 项目 | 上限 |
| --- | --- |
| 单个文件 body 块 | 4 MiB |
| 单个内联 framing 片段 | 64 KiB |
| 一份清单内联字节合计 | 8 MiB |
| 清单未压缩 JSON，以及 HTTP 压缩请求体 | 各 16 MiB |
| 一份清单片段总数 | 65,536 |
| 逻辑工作区 tar | 沿用资产总大小上限 8 GiB |

内联字节在 JSON 中使用 base64，会占用更多清单空间；所有限制都必须同时满足。文件数量、长路径及 PAX 元数据都会消耗预算。超限会明确失败；已经建立的待发操作及冻结文件会保留，不会自动退回整包上传。修改配置也不会替换已经固定的 pending 方式。需要改用全量方式时，保留原状态目录供审阅，在新状态目录按全量配置接管；先查看平台版本，出现冲突时按既有明确选择流程处理。

本实现仍会完整扫描工作区、打 tar 和计算摘要，无变化阶段也有这些本地成本。每个操作首次成功提交前，平台还要完整读取全部 body 并核对整个 tar SHA-256；重试已经提交的同一操作可直接返回原结果。下载使用逐块打开、关闭的串流方式，不把所有 S3 连接或整个工作区放进内存，但小文件较多时仍会产生较多对象请求。

因此，减少上传字节不保证同步或恢复耗时变短。SQLite/本地磁盘与 PostgreSQL/S3 的请求延迟、首次写入成本和完整校验成本需要分别测量。当前不提供性能 SLO，也不把本实现称为避免全量扫描的文件监听算法或执行中进程迁移。

## 复验与测量

不需要模型密钥的持久化集群入口：

```sh
make test-k8s-workspace-parts
```

它在一次性 kind 集群中，以 PostgreSQL/S3 后端分别运行 Codex 文件和固定 Skill 场景，并为 Worker 开启这项传输方式。主机依赖及清理约定见[集群验收](task-contract.md#一条命令复验)。这是独立专项，不代替默认四 CLI 矩阵，也不代表真实模型或所有故障组合已经验收。

上传量比较使用真实平台、`sync`/`restore` CLI 和回环 HTTP 代理。先提交源码，再构建匹配该干净提交的二进制；结果写到仓库之外。先用小输入检查流程，再顺序运行相同 seed、文件数量和大小的对照：

```sh
make build
python3 tests/workspace_upload_baseline.py --transfer tar-parts.v1 --backend local \
  --files 8 --bytes 127 --seed 0 --output /tmp/workspace-parts-small.json
python3 tests/workspace_upload_baseline.py --transfer full --backend local \
  --files 5000 --bytes 4096 --seed 0 --output /tmp/workspace-full-local.json
python3 tests/workspace_upload_baseline.py --transfer tar-parts.v1 --backend local \
  --files 5000 --bytes 4096 --seed 0 --output /tmp/workspace-parts-local.json
python3 tests/workspace_upload_baseline.py --transfer full --backend postgres-s3 \
  --files 5000 --bytes 4096 --seed 0 --output /tmp/workspace-full-postgres-s3.json
python3 tests/workspace_upload_baseline.py --transfer tar-parts.v1 --backend postgres-s3 \
  --files 5000 --bytes 4096 --seed 0 --output /tmp/workspace-parts-postgres-s3.json
```

本地测量需要 Go、Python 3.11+ 和回环端口。`postgres-s3` 模式另需 Docker Compose；脚本为本次测量创建独立项目、回环端口、合成凭据和 bucket，退出时清理容器及卷，不使用用户现有服务或模型凭据。该测量入口把单文件限制在 4 MiB 内，以检查一次单文件改动恰好对应一个新 body；产品上传本身支持更大文件分成多个块。

比较时同时计算完整 tar PUT、body PUT 和 gzip 清单 POST 的实际请求体字节；不能只统计 body、忽略清单，也不能用文件大小或未压缩清单大小估算网络量。HTTP 头和传输层开销另外区分。首次、无变化、单文件修改及删除各阶段还要记录同步与提交耗时，并核对固定版本 HTTP 下载和历史恢复；结果保留确切源码、二进制与后端身份。[原整包基线](workspace-upload-baseline.md)只证明原上传行为，不能替代新增增量模式的实测结果。
