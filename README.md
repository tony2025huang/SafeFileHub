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

## Docker 部署

构建镜像：

```sh
docker build -t safefilehub:test .
```

建议使用只读 root，并挂载一个可写 data volume：

```sh
docker run --read-only --tmpfs /tmp:rw,noexec,nosuid,size=64m \
  --mount type=volume,src=safefilehub-data,dst=/var/lib/safefilehub/data \
  -p 127.0.0.1:8080:8080 safefilehub:test
```

镜像工作目录是 `/var/lib/safefilehub`，默认 data 路径为 `/var/lib/safefilehub/data`，其中包含 objects、staging、archive artifacts 和 `safefilehub.db`。**不要把 staging 单独挂载到其他文件系统**：上传完成依赖同一文件系统上的 atomic rename。SQLite WAL 需要稳定、可写并支持文件锁的 data volume；不建议使用受限 tmpfs 作为生产数据库目录。

systemd 可参考 `deploy/safefilehub.service.example`，运维细节见 `docs/operations.md`。

## TLS、反向代理与运维

不要将服务直接暴露到互联网。应放在 TLS-terminating reverse proxy 后面，在代理层和应用层同时限制 request body，只有可信代理才能传递 client IP，后端监听限制在私有接口。生产环境应监控文件描述符、磁盘空间、RSS、延迟、429/503、取消和 staging cleanup 指标。

备份时同时考虑 SQLite 数据库和对象 data；恢复后先执行一致性检查。发布前保留旧镜像和配置以便 rollback；清理 staging 时只删除过期、取消或确认 orphan 的临时文件，不能删除活跃 session。

## Benchmark 与已知限制

`bench/` 提供有界并发和健康响应 guard，`docs/benchmarks/2026-08-11-baseline.md` 记录本地可复现 baseline。当前结果不是跨网络吞吐承诺；没有隔离 `iperf3` 对端时，不应声称网络优化结论。不要自动修改宿主机 sysctl，也不要在没有数据支持时宣称增加并行度一定提升吞吐。

已知环境限制：Docker daemon 的 layer cache/storage 异常可能在镜像 export 阶段报 `snapshot parent missing`；这不是应用编译错误。只读 root 配合受限 tmpfs data 可能使 SQLite WAL 报 `unable to open database file: out of memory (14)`，生产应使用标准可写 data volume。

## 许可证与贡献

本仓库当前主要用于受控环境中的工程验证。提交代码前请运行完整 race、vet、coverage、前端测试和 build；涉及权限、路径、上传、下载、归档或部署配置的改动必须补充回归测试。
