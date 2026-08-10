# SafeFileHub 设计文档

- 日期：2026-08-10
- 状态：设计已确认，待实现
- 参考：CHFS（CuteHttpFileServer）公开 README、版本说明、公开 issue/崩溃栈；CHFS 核心服务端源码未公开，因此关于其内部实现的结论按证据等级标注为推断。

## 1. 目标与非目标

SafeFileHub 是一个单机优先、可跨平台部署的安全文件服务，保留 CHFS 的单文件/低依赖/浏览器访问优势，重点提供：

1. 文件和目录上传、下载；
2. 对特殊字符开头名称的可配置限制；
3. 上传和下载断点续传；
4. 用户管理与按共享根目录相对路径的读取范围；
5. 高带宽 TCP 场景下的可控并发、背压和可观测性。

第一版不实现在线预览、分享链接和 WebDAV；它们留作后续扩展，以降低权限面和后台资源竞争风险。

## 2. CHFS 分析与卡死风险

CHFS 的明显优势是单二进制、跨平台、配置简单、Web UI 简洁、支持多目录共享、账户权限、地址过滤、二维码/移动端和 WebDAV。公开崩溃栈显示其使用 Go `net/http`，入口包含 `net/http.(*conn).serve` 和权限路由；公开材料还显示存在后台预览目录扫描/图片缩略图路径。

由于核心服务端源码未公开，无法逐行确认根因。多文件上传后卡死需要优先验证以下假设：

- multipart 或请求体解析把多个大文件放入内存/临时文件，缺少严格的全局和单用户背压；
- 同步磁盘 I/O、目录递归扫描或全局锁占用 HTTP handler；
- 上传完成后后台图片预览任务与上传争用内存、CPU 和磁盘；高像素图片解码可能造成内存尖峰、GC 抖动或 OOM；
- 文件名中的 `+` 等字符在 form/query 解码中被错误当作空格，导致单文件永远 pending；批量前端队列等待该任务时表现为整体卡住；
- 临时文件、文件描述符或失败上传状态未释放；
- 同名并发写入、客户端断开取消和错误路径清理不完整。

复现必须在隔离 VM/容器进行，采集 RSS、CPU、GC、goroutine、FD、磁盘延迟、临时文件、健康接口 p95/p99 和完整崩溃栈；不得直接压测承载真实数据的 CHFS。

## 3. 技术栈与部署

- 后端：Go，标准 `net/http`，路由可用 chi；
- 元数据：SQLite，短事务，WAL 模式；
- 前端：原生 TypeScript；
- 存储：本地文件系统，正式对象使用随机 object ID；
- 认证：Session Cookie + Argon2id；
- 部署：单二进制和 Docker 两种方式；
- 初始服务监听由反向代理/系统服务负责 TLS，应用自身也可配置 TLS。

## 4. 存储与路径安全

用户看到的是共享根目录下的逻辑相对路径，如 `/public`、`/projects/a`，不暴露本机绝对路径。逻辑路径与物理对象分离：

```text
逻辑：/projects/a/report.pdf
物理：data/objects/01/01J...random-id
```

文件显示名只存 SQLite；临时上传使用 `data/staging/<upload-id>.part`，完成时校验、`fsync` 后在同一文件系统内原子 rename，再以短 SQLite 事务提交元数据。

路径处理只允许一次 URL 解码，然后做 Unicode NFC 规范化、路径段校验和权限检查。拒绝 `.`、`..`、NUL、控制字符、反斜杠、绝对路径、Windows 盘符、无效 percent encoding、双重编码和符号链接逃逸。物理路径解析必须使用根目录约束，不能仅依赖字符串前缀。

## 5. 名称策略

默认拒绝以 `.`, `~`, `$`, `#` 开头的文件/目录；始终拒绝 `.`、`..`、控制字符、空名称、末尾空格/句点和 Windows 保留名（CON、PRN、AUX、NUL、COM1-COM9、LPT1-LPT9）。策略可配置，但不可关闭路径穿越、NUL、绝对路径和符号链接逃逸防护。

`+`, `#`, `%`, `?`, 空格、中文和 Emoji 作为名称内容允许（若未命中前导规则）；URL 中必须按 path segment 使用 `encodeURIComponent`，不把名称放入 query string，也不进行二次解码。

## 6. 用户与权限

角色：`admin`、`manager`、`writer`、`reader`，匿名 guest 可关闭。动作拆分为 `list/read/write/mkdir/delete/archive/manage`。权限模型为 principal × storage-root × logical-path-prefix × action；按最长匹配路径继承，显式 deny 优先，默认拒绝。

所有目录列表、文件下载、Range 下载、分片上传、目录上传、目录归档、删除、重命名和未来 WebDAV 都必须通过同一个 `authorize(user, logicalPath, operation)`。

核心表：`users`、`storage_roots`、`permissions`、`files`、`upload_sessions`、`audit_events`。密码只保存 Argon2id 哈希，管理员密码不进入配置明文。

## 7. 上传协议与断点续传

不把多个文件作为一个大 multipart 请求处理。前端为每个文件建立独立 upload session，目录上传额外携带 `webkitRelativePath`，服务端逐文件校验父目录。

```http
POST  /api/uploads
HEAD  /api/uploads/{id}
PATCH /api/uploads/{id}       Content-Type: application/offset+octet-stream
POST  /api/uploads/{id}/complete
DELETE /api/uploads/{id}
```

创建任务携带逻辑路径、总大小、可选 SHA-256；分片上传携带 `Upload-Offset`。offset 不匹配返回 409；完成时校验大小、hash、分片完整性，`fsync`、原子提交。服务重启后扫描并恢复未过期 session；TTL 到期、取消或失败任务由 GC 安全清理。

初始默认限制：全局上传 16、单用户 4、单 IP 8、chunk 8 MiB（可在 4-16 MiB 调整）、上传空闲超时 30 分钟、session TTL 24 小时。达到上限立即 429/503 + Retry-After，不无限排队。

## 8. 大带宽 TCP 传输设计与评估

### 8.1 先区分瓶颈

带宽利用率不由线程数单独决定。实际上限取决于 `min(网络带宽、TCP/QUIC 栈、TLS、磁盘读写、CPU、单连接拥塞窗口、服务限流)`。每个连接先用流式 I/O 跑满；只有单连接受 RTT/拥塞窗口限制或浏览器连接数不足时，才用有限并行分片提升吞吐。

### 8.2 上传并行模型

- 一个文件由浏览器拆成 8-16 MiB 分片；同一文件默认并发 2-4，可按 RTT、带宽和磁盘动态调节，硬上限 8；
- 多文件默认最多 4 个活跃 session，不能让“文件数 × 分片数”无限放大；
- 一个 upload session 的 offset 由单写入 owner 串行维护，避免同一临时文件的随机并发写竞争；
- 若未来要并行同一大文件，改为每分片独立临时对象（`staging/<id>/<index>`），完成时按顺序合并/校验，而不是多个 goroutine 共享一个 file offset；第一版优先单文件顺序写，稳定性优先；
- 使用连接复用（HTTP/2 或 keep-alive），避免每个分片新建 TCP/TLS 连接；
- 每个请求采用有限内存 buffer（64 KiB-1 MiB），不把分片或文件全部读入内存；
- 对本机同盘使用顺序写、批量 flush；不在每个小块上调用 fsync；完成边界再 fsync；
- 对客户端断开传递 context cancel，立即停止后续读取和写入。

### 8.3 下载并行模型

- 普通文件优先单连接 HTTP Range，服务端流式发送；
- 客户端可把大文件分为 4-8 个 Range，但由客户端合并，服务端对用户/IP/全局设置下载并发上限；
- 不建议服务端为一个下载主动创建多个 TCP 连接；这会放大磁盘随机读和拥塞公平性问题；
- 使用 `io.Copy`/`sendfile`（在 TLS、压缩或平台不支持时自动回退 buffered copy），避免无意义的数据拷贝；
- 设置 `TCP_NODELAY` 仅适用于小控制响应，不用于大文件流；大文件主要依靠较大的 socket buffer、持续写入和内核拥塞控制；
- HTTP/2 多路复用用于控制请求和小文件；大文件数据流仍需限流，避免单连接独占全部带宽；
- HTTP/3/QUIC 作为可选后续评估项，不能在 MVP 中为了“多线程”引入额外部署复杂度。

### 8.4 TCP 与系统参数

应用只设置可安全、可移植的 socket 选项，并提供指标；不在安装时擅自修改宿主机 sysctl。部署文档可建议管理员按实际链路评估：拥塞控制（如 BBR/CUBIC）、`rmem/wmem`、`somaxconn`、文件描述符、NIC offload 和 MTU。任何 sysctl 修改必须由运维明确执行并先备份现状。

### 8.5 背压和带宽整形

采用 token bucket：

```yaml
bandwidth:
  global_mbps: 0       # 0 表示不设应用层总上限
  per_user_mbps: 0
  per_ip_mbps: 0
  burst_seconds: 2
```

网络写入、磁盘读取和上传分片队列都必须有界；队列满时暂停当前流或返回 429，而不是创建无界 goroutine。目标是“带宽跑满但 `/healthz` 仍可响应”。

### 8.6 验收基准

在隔离环境用 `iperf3` 建立网络基线，再用本系统测试 1/2/4/8 并发、RTT 1/20/80ms、SSD/NAS、HTTP/1.1/HTTP/2、TLS 开关。记录吞吐、CPU、RSS、磁盘等待、健康接口 p99、失败率和重传情况。验收不是“线程越多越快”，而是：达到目标带宽的同时健康接口 p99、RSS 和 FD 在预算内，且并发继续增加时服务优雅拒绝而非卡死。

## 9. 下载与目录归档

文件下载支持 Range、ETag、If-Range、Last-Modified 和 Content-Disposition。目录下载使用异步 ZIP/TAR 任务，设置文件数、估算总大小、超时、并发和 TTL；客户端断开时取消，不在 HTTP handler 内无界递归压缩。

## 10. 并发安全与可观测性

- 磁盘 I/O 不持有全局锁；SQLite 事务短小；
- 上传、下载、归档、缩略图未来使用独立 worker pool；
- `http.MaxBytesReader`、读写超时和请求取消必须启用；
- `/healthz` 不依赖数据库全量扫描或目录递归；`/readyz` 检查 DB、存储根和磁盘余量；
- 指标：活跃上传/下载/归档、队列长度、429/503、RSS、FD、磁盘余量、I/O 延迟、p95/p99、staging 数量和清理数量；
- audit 记录登录、权限拒绝、上传完成/取消、删除和管理员操作，不记录密码或文件内容。

## 11. 测试和验收

安全：路径穿越、双重编码、反斜杠、绝对路径、符号链接、Unicode NFC、保留名、无权限 API、归档越界。

上传：1/2/4/8/16 文件、256 MiB/1 GiB/10 GiB、中断恢复、重复分片、offset 错误、同名并发、重启恢复、磁盘满、特殊名称和深层目录。

传输：RTT/带宽矩阵、HTTP/1.1 与 HTTP/2、TLS、单连接与 Range 并行、SSD/NAS；压测期间 `/healthz` 必须持续响应。

验收标准：单文件失败不影响其他任务；正式文件永不暴露半成品；恢复后 SHA-256 一致；越权全部拒绝；资源超限返回明确 429/503；高带宽场景达到链路可用吞吐而不发生无界资源增长。

## 12. 阶段计划

1. 仓库骨架、配置、日志、healthz；
2. 用户、Session、Argon2id、权限；
3. 逻辑路径和名称策略；
4. 目录浏览；
5. 单文件分片上传、恢复、清理；
6. HTTP Range 下载；
7. 多文件/目录上传；
8. 目录归档；
9. 管理后台、限流、指标；
10. TCP/磁盘基准与压力测试；
11. Docker、单二进制和部署文档。

## 13. 实现前置决策

读取范围已确定为共享根目录相对路径。后续实施默认采用 Go + SQLite + 本地对象存储、单文件顺序分片写入 + 有限多文件并发；先以稳定性和可观测性为前提，再用基准数据调整并发和 chunk 参数。
