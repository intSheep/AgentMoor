# 隔离执行与持久服务组合验收

本入口将真实固定版本 Codex、gVisor 隔离、PostgreSQL 元数据、S3 兼容对象存储及仅运维审计组合运行。它创建并删除自己的 kind 节点，不修改已有集群。模型仍为脚本 TLS 服务，不使用真实模型密钥。

```sh
PERSISTENT_OUTPUT=/absolute/new/evidence.json make test-k8s-sandbox-persistent
```

可复用 `GVISOR_SECURITY_BUNDLE`、`GVISOR_SECURITY_CALICO_MANIFEST` 和 `GVISOR_SECURITY_CONTAINERD_CACHE` 的固定官方缓存。两个服务镜像沿用 `tests/kubernetes/persistent.yaml`，运行前需在 Docker 缓存中存在；按实际平台导入专属节点，并在证据中保留真实 imageID 和数据库版本。共享公开依赖缓存保留，不将其描述为已删除。

验收运行三轮相同原生 Session，每轮新建并回收独立 guest Pod。第一、二轮结束后正常删除并重建 API Pod，保留签名 Secret，使用全新 emptyDir；通过公开 HTTP 核对原 Task 回执、检查点和精确旧资产字节，通过独立只读 audit CLI 核对审计前后完全相同。两次替换前即保存安全诊断快照，API 端口实际就绪后才进行下一步。

数据库和对象存储凭据只注入可信 API Pod，Worker 与 guest 不接收这些环境变量；真实原生命令前的计数夹具只记录禁止变量名是否出现，不记录任何值。运维检查实际 PostgreSQL 行和 S3 键，并确认没有 SQLite 或本地对象目录回退。业务客户端只调用公开 HTTP。

这是专属单节点与脚本模型的组合验收；服务使用夹具内明文 PG/S3，不代替生产 TLS 配置，也不宣称整节点网络分区、数据库灾备、自动跨节点接管、全部 CLI 隔离或真实模型质量。API Pod 的正常重建与独立 Worker SIGKILL、节点 kubelet 停止证据分别列出，不互相替代。

## 2026-11-27 实际结果

干净源码 `1afa1ea86347aa98ff50db593e78fdd96ce56ec3` 实际通过：真实 Codex 三轮新 gVisor Pod，每轮原生计数为 1、同一 Session，11 份旧版本字节复读保持；最终运维审计 73 条、11 个资产链接，原公开 UUID 三次重放无新增事件。两个 API 重建分别保持已有 23/48 条审计，UID 连成三个不同 Pod；当前检查点也精确保持。

两个重建 hook 直接比较的是初始 Task 回执及其三份资产；第二次另行比较 confirmation 的最新检查点。后续全部 11 份旧资产复读、精确审计链接和下一轮沿最新检查点恢复是另外的实测断言，不将其描述为直接比较过 confirmation Turn 的完整回执。

实际 PostgreSQL 18.7、两个服务实际 imageID、九个唯一 revision 对象在 15 个 S3 键中，且每次重建前后没有 SQLite/本地对象回退。六个被检查的存储环境变量名均未进入原生执行，完整名字见证据索引；不从这个有限列表推导任意环境变量检查。

同一源码 check/demo 通过。生产代码与 `c383c85` 完全一致，该来源另通过真实 PG/S3 服务回归和完整四 CLI 矩阵；不将其结果改标成本次提供方运行。后续 `b39ad16` 合入 PR27 的差异仅为旧 timeout 单元测试同步修正和文档；该来源通过 fmt/check/demo，实际集群结果仍保持原始来源。

专属节点、外部对照容器及三个本轮镜像标签，按源码实际名称和所有权标签独立确认不存在。第一次独立诊断猜错了两个名称，保留为无效诊断；最终清理证据只使用改正后的实际名称。共享公开服务/依赖缓存保留。

[原始通过记录](evidence/isolated-persistent-provider-raw-2026-11-27.json) · [完整来源、独立核对与日志索引](evidence/isolated-persistent-provider-2026-11-27.json)
