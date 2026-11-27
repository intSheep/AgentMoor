# 签名密钥轮换与单凭据撤销

这套策略由可信控制面管理。客户代码、第三方 Skill 和 Worker 不应持有策略文件；Worker 只拿自己的短期执行凭据。管理命令不增加租户可调用的管理员 HTTP 接口。

## 从旧签名密钥迁移

保留旧凭据时，显式导入现有 32 字节 `signing.key`。策略的父目录应已经存在，只有管理员和 API 服务可访问。命令只输出公开的 key ID，不打印密钥。

```sh
umask 077
./bin/agentmoor auth-init \
  --policy /srv/agentmoor/platform/auth-policy.json \
  --from-key /srv/agentmoor/platform/signing.key --key-id initial
```

新安装可以省略 `--from-key`，生成全新密钥，并拒绝所有旧格式凭据。已有策略不会被 `auth-init` 覆盖。

首次启用需要给服务加上参数并重启；此后轮换、撤销和退休由每次请求重新读取策略生效：

```sh
./bin/agentmoor serve --data /srv/agentmoor/platform \
  --auth-policy /srv/agentmoor/platform/auth-policy.json
```

启动时策略无效会拒绝启动；已运行服务每次鉴权重新读取，启用后不会回退到 `signing.key`。未加 `--auth-policy` 的旧服务仍只用启动时加载的单密钥，**不会参与策略撤销**；迁移时须确认所有服务入口已启用该参数。此处没有多副本一致性或生产 HA 的承诺。

## 签发和更新客户端文件

签发也显式使用同一份策略。新凭据包含 MAC 覆盖的版本、key ID 和随机 credential ID；即使权限和到期时间完全相同，两张新凭据也能分别撤销。客户端继续把它当作不透明字符串使用。

```sh
./bin/agentmoor issue \
  --auth-policy /srv/agentmoor/platform/auth-policy.json \
  --tenant team --project demo --runtime primary --adapter codex \
  --execute --ttl 1h > /run/agentmoor/primary.token.next
mv /run/agentmoor/primary.token.next /run/agentmoor/primary.token
```

输出文件的目录也应仅供相应管理员和 Worker 使用。已有文件客户端每次请求重读凭据；在同一目录原子替换文件，避免读取半张凭据。SaaS 用 `--submit`，回收工具用 `--gc`，能力组合见[运行手册](operations.md#2-给-worker-分配身份和目录)。有效期仍最多 24 小时。

## 轮换、撤销、退休

```sh
# 新签发开始使用 next，旧 key 暂留用于验证已有客户端。
./bin/agentmoor auth-rotate \
  --policy /srv/agentmoor/platform/auth-policy.json --key-id next

# 用上述 issue 流程为客户端续签并原子替换各自文件。

# 单独撤销一张仍有效的凭据；内容只从文件读取，不放进命令参数。
./bin/agentmoor auth-revoke \
  --policy /srv/agentmoor/platform/auth-policy.json \
  --token-file /run/agentmoor/compromised.token

# 确认客户端已迁移后，移除旧验证 key。
./bin/agentmoor auth-retire \
  --policy /srv/agentmoor/platform/auth-policy.json --key-id initial
```

轮换保留新旧 key 的重叠期，不自动判断客户端是否迁移完成。退休立即拒绝该 key 签发的所有凭据，包括显式导入的旧格式；不能退休当前 active key。`auth-revoke` 对同一张仍有效凭据重复执行是幂等的，存储完整 token 的 SHA-256 和到期时间，不存储 bearer 原文。旧格式若字节完全相同，就是同一张凭据；随机 ID 解决的是新格式的独立签发问题。

撤销与退休影响之后的鉴权。已经接受的请求、进行中的 Agent 和外部副作用不会因此自动终止或回滚；任务取消和执行区生命周期另行管理。

## 文件、故障与容量

策略文件包含密钥，应按秘密管理；不要提交仓库、打印正文或挂进执行区。管理命令通过同目录临时文件、文件同步、原子发布和父目录同步保存，更新使用固定锁 inode 的系统文件锁串行处理。锁等待最多 10 秒；进程退出后锁由内核释放，**不要删除 `.lock` 文件来恢复锁**。策略路径的父目录须受信，不能供客户代码写入。

服务每次读取最多 1 MiB，允许最多 16 个验证 key、4096 条尚未清理的撤销记录。管理更新会清理已过期撤销记录，不会为腾位置丢弃仍有效的撤销。超过限制或管理失败时不应假定操作已完成；用实际客户端请求确认授权状态。策略缺失、损坏或不支持时关闭访问，不缓存旧策略继续放行；带 Bearer 的请求得到通用 503，失效/伪造/撤销凭据得到 401，缺少操作能力仍是 403。

服务读取允许 Kubernetes Secret 投影所用的符号链接，但管理命令不修改投影文件或通过符号链接更新策略。可在受信管理侧更新后再部署 Secret；生效时间取决于挂载文件何时更新，不能把本机文件原子替换当作集群同步传播证明。

## 2026-11-27 实际验证

实现源码 `e3738562d3d607c4dd963f7d66cb2885fa8c3619` 的 focused race、`make fmt`、`make check`（含全包 race）和 `make demo` 均通过。后续只补约束、手册和证据，不冒称又重跑原生 CLI 或沙箱。[测试摘要](evidence/authentication-policy-2026-11-27.json)

[平台真实 HTTP 测试](../internal/platform/auth_policy_test.go)覆盖不重启服务的轮换重叠/退休、两张同权限同期限凭据的独立撤销、旧格式的显式迁移及撤销、文件客户端原子换新、22 条路由在撤销/坏策略时拒绝、FIFO/投影链接、并发更新和持锁进程退出。[CLI 测试](../cmd/agentmoor/auth_commands_test.go)实际启动策略服务，并确认不需要 `signing.key`、管理输入只经文件、错误不回显秘密。格式负例还验证规范 Base64、未知/空 selector、大小写与 Unicode JSON 字段别名，带有效合成 MAC 的解析边界也必须拒绝。

本项验证密钥与凭据生命周期，不代表已具备不可信代码隔离。[执行边界与剩余验收](security-boundary.md)
