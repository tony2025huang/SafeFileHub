# SafeFileHub

SafeFileHub 是一个基于 Go 的安全文件传输服务，面向需要认证、细粒度权限和高并发传输的内部或受控场景。项目使用逻辑路径、随机对象键、可恢复上传、原子发布、HTTP Range 下载和临时目录归档任务，避免把用户输入直接映射为宿主机路径。

## 项目定位与能力

- 用户认证、服务端 session 和管理员 API。
- 基于 storage root、逻辑路径前缀和 action 的权限控制，默认拒绝。
- 文件列表、单文件下载、HTTP Range/HEAD、ETag 和安全文件名。
- 分块可恢复上传：HEAD 查询 offset、PATCH 续传、SHA-256 校验、fsync 和 atomic rename。
- 多文件及目录上传 UI，目录相对路径和特殊文件名保持不变。
- 有界 upload/download/archive 并发控制，不创建无界等待队列。
- 目录 ZIP 归档：逐路径授权、文件数/估算大小限制、流式写出、取消和 TTL 清理。
- `/healthz`、`/readyz`、`/metrics` 以及生产组合入口。
- 安全测试、压力 guard、Docker 镜像和 systemd 部署示例。

当前 MVP 不包含公共分享链接、WebDAV、预览渲染或外部部署自动化。

## 架构与安全边界

服务使用 `net/http`、SQLite 元数据和 storage root。用户看到的是逻辑路径；物理对象使用随机 object key，上传 staging、对象和归档文件位于受控 data 目录。路径在统一的 `pathpolicy` 中校验，拒绝 traversal、double encoding、反斜杠、NUL/control character、Windows 保留名、危险前缀和符号链接逃逸。

授权默认拒绝，下载和归档会先执行 session/permission 检查，未授权对象不泄露存在性。对象打开使用 descriptor-relative 和 `O_NOFOLLOW` 等保护；归档 job 还会校验创建者，避免猜测 job ID 跨用户访问。

生产入口为 `httpapi.NewProductionServer`，统一组合认证、session、文件列表、上传、下载、归档、管理员 API、健康检查、metrics 和静态 UI，并共享同一个 session、authorizer、metrics 与并发 limiter。

## 主要路由

- `GET /healthz`：浅 liveness 检查，不扫描目录。
- `GET /readyz`：数据库、已持有 storage descriptor 和 storage path 的常量开销检查，不扫描目录。
- `GET /metrics`：固定且有界的指标，不使用任意路径、用户、IP、密码或文件内容作为 label。
- `POST /login`、`POST /logout`、`GET /session`：认证和 session 生命周期。
- `GET /roots/{rootID}/files`：授权后的逻辑文件列表。
- 上传 API：创建 session、`HEAD` offset、`PATCH` 分块、取消和完成。
- `GET|HEAD /api/files/{fileID}`：流式文件下载与 Range 请求。
- `POST /api/directories`：创建显式空目录；`DELETE /api/files/{fileID}`、`DELETE /api/directories/{directoryID}`：删除已发布对象（目录仅非递归删除）。
- 归档 API：创建、下载和取消临时目录归档任务。
- `/api/admin/*`：管理员用户、密码、禁用状态和 scoped permission 管理。

具体字段和响应以 `internal/httpapi` 实现及测试为准。

## 运行要求

- Go **1.24**；本地环境可使用 `GOTOOLCHAIN=local` 避免自动下载 toolchain。
- Node.js 24（前端测试和语法构建检查）。
- SQLite 所需的可写 data 目录。
- Docker（可选）或 systemd（可选）。

## 本地开发与测试

```sh
GOTOOLCHAIN=local /usr/local/go/bin/go test ./... -race -count=1
GOTOOLCHAIN=local /usr/local/go/bin/go vet ./...
GOTOOLCHAIN=local /usr/local/go/bin/go test ./... -cover
npm test
npm run build
go test ./... -bench . -benchmem
```

不要在测试或日志中输出密码、session secret、请求 body、文件内容或 token。benchmark 只应使用本地/隔离环境中的临时数据。

## 生产部署

生产环境必须使用持久化的宿主机 filesystem，不要使用 `tmpfs`。建议将宿主机目录设为 `/var/lib/safefilehub/data`；它包含 SQLite 数据库、objects、`staging` 和 archive artifacts。`data/staging` 必须与 data 中的对象存储位于**同一文件系统**，因为上传完成依赖 atomic rename。SQLite WAL 还需要稳定、可写并支持文件锁的目录。

创建目录后，应由运行服务的非 root 用户拥有，并限制权限：

```sh
install -d -o safefilehub -g safefilehub -m 0700 /var/lib/safefilehub/data
```

### Docker

构建镜像：

```sh
docker build -t safefilehub:test .
```

生产运行示例（将服务仅发布到本机，由反向代理访问）：

```sh
docker run -d --name safefilehub --restart unless-stopped \
  --read-only \
  --mount type=bind,src=/var/lib/safefilehub/data,dst=/var/lib/safefilehub/data \
  -p 127.0.0.1:8080:8080 \
  safefilehub:test
```

镜像工作目录是 `/var/lib/safefilehub`，默认 data 路径为 `/var/lib/safefilehub/data`。以上命令不使用 `tmpfs`；持久化 bind mount 同时保存数据库、对象、`staging` 和归档产物。若改用 Docker named volume，也必须只将整个 data 目录挂载为一个稳定的、支持 SQLite 文件锁的 volume，不能把 `staging` 挂载到另一文件系统。

### systemd

以 `deploy/safefilehub.service.example` 为基础：复制到 `/etc/systemd/system/safefilehub.service`，按注释设置二进制路径、`User`/`Group`，并保持 `WorkingDirectory=/var/lib/safefilehub`、`ReadWritePaths=/var/lib/safefilehub/data` 与该持久化 data 目录一致。示例已通过 `-recover-on-start=true` 启动服务。随后执行：

```sh
systemctl daemon-reload
systemctl enable --now safefilehub
systemctl status safefilehub
```

运维恢复、备份和回滚细节见 `docs/operations.md`。

## Nginx HTTPS reverse proxy

不要将服务直接暴露到互联网。将 SafeFileHub 绑定到 `127.0.0.1:8080`（Docker 示例通过端口映射实现），并在 Nginx 终止 TLS。当前 HTTP 路由没有 WebSocket endpoint，因此配置中不添加 WebSocket upgrade headers。

以下为一个站点配置示例；替换域名与证书路径占位符：

```nginx
upstream safefilehub {
    server 127.0.0.1:8080;
}

server {
    listen 80;
    listen [::]:80;
    server_name files.example.com;
    return 301 https://$host$request_uri;
}

server {
    listen 443 ssl;
    listen [::]:443 ssl;
    server_name files.example.com;

    ssl_certificate     /path/to/fullchain.pem;
    ssl_certificate_key /path/to/privkey.pem;

    access_log /var/log/nginx/safefilehub.access.log;
    error_log  /var/log/nginx/safefilehub.error.log warn;

    # Match the application's per-PATCH maximum request body (64 MiB).
    client_max_body_size 64m;
    # The application permits 30 minutes of inactivity while receiving an upload.
    client_body_timeout 30m;

    location / {
        proxy_pass http://safefilehub;
        proxy_http_version 1.1;
        # Stream resumable upload PATCH bodies instead of buffering them on Nginx disk.
        proxy_request_buffering off;
        proxy_read_timeout 30m;
        proxy_send_timeout 30m;

        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-Host $host;
    }
}
```

SafeFileHub 当前仍以 socket peer address 识别客户端，未配置 trusted-proxy support；因此它不会使用上述 `X-Forwarded-*` 头部来决定 client IP。只允许可信代理连接后端。

## 初始管理员与密码

代码中没有默认用户名或默认密码。当前生产入口也没有 CLI、环境变量或无需认证的 API 来创建第一个用户；`POST /api/admin/users` 只能由已有 session 且用户名已列在 `config.Config.AdminUsernames` 中的管理员调用。默认 `AdminUsernames` 为空，且 `cmd/safefilehub/main.go` 直接使用 `config.Default()`，没有提供设置该 bootstrap 列表的运行时配置接口。

因此，**首次管理员初始化目前是部署缺口**：不能按 README 编造一个可用的首建命令或 API。上线前应补充并审计一个受控 bootstrap 机制；在此之前，空数据库不能通过当前公开 CLI/API 完成首次管理员创建。后续管理员创建接口实际为 `POST /api/admin/users`，JSON 字段为 `{"username":"...","password":"..."}`；密码重置为 `PUT /api/admin/users/{userID}/password`，JSON 字段为 `{"password":"..."}`，两者都需要已认证管理员。

部署实现 bootstrap 后，使用安全随机、仅一次传递的初始密码，例如：

```sh
openssl rand -base64 32
```

不要把密码写入 systemd unit、shell history、镜像、日志或版本库。首次登录后立即通过受控管理员流程重置初始密码，并按最小权限创建其他用户与 scoped permissions。

## 日志与运维

应用当前没有独立文件日志：它使用 Go 标准日志写入 stdout/stderr。使用 systemd 时通过以下命令查看：

```sh
journalctl -u safefilehub
```

使用 Docker 时查看容器 stdout/stderr：

```sh
docker logs safefilehub
```

Docker 默认 `json-file` logging driver 的宿主机日志文件通常是 `/var/lib/docker/containers/<container-id>/<container-id>-json.log`；该路径由 Docker daemon 的 data root 和 logging driver 配置决定，不能假定所有部署均相同。以 `docker inspect -f '{{.LogPath}}' safefilehub` 或实际 daemon 配置为准，生产环境应配置日志轮转。Nginx access/error log 建议分别使用 `/var/log/nginx/safefilehub.access.log` 和 `/var/log/nginx/safefilehub.error.log`，如上例所示。

生产环境应监控文件描述符、磁盘空间、RSS、延迟、429/503、取消和 staging cleanup 指标。备份时同时考虑 SQLite 数据库和对象 data；恢复后先执行一致性检查。发布前保留旧镜像和配置以便 rollback；清理 staging 时只删除过期、取消或确认 orphan 的临时文件，不能删除活跃 session。

## Benchmark 与已知限制

`bench/` 提供有界并发和健康响应 guard，`docs/benchmarks/2026-08-11-baseline.md` 记录本地可复现 baseline。当前结果不是跨网络吞吐承诺；没有隔离 `iperf3` 对端时，不应声称网络优化结论。不要自动修改宿主机 sysctl，也不要在没有数据支持时宣称增加并行度一定提升吞吐。

已知环境限制：Docker daemon 的 layer cache/storage 异常可能在镜像 export 阶段报 `snapshot parent missing`；这不是应用编译错误。生产 data 目录必须是持久化、可写并支持 SQLite 文件锁的 filesystem。

## 许可证与贡献

本仓库当前主要用于受控环境中的工程验证。提交代码前请运行完整 race、vet、coverage、前端测试和 build；涉及权限、路径、上传、下载、归档或部署配置的改动必须补充回归测试。

### Initial administrator and logging

On the first start against an empty database, SafeFileHub creates a random `sfh-…` administrator username and a high-entropy password. The credentials are emitted **once** to the process initialization log; retain them securely. Passwords are stored only as Argon2id hashes. Normal restarts do not rotate the account.

To recover access, run the service binary with `--reset-initial-admin`. It resets only `users.id=1`, enables it, prints a fresh random username/password, and exits without serving HTTP. It accepts no password argument and fails if id 1 is absent.

Use `--log-path`, `--log-format=json|text`, and `--log-retention-days` for production log configuration. Application logs deliberately omit passwords, cookies, Authorization headers, request/file contents, tokens, and query values. When reverse-proxying, configure `trusted-proxy-cidr` only for actual proxy networks; untrusted forwarded headers are ignored.
