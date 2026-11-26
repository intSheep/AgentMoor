# 活跃 Worker Pod 突然丢失验收

从仓库根目录执行以下命令。主机依赖与[普通 kind 验收](task-contract.md#一条命令复验)相同，不需要真实模型账户或密钥。

```sh
make test-k8s-pod-loss
```

这是独立的慢验收，不随 `make test-k8s-file-report` 运行。它保留生产代码的 15 分钟执行期限，因此整个运行至少需要 15 分钟，再加集群启动、镜像构建和文件恢复时间。测试不改数据库、不推进测试时钟，也不缩短平台超时。

## 验证过程

1. 固定版本 Codex 通过脚本模型调用真实 MCP 工具，生成报告；删除首个 Pod 和临时工作卷。
2. 新 Pod 沿用原生会话，恢复工作区并修订 CSV、报告和工具证明文件；删除该 Pod。
3. 再提交一个新 UUID，等 MCP 工具写出启动标志并进入阻塞。故障注入程序从专属 kind 节点的 PID 命名空间向 Worker 容器发 SIGKILL，确认退出码 137，再删除对应 Job 和 Pod。这样不会给 Worker 正常确认失败的机会。
4. 独立 SaaS 客户端只通过公开 HTTP API 检查：期限前仍为 `running`，到达任务记录的 `deadline` 后为 `interrupted`；该回合没有成功资产，最近成功 checkpoint 和旧文件 SHA-256 不变。
5. 用原 UUID 重投两次，仍得到同一中断回合，不重建 Job。用新 UUID 创建新 Pod，从最近成功版本恢复并完成审计；原生会话不变，未归档草稿不进入恢复工作区，历史业务报告仍能下载且数据库保持两行。

Kubernetes 和节点容器操作只承担准备、同步启动标志和故障注入。任务状态、会话/工作区引用、文件字节和业务金额均由公开 HTTP 客户端验证。SIGKILL 只允许作用于名称匹配本次测试规则的 kind 节点，并核对容器的运行状态和 `agentmoor` 进程身份。

## 输出和清理

`POD_LOSS_STAGE` 分段显示报告生成、工具启动、Worker 无确认退出和生产期限等待。等待中约每 55 秒输出剩余秒数；不会因长时间无日志而误认为已经通过。成功时输出 `POD_LOSS_RESULT`，包含 Pod UID、原生会话 ID 的 SHA-256、公开期限、中断观察时间、旧版文件摘要和恢复结果。

测试退出时回收专属 kind 集群、镜像标签与 kubeconfig，不改变用户当前 Kubernetes context。若失败，先检查阶段输出、异常和 `docker info`；`make test-k8s-file-report` 可独立复验较短的文件生成、接续、取消和业务数据库重试场景。

此入口验证固定 Codex CLI 和脚本 Responses 模型、SQLite 元数据与本地不可变对象存储。它不代表真实模型或 PostgreSQL/S3 后端组合已通过相同的跨 Pod 流程。

## 2026-11-26 实测结果

测试源码提交 `c91198683ef002f211e0bc1a032a308b44432579`（基于已合并的 `942eac4`），固定 Linux ARM64 Codex `0.171.0-alpha.13`、脚本 Responses 模型、kind 0.33.0 / Kubernetes v1.35.0。`make test-k8s-pod-loss` 通过，退出码为 0；专属集群、Pod、镜像标签和 kubeconfig 已清理。[结构化证据](evidence/codex-pod-loss-2026-11-26.json)

公开 deadline 为 `2026-11-26T13:17:41.78629072Z`，在 `2026-11-26T13:17:41.876412Z` 观察到 `interrupted`，约为期限后 0.09 秒。期限前全部轮询为 `running`，没有成功资产且 checkpoint 未变。原 UUID 重投两次都返回同一中断记录，未创建新 Job；新 UUID 在新的 Pod 恢复审计成功，原生会话不变，未归档草稿没有恢复。旧报告仍可按固定版本下载，业务数据库保持两行。

| 阶段 | Pod UID |
| --- | --- |
| 生成成功 | `8bac1a7a-281a-4cec-8ddc-e14b18f2f232` |
| 修订成功 | `8c147b37-6899-4649-be22-cf5e0900153b` |
| 活跃工具中 SIGKILL | `73af4a5d-8f6d-4333-878f-e630ecbb5438` |
| 中断后新 UUID 恢复成功 | `ce6324ed-7481-4b11-be33-ae9c0c66cf9c` |

原生会话 ID 的 SHA-256 为 `6cf8e459b9d80b5b5e4265c305e3314d46120c7c4370fb017258518597cb5de9`。源提交的 `make fmt`、`make check`（含 race）、`make demo` 与 Python/Shell 离线检查均通过。此次长验收没有指定 Skill；后续合并固定 Skill 功能的主分支时，仍保留此原始实测提交和范围，不把新的合并提交当作又跑过一次 15 分钟验收。
