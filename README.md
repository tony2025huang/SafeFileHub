# SafeFileHub

SafeFileHub 是面向内部或受控环境的 Go 安全文件服务，提供认证、细粒度权限、可恢复上传、文件与目录管理、Range 下载和目录归档。用户逻辑路径不会直接映射为宿主机路径：完成对象使用随机 object key，路径统一经过校验与授权。

## 能力与路由

- 服务端 session、管理员用户与 scoped permission；默认拒绝。
- 分块可恢复上传：创建会话、`HEAD` 查询 offset、`PATCH` 续传、`POST` 完成、SHA-256 校验、fsync 与 atomic rename。
- 文件列表、`GET|HEAD /api/files/{fileID}` 下载、Range、ETag 和安全 `Content-Disposition`。
- 显式目录、零字节文件、非递归删除，以及有界目录 ZIP 归档。
- `/healthz`、`/readyz`、`/metrics`，有界 upload/download/archive 并发控制。

主要 API：

- `POST /login`、`POST /logout`、`GET /session`
- `GET /roots/{rootID}/files`
- `POST /api/uploads`、`HEAD|PATCH|DELETE /api/uploads/{id}`、`POST /api/uploads/{id}/complete`
- `POST /api/files`：创建已发布的零字节文件。JSON：`{"root_id":1,"path":"reports/empty.txt"}`。
- `POST /api/directories`：创建显式目录。JSON：`{"root_id":1,"path":"reports"}`。
- `DELETE /api/files/{fileID}`：删除已发布文件。
- `DELETE /api/directories/{directoryID}`：仅非递归删除；含已发布文件或显式子目录时返回 `409`。
- `DELETE /api/uploads/{id}`：**只取消未完成**的 upload session，不删除已发布文件。
- `POST /api/roots/{rootID}/archives`、`GET|DELETE /api/archives/{jobID}`，以及 `/api/admin/*`。

公开路由（无需登录即可访问）包括 `GET /`、`GET /index.html`、`GET /login`、`GET /login.html`、`GET /app.js`、`GET /healthz`、`GET /readyz`、`GET /metrics`、`GET /api/site-settings`、`GET /assets/site/{assetID}` 和 `GET /favicon.ico`（没有对应品牌资源时可能返回 `404`）。登录页是内嵌的自包含页面；`POST /login` 用于建立 session。文件、上传、归档和管理 API 仍需认证与相应权限。

创建和删除均需对应逻辑路径的权限；路径校验拒绝 traversal、double encoding、反斜杠、NUL/control character、Windows 保留名、危险前缀和符号链接逃逸。具体字段和响应以 `internal/httpapi` 的实现与测试为准。

## 初始管理员

空数据库首次启动时，服务会自动创建一个随机 `sfh-*` 管理员用户名和高强度随机密码，并且**只在该初始化时**输出一次凭据。普通重启不会轮换账户或密码。数据库只保存 Argon2id password hash，不保存明文密码。

必须将进程日志视为凭据载体：首次启动前就保护日志。推荐日志目录由 `root:safefilehub` 管理、仅该组可访问（例如 `0770`），服务以 `safefilehub` 用户运行；日志文件由服务创建为 `0600`。不要把初始凭据写进 unit、镜像、shell history 或版本库。

紧急恢复使用：

```sh
safefilehub --reset-initial-admin
```

此命令只重置 `users.id=1`：启用该用户、生成新的 `sfh-*` 用户名与密码、输出新凭据，然后退出；不会启动 HTTP。它不接受密码参数，且 id 1 不存在时失败。

## CLI

所有 flag 均使用 Go flag 语法（`-flag` 或 `--flag`）。默认配置使用相对 `data/`，因此生产环境应通过 `WorkingDirectory` 控制其解析位置。

| Flag | 默认值 | 限制与行为 |
| --- | --- | --- |
| `--recover-on-start` | `true` | 启动 HTTP 前执行一次有界 upload recovery。 |
| `--recover-only` | `false` | recovery 后退出；必须同时启用 `--recover-on-start`。 |
| `--recover-dry-run` | `false` | 只报告 upload recovery，不修改文件或元数据。 |
| `--recover-limit` | `64` | 每轮最多检查 64 项；范围 `1..64`。 |
| `--reset-initial-admin` | `false` | 重置仅 id 1 的初始管理员后退出，不提供 HTTP。 |
| `--log-path` | 空 | 可选应用日志文件；为空时只写 stderr/stdout。 |
| `--log-format` | `json`（空值也按 JSON） | 仅 `json` 或 `text`。 |
| `--log-retention-days` | `0` | 已轮转日志按年龄保留；`0` 禁用按年龄删除；不得为负。 |
| `--log-max-bytes` | `104857600`（100 MiB） | 当前日志达到此大小前轮转；`0` 禁用按大小轮转；不得为负。 |
| `--log-backups` | `10` | 最多保留此数量的已轮转日志；`0` 禁用按数量删除；不得为负。 |
| `--trusted-proxy-cidr` | 未设置 | 可重复，例如 `--trusted-proxy-cidr=127.0.0.1/32 --trusted-proxy-cidr=::1/128`；只应填后端直连反向代理的 CIDR。 |

应用事件始终写到 stderr；设置 `--log-path` 后，同一日志还会写入该文件（所以是 stderr **加**文件，而非替代）。文件以 `0600` 创建，按 UTC 时间戳命名轮转副本；年龄和数量策略仅清理轮转副本。启动与维护消息也会随 Go 标准日志写到 stderr，并在设置文件路径后镜像到文件。

## 日志与审计覆盖

每个请求有一条终态结构化事件。`login`、`logout`、文件列表、管理员 API，以及认证或授权拒绝均由该终态事件覆盖。upload、download、archive、零字节 file create、directory create、file delete、directory delete 另外分别发出 `*.start` 与 `*.complete`：完成事件记录成功、失败或取消。

日志字段包括 operation、route、status、success、request ID、session audit ID、transfer ID（适用时）、client IP、direct peer IP、user ID（适用时）、字节数和耗时。密码、cookie、`Authorization`、token、请求/文件内容以及敏感 query 字段会被脱敏或排除。管理员的持久审计记录也保留操作、目标和状态，不保存凭据。

## 存储、恢复与备份

生产环境使用持久化 filesystem，**不要使用 `tmpfs`**。同一个 `data/` 目录包含 SQLite、完成对象、`staging` 与 archive 产物；`staging` 和 objects 必须在同一 filesystem，上传完成依赖 atomic rename。SQLite 启用 WAL，底层目录必须稳定、可写并支持文件锁。

上传启动 recovery 有界地核对过期 session、staging 文件和孤儿。已发布对象还有两类恢复：创建零字节对象后元数据写入失败会持久记录 cleanup job；文件删除先写 tombstone，移除对象后才最终删除元数据。启动时的 published recovery 会在限制内重试 cleanup job 和 tombstone，避免崩溃时遗失恢复线索。

备份 SQLite 数据库与完成对象数据时必须作为同一恢复单元，并用 SQLite 支持的备份方式或与服务协调的 snapshot。恢复到隔离环境后，先检查属主/权限，启动 recovery，再做认证、列表、下载与续传校验。

## Docker

`Dockerfile` 构建静态 `safefilehub`，运行镜像基于 Alpine，以 `safefilehub:safefilehub` 用户、工作目录 `/var/lib/safefilehub` 运行；入口是 `/usr/local/bin/safefilehub`，没有额外 entrypoint 脚本或环境变量配置。镜像暴露 `8080`，并声明 `/var/lib/safefilehub/data` 为 volume；相对默认路径会解析为该 data 目录。

```sh
docker build -t safefilehub:test .
docker run -d --name safefilehub --restart unless-stopped \
  --mount type=bind,src=/var/lib/safefilehub/data,dst=/var/lib/safefilehub/data \
  -p 127.0.0.1:8080:8080 \
  safefilehub:test --log-format=json
```

bind mount（或一个覆盖整个 data 目录、支持 SQLite 文件锁的 named volume）必须持久化 SQLite、objects、staging 和 archive。不要单独把 staging 挂载到另一 filesystem。若启用 `--log-path`，其父目录也必须对容器内 `safefilehub` 用户可写；Dockerfile 本身不创建或挂载宿主机日志目录。

## systemd 与反向代理

使用 `deploy/safefilehub.service.example`：复制后按实际二进制、用户、组和目录调整，再执行 `systemctl daemon-reload`、`systemctl enable --now safefilehub`。示例中的日志路径和 `ReadWritePaths` 是配套的；详见 `docs/operations.md`。默认后端监听地址是 `:8080`，生产环境通常让 systemd 服务仅监听 loopback/私网，再由 Nginx 终止 TLS 并反向代理到 `http://127.0.0.1:8080`。若需要临时公网验证，公网暴露端口必须由部署环境的端口映射、防火墙或负载均衡配置决定，不应把某个临时端口写死在应用或文档中；验证结束后应立即撤销暴露。

将服务绑定在 loopback 或私网地址，TLS 在 Nginx/Caddy/Envoy 或托管负载均衡器终止。只有 direct peer IP 属于某个 `--trusted-proxy-cidr` 时，服务才解析 `X-Forwarded-For`：从右向左跳过可信代理，采用最接近可信代理的第一个非可信 IP；若没有该值才使用有效 `X-Real-IP`，否则使用 direct peer。未受信任 direct peer 提供的转发头一律忽略。

Nginx 示例：

```nginx
location / {
    proxy_pass http://127.0.0.1:8080;
    proxy_http_version 1.1;
    proxy_request_buffering off;
    proxy_read_timeout 30m;
    proxy_send_timeout 30m;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_set_header X-Forwarded-Host $host;
}
```

## 开发检查

本地门禁使用 Go 1.24 工具链（不要让命令隐式下载或切换工具链）：

```sh
GOTOOLCHAIN=local /usr/local/go/bin/go version  # 应为 go1.24.x
GOTOOLCHAIN=local /usr/local/go/bin/go test ./... -race -count=1
GOTOOLCHAIN=local /usr/local/go/bin/go vet ./...
GOTOOLCHAIN=local /usr/local/go/bin/go test ./... -cover
npm test
npm run build
git diff --check
```

`bench/` 提供有界并发与健康检查 guard；不要把本地 benchmark 结果当作跨网络吞吐承诺，也不要未经隔离压测就修改宿主机 sysctl。
