# NewAPI Subscription Redeemer

NewAPI Subscription Redeemer 是一个独立的兑换码桥接服务。它在本地维护一张 SQLite 兑换码表，让用户可以用一次性兑换码激活 `newapi` 里的订阅套餐。

服务不会修改 `newapi` 的官方兑换码语义；它通过管理员订阅接口完成绑定：

- `POST /api/subscription/admin/bind`
- `POST /api/subscription/admin/users/:id/subscriptions`

## 功能

- 本地 SQLite 持久化兑换码
- CLI 创建、查询、兑换和停用兑换码
- HTTP API 支持用户兑换和管理员发码
- 用户兑换支持“先核对、再确认激活”，核对时校验 NewAPI 用户邮箱
- 用户核对接口按来源 IP 限流，避免对 NewAPI 形成放大请求
- 用户 ID 和邮箱连续核对失败会写入 SQLite 并临时锁定，降低撞库风险
- 用户网页和管理员网页分离
- 管理员网页和管理员 API 统一使用入口前缀
- 操作审计日志记录发码、状态变更、兑换成功和兑换失败
- `active -> pending -> used` 状态机，降低重复激活风险
- Go 单二进制运行，Web UI 直接嵌入二进制
- Docker 多阶段构建，运行镜像使用 `scratch`

## 项目结构

```text
.
├── main.go                  # Go 服务入口
├── internal/redeemer/       # Go 后端实现
├── web/                     # 嵌入到 Go 二进制的网页 UI
├── internal/redeemer/redeemer_test.go
├── .env.example             # 环境变量示例
├── Dockerfile
├── docker-compose.yml
├── go.mod
└── go.sum
```

## 快速开始

复制配置示例，并按实际环境填写：

```bash
cp .env.example .env.local
```

无参数启动时，程序会自动读取二进制同目录下的 `.env.local`；使用 `go run .` 开发运行时，也会读取当前工作目录下的 `.env.local`。配置优先级为：默认值 < `.env.local` < 系统环境变量 < 启动参数。

至少需要配置：

```bash
export NEWAPI_BASE_URL="https://your-newapi.example.com"
export NEWAPI_ADMIN_ACCESS_TOKEN="your_admin_access_token"
export NEWAPI_ADMIN_USER_ID="1"
export REDEEMER_ADMIN_SECRET="change-this"
export REDEEMER_ADMIN_PREFIX="xx"
export REDEEMER_DB_PATH="$(pwd)/redeemer.db"
```

初始化数据库：

```bash
go run . init-db
```

启动服务：

```bash
go run .
```

直接运行二进制或 `go run .` 会默认启动 HTTP 服务，并在数据库文件不存在时自动创建表结构。需要指定监听地址时，也可以显式使用：

```bash
go run . serve --host 127.0.0.1 --port 8789
```

`serve` 支持用启动参数覆盖环境配置：

```bash
go run . serve \
  --db-path ./redeemer.db \
  --host 127.0.0.1 \
  --port 8789 \
  --admin-secret change-this \
  --admin-prefix xx \
  --upstream-name NewAPI \
  --bind-mode bind \
  --timeout-seconds 20 \
  --preview-rate-limit 10 \
  --preview-rate-window-seconds 60 \
  --preview-mismatch-limit 5 \
  --preview-lock-seconds 900 \
  --newapi-base-url https://your-newapi.example.com \
  --newapi-admin-access-token your_admin_access_token \
  --newapi-admin-user-id 1
```

访问页面：

```text
用户页: http://127.0.0.1:8789/
管理员页: http://127.0.0.1:8789/xx/admin
```

健康检查：

```bash
curl http://127.0.0.1:8789/healthz
```

## 配置

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `NEWAPI_BASE_URL` | 空 | `newapi` 服务地址 |
| `NEWAPI_ADMIN_ACCESS_TOKEN` | 空 | `newapi` 管理员访问令牌 |
| `NEWAPI_ADMIN_USER_ID` | `0` | `newapi` 管理员用户 ID |
| `REDEEMER_DB_PATH` | `./redeemer.db` | SQLite 数据库路径 |
| `REDEEMER_ADMIN_SECRET` | 空 | 管理 API 和管理员页密钥 |
| `REDEEMER_ADMIN_PREFIX` | `xx` | 管理员页和管理 API 的统一前缀 |
| `REDEEMER_UPSTREAM_NAME` | `NewAPI` | 网页上显示的上游系统名称 |
| `REDEEMER_BIND_MODE` | `bind` | `bind` 或 `create` |
| `REDEEMER_HOST` | `127.0.0.1` | HTTP 监听地址 |
| `REDEEMER_PORT` | `8789` | HTTP 监听端口 |
| `REDEEMER_TIMEOUT_SECONDS` | `20` | 调用 `newapi` 的超时时间 |
| `REDEEMER_PREVIEW_RATE_LIMIT` | `10` | 单个来源在核对窗口内允许的请求数，`0` 表示关闭限流 |
| `REDEEMER_PREVIEW_RATE_WINDOW_SECONDS` | `60` | 用户核对限流窗口秒数 |
| `REDEEMER_PREVIEW_MISMATCH_LIMIT` | `5` | 同一用户 ID 或邮箱连续核对失败后触发锁定的次数，`0` 表示关闭 |
| `REDEEMER_PREVIEW_LOCK_SECONDS` | `900` | 用户 ID 或邮箱核对失败后的锁定秒数 |

`REDEEMER_ADMIN_WEB_PREFIX` 仍会被兼容读取，但新配置建议使用 `REDEEMER_ADMIN_PREFIX`。

## CLI

生成兑换码：

```bash
go run . create-codes \
  --plan-id 3 \
  --count 5 \
  --prefix PRO \
  --note "618 活动"
```

生成带过期时间的兑换码：

```bash
go run . create-codes \
  --plan-id 3 \
  --count 5 \
  --prefix PRO \
  --expires-at "2026-12-31T23:59:59+08:00"
```

查询兑换码：

```bash
go run . list-codes --status active --limit 50
```

查询审计日志：

```bash
go run . list-audit --limit 50
```

本地兑换：

```bash
go run . redeem --code PRO-ABCD-EFGH-JKLM --user-id 123 --email user@example.com
```

停用或恢复兑换码：

```bash
go run . set-status --code PRO-ABCD-EFGH-JKLM --status disabled
```

## Web UI

用户页用于提交兑换码、`newapi` 用户 ID 和用户邮箱。兑换流程分两步：

1. 核对兑换信息
2. 确认激活订阅

管理员页用于创建兑换码、查询兑换码、停用和恢复未使用的兑换码。管理员页地址由 `REDEEMER_ADMIN_PREFIX` 决定：

```text
/{REDEEMER_ADMIN_PREFIX}/admin
```

默认地址：

```text
/xx/admin
```

管理员页还包含审计日志面板，可按事件类型、兑换码和条数查询最近操作。

## HTTP API

响应统一使用 JSON：

```json
{
  "success": true,
  "message": "ok",
  "data": {}
}
```

错误响应：

```json
{
  "success": false,
  "message": "错误信息"
}
```

### 用户核对

核对接口会读取本地兑换码状态，并调用 `newapi` 查询用户信息。只有用户 ID 和邮箱匹配时才会返回核对结果；该接口不会锁定兑换码，也不会激活订阅。

```bash
curl -X POST http://127.0.0.1:8789/api/v1/redeem/preview \
  -H 'Content-Type: application/json' \
  -d '{
    "code": "PRO-ABCD-EFGH-JKLM",
    "user_id": 123,
    "email": "user@example.com"
  }'
```

### 用户兑换

```bash
curl -X POST http://127.0.0.1:8789/api/v1/redeem \
  -H 'Content-Type: application/json' \
  -d '{
    "code": "PRO-ABCD-EFGH-JKLM",
    "user_id": 123,
    "email": "user@example.com"
  }'
```

成功响应示例：

```json
{
  "success": true,
  "message": "订阅已激活",
  "data": {
    "code": "PRO-ABCD-EFGH-JKLM",
    "plan_id": 3,
    "status": "used",
    "used_by_user_id": 123,
    "newapi_message": "用户分组将升级到 pro"
  }
}
```

### 管理鉴权

管理 API 需要提供 `REDEEMER_ADMIN_SECRET`。支持两种传递方式：

```http
Authorization: Bearer change-this
```

或：

```http
X-Admin-Secret: change-this
```

管理 API 的基础路径由 `REDEEMER_ADMIN_PREFIX` 决定：

```text
/{REDEEMER_ADMIN_PREFIX}/api/v1/admin
```

默认基础路径：

```text
/xx/api/v1/admin
```

### 创建兑换码

```bash
curl -X POST http://127.0.0.1:8789/xx/api/v1/admin/codes \
  -H 'Authorization: Bearer change-this' \
  -H 'Content-Type: application/json' \
  -d '{
    "plan_id": 3,
    "count": 2,
    "prefix": "PRO",
    "note": "活动赠送"
  }'
```

### 查询兑换码

```bash
curl 'http://127.0.0.1:8789/xx/api/v1/admin/codes?status=active&limit=50' \
  -H 'Authorization: Bearer change-this'
```

### 停用或恢复兑换码

```bash
curl -X POST http://127.0.0.1:8789/xx/api/v1/admin/codes/status \
  -H 'Authorization: Bearer change-this' \
  -H 'Content-Type: application/json' \
  -d '{
    "code": "PRO-ABCD-EFGH-JKLM",
    "status": "disabled"
  }'
```

### 批量删除、停用或恢复兑换码

```bash
curl -X POST http://127.0.0.1:8789/xx/api/v1/admin/codes/batch \
  -H 'Authorization: Bearer change-this' \
  -H 'Content-Type: application/json' \
  -d '{
    "action": "disable",
    "codes": [
      "PRO-ABCD-EFGH-JKLM",
      "PRO-2345-6789-WXYZ"
    ]
  }'
```

`action` 可选 `disable`、`restore`、`delete`。批量删除只允许删除未使用且不在处理中的兑换码。

### 查询审计日志

```bash
curl 'http://127.0.0.1:8789/xx/api/v1/admin/audit-events?limit=50' \
  -H 'Authorization: Bearer change-this'
```

可选查询参数：

| 参数 | 说明 |
| --- | --- |
| `event_type` | 事件类型，例如 `codes.created` |
| `code` | 兑换码 |
| `limit` | 返回条数，范围 `1-5000` |

当前记录的事件类型：

| 事件类型 | 说明 |
| --- | --- |
| `codes.created` | 管理员或 CLI 创建兑换码 |
| `code.status_changed` | 管理员或 CLI 停用/恢复兑换码 |
| `codes.status_changed` | 管理员批量停用/恢复兑换码 |
| `codes.deleted` | 管理员批量删除兑换码 |
| `code.redeemed` | 兑换码激活成功 |
| `code.redeem_failed` | 兑换码激活失败并释放回 `active` |

## NewAPI 绑定模式

默认使用：

```bash
export REDEEMER_BIND_MODE="bind"
```

服务会调用：

```text
POST /api/subscription/admin/bind
```

请求体：

```json
{
  "user_id": 123,
  "plan_id": 3
}
```

设置为 `create` 时：

```bash
export REDEEMER_BIND_MODE="create"
```

服务会调用：

```text
POST /api/subscription/admin/users/123/subscriptions
```

请求体：

```json
{
  "plan_id": 3
}
```

## 状态机

兑换码状态：

| 状态 | 说明 |
| --- | --- |
| `active` | 可兑换 |
| `pending` | 已锁定，正在调用 `newapi` |
| `used` | 已成功激活订阅 |
| `disabled` | 管理员停用 |

兑换时会先把兑换码从 `active` 锁定为 `pending`，再调用 `newapi`。调用成功后写回 `used`；调用失败则恢复为 `active` 并记录错误。

如果 NewAPI 套餐返回 `max_purchase_per_user`，兑换服务会在本地按“同一 NewAPI 用户 + 同一套餐”统计 `used` 和 `pending` 兑换码。数量达到套餐限购值时会提前拒绝兑换；`0` 或缺省表示不限制。

## Docker

Dockerfile 使用 Go 多阶段构建，最终运行层是 `scratch`，只包含 CA 证书、`/data` 目录和编译后的 `redeemer` 二进制。

构建镜像：

```bash
docker build -t newapi-subscription-redeemer:local .
```

运行容器：

```bash
docker run --rm \
  -p 127.0.0.1:8789:8789 \
  -v newapi-redeemer-data:/data \
  -e REDEEMER_ADMIN_SECRET="change-this" \
  -e REDEEMER_ADMIN_PREFIX="xx" \
  -e NEWAPI_BASE_URL="https://your-newapi.example.com" \
  -e NEWAPI_ADMIN_ACCESS_TOKEN="your_admin_access_token" \
  -e NEWAPI_ADMIN_USER_ID="1" \
  newapi-subscription-redeemer:local
```

容器内默认使用 `/data/redeemer.db` 保存 SQLite 数据。

## Docker Compose

Compose 会把 `.env.local` 只读挂载到容器内的 `/.env.local`，由程序启动时自行读取。容器内会固定覆盖 `REDEEMER_HOST=0.0.0.0` 和 `REDEEMER_DB_PATH=/data/redeemer.db`，避免本地监听地址和本地数据库路径影响容器运行。

```bash
docker compose up -d --build
```

Compose 默认只绑定本机 `127.0.0.1:8789`，数据保存在 `redeemer-data` 卷中。需要改宿主机端口时，使用系统环境变量：

```bash
REDEEMER_PORT=8790 docker compose up -d --build
```

## GitHub Actions 镜像构建

仓库包含 GitHub Actions 工作流：

```text
.github/workflows/docker-image.yml
```

触发规则：

- push 到 `main` 或 `master`：构建并推送 `latest`、分支 tag 和 `sha-*` tag
- push `v*.*.*` tag：构建并推送对应版本 tag
- pull request：只构建，不推送
- `workflow_dispatch`：支持手动触发

镜像会推送到 GHCR：

```text
ghcr.io/<owner>/<repo>
```

工作流默认构建：

```text
linux/amd64
linux/arm64
```

首次使用时，需要在 GitHub 仓库的 Packages 设置里确认镜像可见性；私有仓库默认会生成私有镜像。

## 开发

运行测试：

```bash
go test ./...
```

本地构建 Go 二进制：

```bash
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o redeemer .
```

检查 CLI 入口：

```bash
go run . --help
```

检查 Docker Compose 配置：

```bash
docker compose config
```

## 部署建议

- 默认只监听 `127.0.0.1`
- 对外访问建议通过 Nginx 或 Caddy 反向代理
- `REDEEMER_ADMIN_SECRET` 使用长随机字符串
- `REDEEMER_ADMIN_PREFIX` 不应使用默认值
- SQLite 数据库路径放到持久化目录
- 生产环境保留兑换码生成记录，便于审计
