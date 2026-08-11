# SafeFileHub 运维指南

## 部署边界与 trusted proxy

在维护中的反向代理（Nginx、Caddy、Envoy 或托管 LB）终止 TLS，HTTP 重定向到 HTTPS。SafeFileHub 应绑定 loopback 或私网地址，不应直接暴露到公网；以非 root 用户、限制性 umask 运行。

只在 SafeFileHub 的 **direct socket peer** 属于重复指定的 `--trusted-proxy-cidr` 之一时，才信任转发地址。收到可信 peer 的请求时，服务从 `X-Forwarded-For` 最右侧开始，跳过可信代理 hop，采用第一个非可信 IP；没有可用 XFF 时才使用合法的 `X-Real-IP`，仍没有时使用 direct peer。来自不可信 direct peer 的 `X-Forwarded-*` 与 `X-Real-IP` 均被忽略。因此 CIDR 只能填写实际会连接后端的代理网段，不能填写客户端网段或过宽网段。

Nginx 至少应传递：

```nginx
proxy_set_header X-Real-IP $remote_addr;
proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
```

并以 `--trusted-proxy-cidr=127.0.0.1/32`、`--trusted-proxy-cidr=::1/128` 或实际代理网段启动服务。

## 持久化存储与备份

`data/` 是一个恢复单元：含 SQLite 元数据、完成 objects、`staging` 和 archive artifacts。必须使用本地、持久、可写并支持文件锁的 filesystem；生产环境不得使用 `tmpfs`。`staging` 必须和完成对象在同一 filesystem，完成上传依赖 atomic rename。SQLite 尝试启用 WAL，因此 WAL/SHM 文件也需要稳定目录和正确权限。

建议目录由服务账号私有持有：

```sh
install -d -o safefilehub -g safefilehub -m 0700 /var/lib/safefilehub/data
install -d -o root -g safefilehub -m 0770 /var/log/safefilehub
```

SQLite 数据库与完成对象必须一起备份，使用 SQLite 支持的备份方式或与服务协调的 filesystem snapshot。先在隔离环境恢复该配对：检查 ownership/mode，启动 recovery，验证 `/healthz`、认证、列表、下载与 checksum 已验证的续传；不要将未经验证的备份覆盖生产。

## 初始管理员与日志

空数据库第一次启动自动生成随机 `sfh-*` 管理员用户名和高强度密码，并只输出一次。普通重启不会轮换；数据库只保存 Argon2id hash。初始凭据会出现在进程初始化日志中，必须在首次启动前保护日志文件和 journal 访问权限。

`--reset-initial-admin` 是 break-glass 操作：它仅重置 `users.id=1`、启用该账号、打印新的随机凭据，然后退出，绝不会启动 HTTP；id 1 不存在时失败。

应用事件总是写 stderr。设置 `--log-path=/var/log/safefilehub/app.json` 后，同一事件也写文件；服务创建该文件为 `0600`。推荐：

```sh
--log-format=json \
--log-path=/var/log/safefilehub/app.json \
--log-max-bytes=104857600 \
--log-retention-days=30 \
--log-backups=10
```

`--log-format` 只接受 `json|text`（默认 JSON）；`--log-retention-days=0` 和 `--log-max-bytes=0` 分别关闭按年龄、按大小轮转；`--log-backups=0` 关闭按数量清理，三个数值均不能为负。轮转副本使用 UTC 时间戳，仅轮转副本会被年龄/数量策略删除。没有 `--log-path` 时仅使用 stderr/journal 或容器日志。

## 日志与审计矩阵

| 操作 | 记录 |
| --- | --- |
| login、logout、文件列表、管理员 API、认证/授权拒绝 | 一条请求终态事件 |
| upload、download、archive | `*.start` 与 `*.complete`，另有请求终态事件 |
| 零字节 file create、directory create、file delete、directory delete | `*.start` 与 `*.complete`，另有请求终态事件 |

终态/完成记录带 operation、route、status、success、request/session/transfer correlation ID（适用时）、client IP、peer IP、user ID（适用时）、bytes、duration。密码、cookie、Authorization、token、请求及文件内容、敏感 query 不写入应用日志；管理员的持久 audit 也不保存凭据。

## Recovery、cleanup 与回滚

默认启动前执行一次有界 upload reconciliation。检查但不改写：

```sh
safefilehub --recover-on-start=true --recover-only --recover-dry-run --recover-limit=64
```

实际有界 cleanup：

```sh
safefilehub --recover-on-start=true --recover-only --recover-limit=64
```

limit 范围为 `1..64`。不要用宽泛 shell 删除 staging。upload recovery 仅在有界范围内核对 session metadata、regular files、offset、expiry 和 lifecycle lock。

已发布对象恢复同样在启动期间运行：零字节对象已创建但元数据写入失败时，durable cleanup job 会记录待删 object；删除文件先将文件标记为 tombstone，移除 object 后才 finalize 元数据。启动 recovery 会重试这两类工作，因此错误或崩溃时不要手工删除对象、tombstone 或 cleanup 记录。

回滚步骤：

1. 在反向代理停止新流量。
2. 保留当前 binary/image 与 data，收集 health、磁盘和已脱敏应用日志。
3. 部署之前验证过的 binary/image，优雅重启。
4. 执行有界 recovery，验证 health、认证列表/下载和续传 checksum。
5. 若元数据/对象一致性可疑，先在隔离环境恢复匹配的 SQLite/object 备份；不要直接删数据。

## 监控与发布门禁

监控 `/healthz`、`/readyz`、FD 与 `LimitNOFILE`、磁盘/ inode/IO 等待、SQLite 错误、staging 数量/年龄、RSS、429/503、取消与 cleanup 指标。staging 持续增长通常表示中断 session、cleanup 故障或容量不足，应先保留证据并运行有界 recovery。

发布前：

```sh
GOTOOLCHAIN=local /usr/local/go/bin/go test ./... -race -count=1
GOTOOLCHAIN=local /usr/local/go/bin/go vet ./...
GOTOOLCHAIN=local /usr/local/go/bin/go test ./... -cover
npm test
npm run build
git diff --check
docker build -t safefilehub:test .
```

仅在隔离 benchmark 环境评估 BBR/CUBIC；不要以本地测试替代生产网络结论，也不要为测试填满生产 filesystem。
