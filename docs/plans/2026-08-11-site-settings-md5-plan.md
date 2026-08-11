# SafeFileHub MD5 与站点外观配置实施计划

> 按 TDD 执行；每个任务完成后运行对应 focused tests，再进入下一任务。子任务不得自行 commit/push，父任务统一审查。

## Task 1：数据库模型与 migration

- 新增 `site_settings` 单例表及默认值：site name、primary color、备案开关/文本、MD5 开关、三类 asset 引用。
- 新增 MD5 字段/状态及有界任务表，或等价的文件 checksum lifecycle 字段；保证旧文件默认为不回算状态。
- 新增 `site_assets` 元数据表。
- 编写新库、升级库、重复 migration、默认值和约束测试。
- 验证：`go test ./internal/db -race`。

## Task 2：Repository 站点设置与 MD5 任务协议

- 实现读取/更新站点设置。
- 实现 asset 元数据替换、删除和公开查询。
- 实现上传完成时按当时开关创建 MD5 pending 任务。
- 实现有界 claim/complete/fail/requeue API；claim 必须跨连接唯一。
- 编写并发 claim、关闭/开启开关、旧文件不回算、删除幂等和恢复测试。
- 验证：`go test ./internal/db -race`。

## Task 3：MD5 worker 与受控资源存储

- 新增流式 MD5 计算 worker，限制并发和单轮任务数量。
- 服务启动时恢复 `computing`，执行一次有界 recovery；不启动无限重试。
- 删除文件与 checksum task 并发时保持无悬挂任务。
- 新增 site-assets 受控存储：opaque key、原子写入、权限、大小/MIME/图片像素校验，拒绝 SVG 和路径穿越。
- 编写正确性、大文件流式、失败恢复、资源校验测试。
- 验证：`go test ./internal/checksum ./internal/siteassets ./internal/publishedrecovery -race`（按实际包名调整）。

## Task 4：公开站点配置与管理员 API

- 增加 `GET /api/site-settings`，只返回公开字段和公开资源 URL。
- 增加管理员 `GET/PUT /api/admin/site-settings`。
- 增加管理员三类资源上传 API。
- 增加 `GET /assets/site/{assetID}` 与 `GET /favicon.ico`。
- 复用现有 session/admin 校验；所有写操作接入 application log 和持久化 audit；错误不得泄露内部路径。
- 编写认证、授权、字段校验、资源下载、缓存/Content-Type 和审计测试。
- 验证：`go test ./internal/httpapi -race`。

## Task 5：文件列表/详情 MD5 输出

- 在已有 read 权限过滤之后返回 checksum 状态/value；无 read 权限不可获得文件或 MD5。
- 不改变下载权限，不把 MD5 暴露给无权接口，不自动回算历史文件。
- 为上传完成和文件列表/详情补充 API 测试，覆盖 pending/computing/ready/failed/disabled。
- 验证：`go test ./internal/httpapi ./internal/db -race`。

## Task 6：前端站点壳、MD5 显示和管理员设置页

- 加载公开站点配置并应用网站名称、基准配色、登录 Logo、导航 Logo、favicon。
- 文件列表显示 MD5 状态/value；只使用服务端已授权返回值。
- 新增管理员设置页/API 调用：文本、颜色、备案开关/文本、MD5 开关、三类资源上传。
- 备案号关闭时不渲染；用户输入使用安全 DOM API。
- 添加前端测试：默认值、动态配置、备案隐藏、MD5 状态、设置保存/上传和错误处理。
- 验证：`npm test && npm run build`。

## Task 7：集成、文档与发布门禁

- 接入 production server 生命周期、启动 recovery、优雅关闭 worker。
- 更新 README、operations、systemd/Docker 部署说明和 API 文档；明确 MD5 不是安全签名，明确资源目录与持久化要求。
- 运行：
  - `gofmt -w`（仅 Go 改动文件）；
  - `GOTOOLCHAIN=local /usr/local/go/bin/go test ./... -race -count=1`；
  - `GOTOOLCHAIN=local /usr/local/go/bin/go vet ./...`；
  - `npm test`；
  - `npm run build`；
  - `git diff --check`。
- 人工复核敏感日志、路径安全、默认配置、旧数据库升级和工作区杂项后，创建双语 commit。推送需另行确认。

## 不在本次范围

- 不新增 `checksum_read` 权限。
- 不新增 MD5 命令行开关。
- 不对历史文件批量回算 MD5。
- 不支持外部 Logo/favicon URL。
- 不允许 SVG 上传。
- 不引入递归删除或无限后台重试。
