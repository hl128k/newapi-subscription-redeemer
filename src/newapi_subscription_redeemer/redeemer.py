#!/usr/bin/env python3
"""Standalone redemption service that turns codes into NewAPI subscriptions.

Why this exists:
- QuantumNous/new-api currently has two separate built-in modules:
  1) redemption codes that top up wallet quota
  2) subscription plans and admin subscription binding APIs
- It does not ship a built-in bridge that lets one-time redemption codes directly
  activate a subscription plan.

This script provides that bridge as an external service.
"""

from __future__ import annotations

import argparse
import json
import os
import secrets
import sqlite3
import sys
import time
import urllib.error
import urllib.request
from dataclasses import dataclass
from datetime import datetime, timezone
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any, Dict, Iterable, List, Optional
from urllib.parse import parse_qs, urlparse

DEFAULT_DB_PATH = str(Path(__file__).with_name("redeemer.db"))
STATIC_DIR = Path(__file__).with_name("web")
STATIC_ROUTES = {
    "/": ("index.html", "text/html; charset=utf-8"),
    "/index.html": ("index.html", "text/html; charset=utf-8"),
    "/static/styles.css": ("styles.css", "text/css; charset=utf-8"),
    "/static/app.js": ("app.js", "application/javascript; charset=utf-8"),
}
VALID_STATUSES = {"active", "pending", "used", "disabled"}
ALPHABET = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

SCHEMA_SQL = """
CREATE TABLE IF NOT EXISTS subscription_codes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    code TEXT NOT NULL UNIQUE,
    plan_id INTEGER NOT NULL,
    status TEXT NOT NULL DEFAULT 'active',
    note TEXT NOT NULL DEFAULT '',
    metadata_json TEXT NOT NULL DEFAULT '{}',
    created_at INTEGER NOT NULL,
    expires_at INTEGER,
    used_at INTEGER,
    used_by_user_id INTEGER,
    pending_at INTEGER,
    pending_user_id INTEGER,
    pending_token TEXT,
    newapi_message TEXT,
    newapi_response_json TEXT,
    last_error TEXT
);

CREATE INDEX IF NOT EXISTS idx_subscription_codes_status
    ON subscription_codes(status);
CREATE INDEX IF NOT EXISTS idx_subscription_codes_plan_id
    ON subscription_codes(plan_id);

CREATE TABLE IF NOT EXISTS audit_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    event_type TEXT NOT NULL,
    actor_type TEXT NOT NULL DEFAULT '',
    actor_id TEXT NOT NULL DEFAULT '',
    code TEXT,
    plan_id INTEGER,
    status TEXT NOT NULL DEFAULT '',
    message TEXT NOT NULL DEFAULT '',
    metadata_json TEXT NOT NULL DEFAULT '{}',
    created_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_audit_events_created_at
    ON audit_events(created_at);
CREATE INDEX IF NOT EXISTS idx_audit_events_event_type
    ON audit_events(event_type);
CREATE INDEX IF NOT EXISTS idx_audit_events_code
    ON audit_events(code);
"""


class ServiceError(Exception):
    def __init__(self, message: str, status: int = HTTPStatus.BAD_REQUEST) -> None:
        super().__init__(message)
        self.message = message
        self.status = int(status)


@dataclass
class Config:
    db_path: str
    newapi_base_url: str
    newapi_admin_access_token: str
    newapi_admin_user_id: int
    bind_mode: str = "bind"
    timeout_seconds: float = 20.0
    admin_secret: str = ""
    admin_prefix: str = "xx"
    host: str = "127.0.0.1"
    port: int = 8789

    @classmethod
    def from_env(cls) -> "Config":
        return cls(
            db_path=os.environ.get("REDEEMER_DB_PATH", DEFAULT_DB_PATH),
            newapi_base_url=os.environ.get("NEWAPI_BASE_URL", "").strip(),
            newapi_admin_access_token=os.environ.get("NEWAPI_ADMIN_ACCESS_TOKEN", "").strip(),
            newapi_admin_user_id=_int_env("NEWAPI_ADMIN_USER_ID", 0),
            bind_mode=os.environ.get("REDEEMER_BIND_MODE", "bind").strip() or "bind",
            timeout_seconds=_float_env("REDEEMER_TIMEOUT_SECONDS", 20.0),
            admin_secret=os.environ.get("REDEEMER_ADMIN_SECRET", "").strip(),
            admin_prefix=_normalize_admin_prefix(
                os.environ.get("REDEEMER_ADMIN_PREFIX", os.environ.get("REDEEMER_ADMIN_WEB_PREFIX", "xx"))
            ),
            host=os.environ.get("REDEEMER_HOST", "127.0.0.1").strip() or "127.0.0.1",
            port=_int_env("REDEEMER_PORT", 8789),
        )

    def admin_web_path(self) -> str:
        return f"/{self.admin_prefix}/admin"

    def admin_api_path(self, path: str) -> str:
        return f"/{self.admin_prefix}{path}"

    def validate_runtime(self, require_admin_secret: bool = False) -> None:
        if not self.db_path:
            raise ServiceError("REDEEMER_DB_PATH 未配置", HTTPStatus.INTERNAL_SERVER_ERROR)
        if require_admin_secret and not self.admin_secret:
            raise ServiceError("REDEEMER_ADMIN_SECRET 未配置", HTTPStatus.INTERNAL_SERVER_ERROR)
        if self.bind_mode not in {"bind", "create"}:
            raise ServiceError("REDEEMER_BIND_MODE 只能是 bind 或 create", HTTPStatus.INTERNAL_SERVER_ERROR)
        if not self.newapi_base_url:
            raise ServiceError("NEWAPI_BASE_URL 未配置", HTTPStatus.INTERNAL_SERVER_ERROR)
        if not self.newapi_admin_access_token:
            raise ServiceError("NEWAPI_ADMIN_ACCESS_TOKEN 未配置", HTTPStatus.INTERNAL_SERVER_ERROR)
        if self.newapi_admin_user_id <= 0:
            raise ServiceError("NEWAPI_ADMIN_USER_ID 必须大于 0", HTTPStatus.INTERNAL_SERVER_ERROR)


@dataclass
class NewAPIResult:
    message: str
    raw: Dict[str, Any]


class RedemptionService:
    def __init__(self, config: Config) -> None:
        self.config = config

    def init_db(self) -> None:
        with self._connect() as conn:
            conn.executescript(SCHEMA_SQL)

    def create_codes(
        self,
        *,
        plan_id: int,
        count: int,
        prefix: str,
        note: str,
        expires_at: Optional[int],
        metadata: Optional[Dict[str, Any]] = None,
        audit_actor_type: str = "cli",
        audit_actor_id: str = "",
        audit_metadata: Optional[Dict[str, Any]] = None,
    ) -> List[Dict[str, Any]]:
        if plan_id <= 0:
            raise ServiceError("plan_id 必须大于 0")
        if count <= 0 or count > 1000:
            raise ServiceError("count 必须在 1-1000 之间")
        if expires_at is not None and expires_at <= int(time.time()):
            raise ServiceError("expires_at 必须晚于当前时间")

        prefix = _normalize_prefix(prefix)
        metadata_json = json.dumps(metadata or {}, ensure_ascii=True, sort_keys=True)
        created_at = int(time.time())
        created: List[Dict[str, Any]] = []

        with self._connect() as conn:
            conn.execute("BEGIN IMMEDIATE")
            try:
                for _ in range(count):
                    code = self._generate_unique_code(conn, prefix)
                    conn.execute(
                        """
                        INSERT INTO subscription_codes (
                            code, plan_id, status, note, metadata_json, created_at, expires_at
                        ) VALUES (?, ?, 'active', ?, ?, ?, ?)
                        """,
                        (code, plan_id, note, metadata_json, created_at, expires_at),
                    )
                    created.append(
                        {
                            "code": code,
                            "plan_id": plan_id,
                            "status": "active",
                            "note": note,
                            "created_at": created_at,
                            "expires_at": expires_at,
                            "metadata": metadata or {},
                        }
                    )
                self._insert_audit_event(
                    conn,
                    event_type="codes.created",
                    actor_type=audit_actor_type,
                    actor_id=audit_actor_id,
                    code=None,
                    plan_id=plan_id,
                    status="active",
                    message=f"created {len(created)} codes",
                    metadata={
                        "count": len(created),
                        "prefix": prefix,
                        "note": note,
                        "expires_at": expires_at,
                        "codes": [item["code"] for item in created],
                        **(audit_metadata or {}),
                    },
                )
                conn.commit()
            except Exception:
                conn.rollback()
                raise
        return created

    def list_codes(self, *, status: Optional[str], plan_id: Optional[int], limit: int) -> List[Dict[str, Any]]:
        if status and status not in VALID_STATUSES:
            raise ServiceError(f"无效状态: {status}")
        if limit <= 0 or limit > 5000:
            raise ServiceError("limit 必须在 1-5000 之间")

        clauses: List[str] = []
        params: List[Any] = []
        if status:
            clauses.append("status = ?")
            params.append(status)
        if plan_id is not None:
            clauses.append("plan_id = ?")
            params.append(plan_id)

        where = ""
        if clauses:
            where = "WHERE " + " AND ".join(clauses)

        sql = f"""
            SELECT id, code, plan_id, status, note, metadata_json, created_at, expires_at,
                   used_at, used_by_user_id, pending_at, pending_user_id,
                   newapi_message, last_error
            FROM subscription_codes
            {where}
            ORDER BY id DESC
            LIMIT ?
        """
        params.append(limit)

        with self._connect() as conn:
            rows = conn.execute(sql, params).fetchall()
        return [self._row_to_dict(row) for row in rows]

    def list_audit_events(
        self,
        *,
        event_type: Optional[str],
        code: Optional[str],
        limit: int,
    ) -> List[Dict[str, Any]]:
        if limit <= 0 or limit > 5000:
            raise ServiceError("limit 必须在 1-5000 之间")

        clauses: List[str] = []
        params: List[Any] = []
        if event_type:
            clauses.append("event_type = ?")
            params.append(event_type)
        if code:
            clauses.append("code = ?")
            params.append(code.strip())

        where = ""
        if clauses:
            where = "WHERE " + " AND ".join(clauses)

        sql = f"""
            SELECT id, event_type, actor_type, actor_id, code, plan_id, status,
                   message, metadata_json, created_at
            FROM audit_events
            {where}
            ORDER BY id DESC
            LIMIT ?
        """
        params.append(limit)

        with self._connect() as conn:
            rows = conn.execute(sql, params).fetchall()
        return [self._audit_row_to_dict(row) for row in rows]

    def set_code_status(
        self,
        *,
        code: str,
        status: str,
        audit_actor_type: str = "cli",
        audit_actor_id: str = "",
        audit_metadata: Optional[Dict[str, Any]] = None,
    ) -> Dict[str, Any]:
        code = code.strip()
        if not code:
            raise ServiceError("code 不能为空")
        if status not in {"active", "disabled"}:
            raise ServiceError("只允许切换到 active 或 disabled")

        with self._connect() as conn:
            conn.execute("BEGIN IMMEDIATE")
            try:
                row = conn.execute(
                    "SELECT id, plan_id, status, used_at FROM subscription_codes WHERE code = ?",
                    (code,),
                ).fetchone()
                if row is None:
                    raise ServiceError("兑换码不存在", HTTPStatus.NOT_FOUND)
                if row["used_at"] is not None:
                    raise ServiceError("已使用的兑换码不能改状态", HTTPStatus.CONFLICT)

                conn.execute(
                    """
                    UPDATE subscription_codes
                    SET status = ?, pending_at = NULL, pending_user_id = NULL,
                        pending_token = NULL, last_error = NULL
                    WHERE id = ?
                    """,
                    (status, row["id"]),
                )
                self._insert_audit_event(
                    conn,
                    event_type="code.status_changed",
                    actor_type=audit_actor_type,
                    actor_id=audit_actor_id,
                    code=code,
                    plan_id=row["plan_id"],
                    status=status,
                    message=f"status changed from {row['status']} to {status}",
                    metadata={
                        "old_status": row["status"],
                        "new_status": status,
                        **(audit_metadata or {}),
                    },
                )
                conn.commit()
                updated = conn.execute(
                    "SELECT * FROM subscription_codes WHERE id = ?",
                    (row["id"],),
                ).fetchone()
            except Exception:
                conn.rollback()
                raise
        return self._row_to_dict(updated)

    def preview_redeem_code(self, *, code: str, user_id: int) -> Dict[str, Any]:
        code = code.strip()
        if not code:
            raise ServiceError("code 不能为空")
        if user_id <= 0:
            raise ServiceError("user_id 必须大于 0")

        now = int(time.time())
        with self._connect() as conn:
            row = conn.execute(
                "SELECT * FROM subscription_codes WHERE code = ?",
                (code,),
            ).fetchone()

        if row is None:
            raise ServiceError("兑换码不存在", HTTPStatus.NOT_FOUND)
        if row["status"] == "used":
            raise ServiceError("兑换码已被使用", HTTPStatus.CONFLICT)
        if row["status"] == "disabled":
            raise ServiceError("兑换码已停用", HTTPStatus.CONFLICT)
        if row["status"] == "pending":
            raise ServiceError("兑换码正在处理中，请稍后联系管理员", HTTPStatus.CONFLICT)
        if row["expires_at"] is not None and row["expires_at"] <= now:
            raise ServiceError("兑换码已过期", HTTPStatus.GONE)

        return {
            "code": row["code"],
            "user_id": user_id,
            "plan_id": row["plan_id"],
            "status": row["status"],
            "expires_at": row["expires_at"],
        }

    def redeem_code(
        self,
        *,
        code: str,
        user_id: int,
        audit_actor_type: str = "user",
        audit_actor_id: str = "",
        audit_metadata: Optional[Dict[str, Any]] = None,
    ) -> Dict[str, Any]:
        code = code.strip()
        if not code:
            raise ServiceError("code 不能为空")
        if user_id <= 0:
            raise ServiceError("user_id 必须大于 0")
        if not audit_actor_id:
            audit_actor_id = str(user_id)

        claimed = self._claim_code(code=code, user_id=user_id)
        try:
            newapi_result = self._activate_subscription(user_id=user_id, plan_id=claimed["plan_id"])
        except Exception as exc:
            self._release_claim(code_id=claimed["id"], pending_token=claimed["pending_token"], error_message=str(exc))
            self._record_audit_event(
                event_type="code.redeem_failed",
                actor_type=audit_actor_type,
                actor_id=audit_actor_id,
                code=code,
                plan_id=claimed["plan_id"],
                status="active",
                message=str(exc),
                metadata={
                    "user_id": user_id,
                    **(audit_metadata or {}),
                },
            )
            if isinstance(exc, ServiceError):
                raise
            raise ServiceError(f"调用 NewAPI 失败: {exc}", HTTPStatus.BAD_GATEWAY) from exc

        return self._finalize_claim(
            code_id=claimed["id"],
            pending_token=claimed["pending_token"],
            user_id=user_id,
            newapi_result=newapi_result,
            audit_actor_type=audit_actor_type,
            audit_actor_id=audit_actor_id,
            audit_metadata=audit_metadata,
        )

    def _claim_code(self, *, code: str, user_id: int) -> Dict[str, Any]:
        now = int(time.time())
        pending_token = secrets.token_hex(16)
        with self._connect() as conn:
            conn.execute("BEGIN IMMEDIATE")
            try:
                row = conn.execute(
                    "SELECT * FROM subscription_codes WHERE code = ?",
                    (code,),
                ).fetchone()
                if row is None:
                    raise ServiceError("兑换码不存在", HTTPStatus.NOT_FOUND)
                if row["status"] == "used":
                    raise ServiceError("兑换码已被使用", HTTPStatus.CONFLICT)
                if row["status"] == "disabled":
                    raise ServiceError("兑换码已停用", HTTPStatus.CONFLICT)
                if row["status"] == "pending":
                    raise ServiceError("兑换码正在处理中，请稍后联系管理员", HTTPStatus.CONFLICT)
                if row["expires_at"] is not None and row["expires_at"] <= now:
                    raise ServiceError("兑换码已过期", HTTPStatus.GONE)

                conn.execute(
                    """
                    UPDATE subscription_codes
                    SET status = 'pending', pending_at = ?, pending_user_id = ?,
                        pending_token = ?, last_error = NULL
                    WHERE id = ?
                    """,
                    (now, user_id, pending_token, row["id"]),
                )
                conn.commit()
            except Exception:
                conn.rollback()
                raise
        claimed = dict(row)
        claimed["pending_token"] = pending_token
        return claimed

    def _release_claim(self, *, code_id: int, pending_token: str, error_message: str) -> None:
        with self._connect() as conn:
            conn.execute("BEGIN IMMEDIATE")
            try:
                conn.execute(
                    """
                    UPDATE subscription_codes
                    SET status = 'active', pending_at = NULL, pending_user_id = NULL,
                        pending_token = NULL, last_error = ?
                    WHERE id = ? AND status = 'pending' AND pending_token = ?
                    """,
                    (error_message[:1000], code_id, pending_token),
                )
                conn.commit()
            except Exception:
                conn.rollback()
                raise

    def _finalize_claim(
        self,
        *,
        code_id: int,
        pending_token: str,
        user_id: int,
        newapi_result: NewAPIResult,
        audit_actor_type: str,
        audit_actor_id: str,
        audit_metadata: Optional[Dict[str, Any]],
    ) -> Dict[str, Any]:
        now = int(time.time())
        with self._connect() as conn:
            conn.execute("BEGIN IMMEDIATE")
            try:
                result = conn.execute(
                    """
                    UPDATE subscription_codes
                    SET status = 'used', used_at = ?, used_by_user_id = ?,
                        pending_at = NULL, pending_user_id = NULL, pending_token = NULL,
                        newapi_message = ?, newapi_response_json = ?, last_error = NULL
                    WHERE id = ? AND status = 'pending' AND pending_token = ?
                    """,
                    (
                        now,
                        user_id,
                        newapi_result.message,
                        json.dumps(newapi_result.raw, ensure_ascii=True, sort_keys=True),
                        code_id,
                        pending_token,
                    ),
                )
                if result.rowcount != 1:
                    raise ServiceError("兑换状态冲突，未能完成写回", HTTPStatus.CONFLICT)
                row = conn.execute("SELECT * FROM subscription_codes WHERE id = ?", (code_id,)).fetchone()
                self._insert_audit_event(
                    conn,
                    event_type="code.redeemed",
                    actor_type=audit_actor_type,
                    actor_id=audit_actor_id,
                    code=row["code"],
                    plan_id=row["plan_id"],
                    status="used",
                    message=newapi_result.message,
                    metadata={
                        "user_id": user_id,
                        **(audit_metadata or {}),
                    },
                )
                conn.commit()
            except Exception:
                conn.rollback()
                raise
        data = self._row_to_dict(row)
        data["newapi"] = newapi_result.raw
        return data

    def _activate_subscription(self, *, user_id: int, plan_id: int) -> NewAPIResult:
        self.config.validate_runtime(require_admin_secret=False)
        base = self.config.newapi_base_url.rstrip("/")
        if self.config.bind_mode == "create":
            path = f"/api/subscription/admin/users/{user_id}/subscriptions"
            payload = {"plan_id": plan_id}
        else:
            path = "/api/subscription/admin/bind"
            payload = {"user_id": user_id, "plan_id": plan_id}

        access_token = self.config.newapi_admin_access_token
        if not access_token.lower().startswith("bearer "):
            access_token = f"Bearer {access_token}"

        body = json.dumps(payload).encode("utf-8")
        request = urllib.request.Request(
            url=base + path,
            data=body,
            method="POST",
            headers={
                "Content-Type": "application/json",
                "Authorization": access_token,
                "New-Api-User": str(self.config.newapi_admin_user_id),
            },
        )

        try:
            with urllib.request.urlopen(request, timeout=self.config.timeout_seconds) as response:
                raw_body = response.read().decode("utf-8")
                data = json.loads(raw_body or "{}")
        except urllib.error.HTTPError as exc:
            body_text = exc.read().decode("utf-8", errors="replace")
            raise ServiceError(
                f"NewAPI 返回 HTTP {exc.code}: {body_text[:500]}",
                HTTPStatus.BAD_GATEWAY,
            ) from exc
        except urllib.error.URLError as exc:
            raise ServiceError(f"无法连接 NewAPI: {exc}", HTTPStatus.BAD_GATEWAY) from exc
        except json.JSONDecodeError as exc:
            raise ServiceError("NewAPI 返回了无法解析的 JSON", HTTPStatus.BAD_GATEWAY) from exc

        if not data.get("success"):
            message = str(data.get("message") or "NewAPI 激活失败")
            raise ServiceError(message, HTTPStatus.BAD_GATEWAY)

        data_field = data.get("data")
        message = str(data.get("message") or "").strip()
        if isinstance(data_field, dict) and data_field.get("message"):
            message = str(data_field.get("message"))
        if not message:
            message = "订阅已激活"

        return NewAPIResult(message=message, raw=data)

    def _connect(self) -> sqlite3.Connection:
        conn = sqlite3.connect(self.config.db_path, timeout=30, isolation_level=None)
        conn.row_factory = sqlite3.Row
        conn.execute("PRAGMA journal_mode=WAL")
        conn.execute("PRAGMA foreign_keys=ON")
        return conn

    def _generate_unique_code(self, conn: sqlite3.Connection, prefix: str) -> str:
        for _ in range(20):
            code = generate_code(prefix)
            exists = conn.execute(
                "SELECT 1 FROM subscription_codes WHERE code = ?",
                (code,),
            ).fetchone()
            if exists is None:
                return code
        raise ServiceError("生成兑换码失败，请重试", HTTPStatus.INTERNAL_SERVER_ERROR)

    def _record_audit_event(
        self,
        *,
        event_type: str,
        actor_type: str,
        actor_id: str,
        code: Optional[str],
        plan_id: Optional[int],
        status: str,
        message: str,
        metadata: Optional[Dict[str, Any]] = None,
    ) -> None:
        with self._connect() as conn:
            conn.execute("BEGIN IMMEDIATE")
            try:
                self._insert_audit_event(
                    conn,
                    event_type=event_type,
                    actor_type=actor_type,
                    actor_id=actor_id,
                    code=code,
                    plan_id=plan_id,
                    status=status,
                    message=message,
                    metadata=metadata,
                )
                conn.commit()
            except Exception:
                conn.rollback()
                raise

    def _insert_audit_event(
        self,
        conn: sqlite3.Connection,
        *,
        event_type: str,
        actor_type: str,
        actor_id: str,
        code: Optional[str],
        plan_id: Optional[int],
        status: str,
        message: str,
        metadata: Optional[Dict[str, Any]] = None,
    ) -> None:
        conn.execute(
            """
            INSERT INTO audit_events (
                event_type, actor_type, actor_id, code, plan_id, status,
                message, metadata_json, created_at
            ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
            """,
            (
                event_type,
                actor_type,
                actor_id,
                code,
                plan_id,
                status,
                message[:1000],
                json.dumps(metadata or {}, ensure_ascii=True, sort_keys=True),
                int(time.time()),
            ),
        )

    def _row_to_dict(self, row: sqlite3.Row) -> Dict[str, Any]:
        item = dict(row)
        metadata_text = item.pop("metadata_json", "{}") or "{}"
        try:
            item["metadata"] = json.loads(metadata_text)
        except json.JSONDecodeError:
            item["metadata"] = {"_raw": metadata_text}
        return item

    def _audit_row_to_dict(self, row: sqlite3.Row) -> Dict[str, Any]:
        item = dict(row)
        metadata_text = item.pop("metadata_json", "{}") or "{}"
        try:
            item["metadata"] = json.loads(metadata_text)
        except json.JSONDecodeError:
            item["metadata"] = {"_raw": metadata_text}
        return item


class RedeemerHandler(BaseHTTPRequestHandler):
    service: RedemptionService
    config: Config

    server_version = "NewAPISubscriptionRedeemer/0.1"

    def do_GET(self) -> None:
        parsed = urlparse(self.path)
        if parsed.path == "/healthz":
            self._json_response(HTTPStatus.OK, {"success": True, "message": "ok"})
            return
        if parsed.path == self.config.admin_api_path("/api/v1/admin/codes"):
            self._require_admin()
            query = parse_qs(parsed.query)
            status = _first(query.get("status"))
            plan_id = _maybe_int(_first(query.get("plan_id")))
            limit = _maybe_int(_first(query.get("limit"))) or 100
            items = self.service.list_codes(status=status, plan_id=plan_id, limit=limit)
            self._json_response(HTTPStatus.OK, {"success": True, "data": items})
            return
        if parsed.path == self.config.admin_api_path("/api/v1/admin/audit-events"):
            self._require_admin()
            query = parse_qs(parsed.query)
            event_type = _first(query.get("event_type"))
            code = _first(query.get("code"))
            limit = _maybe_int(_first(query.get("limit"))) or 100
            items = self.service.list_audit_events(event_type=event_type, code=code, limit=limit)
            self._json_response(HTTPStatus.OK, {"success": True, "data": items})
            return
        if self._serve_static(parsed.path):
            return
        self._json_response(HTTPStatus.NOT_FOUND, {"success": False, "message": "not found"})

    def do_POST(self) -> None:
        parsed = urlparse(self.path)
        if parsed.path == "/api/v1/redeem/preview":
            payload = self._read_json_body()
            code = str(payload.get("code") or "")
            user_id = _maybe_int(payload.get("user_id")) or 0
            result = self.service.preview_redeem_code(code=code, user_id=user_id)
            self._json_response(
                HTTPStatus.OK,
                {"success": True, "message": "兑换信息可用", "data": result},
            )
            return
        if parsed.path == "/api/v1/redeem":
            payload = self._read_json_body()
            code = str(payload.get("code") or "")
            user_id = _maybe_int(payload.get("user_id")) or 0
            result = self.service.redeem_code(
                code=code,
                user_id=user_id,
                audit_actor_type="user",
                audit_actor_id=str(user_id),
                audit_metadata=self._request_metadata(),
            )
            self._json_response(
                HTTPStatus.OK,
                {"success": True, "message": result.get("newapi_message") or "订阅已激活", "data": result},
            )
            return
        if parsed.path == self.config.admin_api_path("/api/v1/admin/codes"):
            self._require_admin()
            payload = self._read_json_body()
            created = self.service.create_codes(
                plan_id=_maybe_int(payload.get("plan_id")) or 0,
                count=_maybe_int(payload.get("count")) or 0,
                prefix=str(payload.get("prefix") or "SUB"),
                note=str(payload.get("note") or ""),
                expires_at=parse_datetime_to_epoch(payload.get("expires_at")),
                metadata=payload.get("metadata") if isinstance(payload.get("metadata"), dict) else {},
                audit_actor_type="admin",
                audit_actor_id=self._request_actor_id(),
                audit_metadata=self._request_metadata(),
            )
            self._json_response(HTTPStatus.CREATED, {"success": True, "data": created})
            return
        if parsed.path == self.config.admin_api_path("/api/v1/admin/codes/status"):
            self._require_admin()
            payload = self._read_json_body()
            updated = self.service.set_code_status(
                code=str(payload.get("code") or ""),
                status=str(payload.get("status") or ""),
                audit_actor_type="admin",
                audit_actor_id=self._request_actor_id(),
                audit_metadata=self._request_metadata(),
            )
            self._json_response(HTTPStatus.OK, {"success": True, "data": updated})
            return
        self._json_response(HTTPStatus.NOT_FOUND, {"success": False, "message": "not found"})

    def log_message(self, format: str, *args: Any) -> None:
        return

    def _require_admin(self) -> None:
        self.config.validate_runtime(require_admin_secret=True)
        provided = self.headers.get("X-Admin-Secret", "").strip()
        if not provided:
            auth = self.headers.get("Authorization", "").strip()
            if auth.lower().startswith("bearer "):
                provided = auth[7:].strip()
        if not provided or provided != self.config.admin_secret:
            raise ServiceError("admin auth failed", HTTPStatus.UNAUTHORIZED)

    def _request_actor_id(self) -> str:
        return self.client_address[0] if self.client_address else "unknown"

    def _request_metadata(self) -> Dict[str, Any]:
        return {
            "source": "http",
            "remote_addr": self._request_actor_id(),
            "path": urlparse(self.path).path,
        }

    def _read_json_body(self) -> Dict[str, Any]:
        length_text = self.headers.get("Content-Length", "0")
        try:
            length = int(length_text)
        except ValueError as exc:
            raise ServiceError("无效的 Content-Length") from exc
        raw = self.rfile.read(length) if length > 0 else b"{}"
        try:
            data = json.loads(raw.decode("utf-8") or "{}")
        except json.JSONDecodeError as exc:
            raise ServiceError("请求体不是合法 JSON") from exc
        if not isinstance(data, dict):
            raise ServiceError("请求体必须是 JSON object")
        return data

    def _json_response(self, status: int, payload: Dict[str, Any]) -> None:
        body = json.dumps(payload, ensure_ascii=True).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _serve_static(self, path: str) -> bool:
        route = STATIC_ROUTES.get(path)
        if route is None and self._is_admin_page_path(path):
            route = ("admin.html", "text/html; charset=utf-8")
        if route is None:
            return False

        filename, content_type = route
        try:
            body = (STATIC_DIR / filename).read_bytes()
        except OSError as exc:
            raise ServiceError("static file not found", HTTPStatus.NOT_FOUND) from exc

        self.send_response(HTTPStatus.OK)
        self.send_header("Content-Type", content_type)
        self.send_header("Cache-Control", "no-store")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
        return True

    def _is_admin_page_path(self, path: str) -> bool:
        admin_path = self.config.admin_web_path()
        return path in {admin_path, admin_path + "/", admin_path + ".html"}

    def handle_one_request(self) -> None:
        try:
            super().handle_one_request()
        except ServiceError as exc:
            self._json_response(exc.status, {"success": False, "message": exc.message})
        except BrokenPipeError:
            return
        except Exception as exc:
            self._json_response(
                HTTPStatus.INTERNAL_SERVER_ERROR,
                {"success": False, "message": f"internal error: {exc}"},
            )


def generate_code(prefix: str = "SUB") -> str:
    prefix = _normalize_prefix(prefix)
    chunks = ["".join(secrets.choice(ALPHABET) for _ in range(4)) for _ in range(3)]
    return prefix + "-" + "-".join(chunks)


def parse_datetime_to_epoch(value: Any) -> Optional[int]:
    if value in (None, "", 0):
        return None
    if isinstance(value, (int, float)):
        return int(value)
    if not isinstance(value, str):
        raise ServiceError("expires_at 必须是 ISO 时间字符串或 Unix 时间戳")
    text = value.strip()
    if not text:
        return None
    if text.isdigit():
        return int(text)
    if text.endswith("Z"):
        text = text[:-1] + "+00:00"
    try:
        dt = datetime.fromisoformat(text)
    except ValueError as exc:
        raise ServiceError("expires_at 不是合法的 ISO 时间") from exc
    if dt.tzinfo is None:
        raise ServiceError("expires_at 必须带时区偏移，例如 +08:00")
    return int(dt.timestamp())


def format_timestamp(ts: Optional[int]) -> Optional[str]:
    if ts is None:
        return None
    return datetime.fromtimestamp(ts, tz=timezone.utc).isoformat()


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Redeem codes into NewAPI subscriptions")
    sub = parser.add_subparsers(dest="command", required=True)

    sub.add_parser("init-db", help="Create or migrate the local SQLite database")

    serve = sub.add_parser("serve", help="Run the HTTP service")
    serve.add_argument("--host", default=None)
    serve.add_argument("--port", type=int, default=None)

    create = sub.add_parser("create-codes", help="Generate redemption codes")
    create.add_argument("--plan-id", type=int, required=True)
    create.add_argument("--count", type=int, default=1)
    create.add_argument("--prefix", default="SUB")
    create.add_argument("--note", default="")
    create.add_argument("--expires-at", default="")
    create.add_argument("--metadata", default="{}", help="JSON object")

    list_cmd = sub.add_parser("list-codes", help="List codes")
    list_cmd.add_argument("--status", default="")
    list_cmd.add_argument("--plan-id", type=int)
    list_cmd.add_argument("--limit", type=int, default=100)

    audit_cmd = sub.add_parser("list-audit", help="List audit events")
    audit_cmd.add_argument("--event-type", default="")
    audit_cmd.add_argument("--code", default="")
    audit_cmd.add_argument("--limit", type=int, default=100)

    redeem = sub.add_parser("redeem", help="Redeem a code locally")
    redeem.add_argument("--code", required=True)
    redeem.add_argument("--user-id", type=int, required=True)

    status_cmd = sub.add_parser("set-status", help="Set code status to active/disabled")
    status_cmd.add_argument("--code", required=True)
    status_cmd.add_argument("--status", required=True)

    return parser


def command_init_db(service: RedemptionService) -> int:
    service.init_db()
    print(json.dumps({"success": True, "db_path": service.config.db_path}, ensure_ascii=True))
    return 0


def command_create_codes(service: RedemptionService, args: argparse.Namespace) -> int:
    service.init_db()
    try:
        metadata = json.loads(args.metadata or "{}")
    except json.JSONDecodeError as exc:
        raise ServiceError("--metadata 必须是 JSON object") from exc
    if not isinstance(metadata, dict):
        raise ServiceError("--metadata 必须是 JSON object")

    created = service.create_codes(
        plan_id=args.plan_id,
        count=args.count,
        prefix=args.prefix,
        note=args.note,
        expires_at=parse_datetime_to_epoch(args.expires_at),
        metadata=metadata,
        audit_actor_type="cli",
        audit_actor_id="",
        audit_metadata={"source": "cli"},
    )
    print(json.dumps({"success": True, "data": created}, ensure_ascii=True, indent=2))
    return 0


def command_list_codes(service: RedemptionService, args: argparse.Namespace) -> int:
    service.init_db()
    status = args.status.strip() or None
    items = service.list_codes(status=status, plan_id=args.plan_id, limit=args.limit)
    for item in items:
        for key in ("created_at", "expires_at", "used_at", "pending_at"):
            item[key + "_iso"] = format_timestamp(item.get(key))
    print(json.dumps({"success": True, "data": items}, ensure_ascii=True, indent=2))
    return 0


def command_redeem(service: RedemptionService, args: argparse.Namespace) -> int:
    service.init_db()
    result = service.redeem_code(
        code=args.code,
        user_id=args.user_id,
        audit_actor_type="cli",
        audit_actor_id=str(args.user_id),
        audit_metadata={"source": "cli"},
    )
    for key in ("created_at", "expires_at", "used_at", "pending_at"):
        result[key + "_iso"] = format_timestamp(result.get(key))
    print(json.dumps({"success": True, "data": result}, ensure_ascii=True, indent=2))
    return 0


def command_set_status(service: RedemptionService, args: argparse.Namespace) -> int:
    service.init_db()
    updated = service.set_code_status(
        code=args.code,
        status=args.status,
        audit_actor_type="cli",
        audit_actor_id="",
        audit_metadata={"source": "cli"},
    )
    print(json.dumps({"success": True, "data": updated}, ensure_ascii=True, indent=2))
    return 0


def command_list_audit(service: RedemptionService, args: argparse.Namespace) -> int:
    service.init_db()
    event_type = args.event_type.strip() or None
    code = args.code.strip() or None
    items = service.list_audit_events(event_type=event_type, code=code, limit=args.limit)
    for item in items:
        item["created_at_iso"] = format_timestamp(item.get("created_at"))
    print(json.dumps({"success": True, "data": items}, ensure_ascii=True, indent=2))
    return 0


def command_serve(service: RedemptionService, args: argparse.Namespace) -> int:
    host = args.host or service.config.host
    port = args.port or service.config.port
    service.init_db()

    handler = type("BoundRedeemerHandler", (RedeemerHandler,), {})
    handler.service = service
    handler.config = service.config

    httpd = ThreadingHTTPServer((host, port), handler)
    print(json.dumps({"success": True, "listening": f"http://{host}:{port}", "db_path": service.config.db_path}, ensure_ascii=True))
    httpd.serve_forever()
    return 0


def _int_env(name: str, default: int) -> int:
    value = os.environ.get(name, "").strip()
    if not value:
        return default
    try:
        return int(value)
    except ValueError as exc:
        raise ServiceError(f"环境变量 {name} 不是整数") from exc


def _float_env(name: str, default: float) -> float:
    value = os.environ.get(name, "").strip()
    if not value:
        return default
    try:
        return float(value)
    except ValueError as exc:
        raise ServiceError(f"环境变量 {name} 不是数字") from exc


def _normalize_prefix(prefix: str) -> str:
    cleaned = "".join(ch for ch in prefix.upper() if ch.isalnum())
    return cleaned[:12] or "SUB"


def _normalize_admin_prefix(prefix: str) -> str:
    parts = [part for part in prefix.strip().strip("/").split("/") if part]
    return "/".join(parts) or "xx"


def _first(values: Optional[Iterable[str]]) -> Optional[str]:
    if not values:
        return None
    for value in values:
        return value
    return None


def _maybe_int(value: Any) -> Optional[int]:
    if value in (None, ""):
        return None
    try:
        return int(value)
    except (TypeError, ValueError) as exc:
        raise ServiceError("字段必须是整数") from exc


def main(argv: Optional[List[str]] = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    config = Config.from_env()
    service = RedemptionService(config)

    try:
        if args.command == "init-db":
            return command_init_db(service)
        if args.command == "serve":
            return command_serve(service, args)
        if args.command == "create-codes":
            return command_create_codes(service, args)
        if args.command == "list-codes":
            return command_list_codes(service, args)
        if args.command == "list-audit":
            return command_list_audit(service, args)
        if args.command == "redeem":
            return command_redeem(service, args)
        if args.command == "set-status":
            return command_set_status(service, args)
    except ServiceError as exc:
        print(json.dumps({"success": False, "message": exc.message}, ensure_ascii=True), file=sys.stderr)
        return 1

    parser.error("unknown command")
    return 2


if __name__ == "__main__":
    raise SystemExit(main())
