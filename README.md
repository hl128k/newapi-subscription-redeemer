# NewAPI Subscription Redeemer

NewAPI Subscription Redeemer 是一个独立的兑换码桥接服务。它在本地维护一张 SQLite 兑换码表，让用户可以用一次性兑换码激活 `newapi` 里的订阅套餐。

服务不会修改 `newapi` 的官方兑换码语义；它通过管理员订阅接口完成绑定：

- `POST /api/subscription/admin/bind`
- `POST /api/subscription/admin/users/:id/subscriptions`

## 功能

- 本地 SQLite 持久化兑换码
- CLI 创建、查询、兑换和停用兑换码
- HTTP API 支持用户兑换和管理员发码
- 用户兑换支持“先核对、再确认激活”
- 用户网页和管理员网页分离
- 管理员网页和管理员 API 统一使用入口前缀
- 操作审计日志记录发码、状态变更、兑换成功和兑换失败
- `active -> pending -> used` 状态机，降低重复激活风险
- 零额外 Python 运行时依赖
- 支持 Docker 和 Docker Compose 部署

## 项目结构

```text
.
├── src/newapi_subscription_redeemer/
│   ├── redeemer.py          # 主服务实现
│   └── web/                 # 内置网页 UI
├── redeemer.py              # 兼容旧命令的启动入口
├── test_redeemer.py         # 单元测试
├── config.example.env       # 环境变量示例
├── Dockerfile
├── docker-compose.yml
└── pyproject.toml
```

## 快速开始

复制配置示例，并按实际环境填写：

```bash
cp config.example.env .env.example.local
```

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
python3 redeemer.py init-db
```

启动服务：

```bash
python3 redeemer.py serve --host 127.0.0.1 --port 8789
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
| `REDEEMER_BIND_MODE` | `bind` | `bind` 或 `create` |
| `REDEEMER_HOST` | `127.0.0.1` | HTTP 监听地址 |
| `REDEEMER_PORT` | `8789` | HTTP 监听端口 |
| `REDEEMER_TIMEOUT_SECONDS` | `20` | 调用 `newapi` 的超时时间 |

`REDEEMER_ADMIN_WEB_PREFIX` 仍会被兼容读取，但新配置建议使用 `REDEEMER_ADMIN_PREFIX`。

## CLI

生成兑换码：

```bash
python3 redeemer.py create-codes \
  --plan-id 3 \
  --count 5 \
  --prefix PRO \
  --note "618 活动"
```

生成带过期时间的兑换码：

```bash
python3 redeemer.py create-codes \
  --plan-id 3 \
  --count 5 \
  --prefix PRO \
  --expires-at "2026-12-31T23:59:59+08:00"
```

查询兑换码：

```bash
python3 redeemer.py list-codes --status active --limit 50
```

查询审计日志：

```bash
python3 redeemer.py list-audit --limit 50
```

本地兑换：

```bash
python3 redeemer.py redeem --code PRO-ABCD-EFGH-JKLM --user-id 123
```

停用或恢复兑换码：

```bash
python3 redeemer.py set-status --code PRO-ABCD-EFGH-JKLM --status disabled
```

## Web UI

用户页用于提交兑换码和 `newapi` 用户 ID。兑换流程分两步：

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

核对接口只读取本地兑换码状态，不会锁定兑换码，也不会调用 `newapi`。

```bash
curl -X POST http://127.0.0.1:8789/api/v1/redeem/preview \
  -H 'Content-Type: application/json' \
  -d '{
    "code": "PRO-ABCD-EFGH-JKLM",
    "user_id": 123
  }'
```

### 用户兑换

```bash
curl -X POST http://127.0.0.1:8789/api/v1/redeem \
  -H 'Content-Type: application/json' \
  -d '{
    "code": "PRO-ABCD-EFGH-JKLM",
    "user_id": 123
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

## Docker

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

```bash
REDEEMER_ADMIN_SECRET="change-this" \
NEWAPI_BASE_URL="https://your-newapi.example.com" \
NEWAPI_ADMIN_ACCESS_TOKEN="your_admin_access_token" \
NEWAPI_ADMIN_USER_ID="1" \
docker compose up -d --build
```

Compose 默认只绑定本机 `127.0.0.1:8789`，数据保存在 `redeemer-data` 卷中。

## 开发

运行测试：

```bash
python -m unittest -v
```

检查 CLI 入口：

```bash
python redeemer.py --help
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
