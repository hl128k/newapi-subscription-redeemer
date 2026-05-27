#!/usr/bin/env python3
import json
import tempfile
import threading
import unittest
import urllib.error
import urllib.request
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

from redeemer import Config, RedeemerHandler, RedemptionService


class StubHandler(BaseHTTPRequestHandler):
    response_status = HTTPStatus.OK
    response_body = {"success": True, "message": "", "data": None}
    requests = []

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        raw = self.rfile.read(length)
        StubHandler.requests.append(
            {
                "path": self.path,
                "headers": dict(self.headers),
                "body": json.loads(raw.decode("utf-8") or "{}"),
            }
        )
        payload = json.dumps(self.response_body).encode("utf-8")
        self.send_response(self.response_status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, format, *args):
        return


class RedeemerTests(unittest.TestCase):
    def setUp(self):
        self.tmpdir = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmpdir.cleanup)
        self.db_path = str(Path(self.tmpdir.name) / "redeemer.db")

    def _start_stub_server(self):
        StubHandler.requests = []
        server = ThreadingHTTPServer(("127.0.0.1", 0), StubHandler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(self._stop_server, server)
        return server

    def _start_redeemer_server(self, service: RedemptionService):
        handler = type("TestRedeemerHandler", (RedeemerHandler,), {})
        handler.service = service
        handler.config = service.config
        server = ThreadingHTTPServer(("127.0.0.1", 0), handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(self._stop_server, server)
        return server

    def _stop_server(self, server: ThreadingHTTPServer) -> None:
        server.shutdown()
        server.server_close()

    def _service(self, base_url: str) -> RedemptionService:
        config = Config(
            db_path=self.db_path,
            newapi_base_url=base_url,
            newapi_admin_access_token="admin-token",
            newapi_admin_user_id=99,
            bind_mode="bind",
            admin_secret="test-secret",
        )
        service = RedemptionService(config)
        service.init_db()
        return service

    def test_redeem_code_success(self):
        server = self._start_stub_server()
        StubHandler.response_status = HTTPStatus.OK
        StubHandler.response_body = {
            "success": True,
            "message": "",
            "data": {"message": "用户分组将升级到 pro"},
        }
        service = self._service(f"http://127.0.0.1:{server.server_address[1]}")
        created = service.create_codes(plan_id=3, count=1, prefix="PRO", note="", expires_at=None)
        result = service.redeem_code(code=created[0]["code"], user_id=123)

        self.assertEqual(result["status"], "used")
        self.assertEqual(result["used_by_user_id"], 123)
        self.assertEqual(result["plan_id"], 3)
        self.assertEqual(result["newapi_message"], "用户分组将升级到 pro")

        self.assertEqual(len(StubHandler.requests), 1)
        req = StubHandler.requests[0]
        self.assertEqual(req["path"], "/api/subscription/admin/bind")
        self.assertEqual(req["body"], {"user_id": 123, "plan_id": 3})
        self.assertEqual(req["headers"].get("New-Api-User"), "99")
        self.assertEqual(req["headers"].get("Authorization"), "Bearer admin-token")

    def test_redeem_code_failure_releases_code(self):
        server = self._start_stub_server()
        StubHandler.response_status = HTTPStatus.OK
        StubHandler.response_body = {
            "success": False,
            "message": "套餐未启用",
            "data": None,
        }
        service = self._service(f"http://127.0.0.1:{server.server_address[1]}")
        created = service.create_codes(plan_id=9, count=1, prefix="VIP", note="", expires_at=None)

        with self.assertRaises(Exception):
            service.redeem_code(code=created[0]["code"], user_id=456)

        items = service.list_codes(status=None, plan_id=9, limit=10)
        self.assertEqual(len(items), 1)
        self.assertEqual(items[0]["status"], "active")
        self.assertIn("套餐未启用", items[0]["last_error"])
        events = service.list_audit_events(event_type="code.redeem_failed", code=created[0]["code"], limit=10)
        self.assertEqual(len(events), 1)
        self.assertEqual(events[0]["plan_id"], 9)
        self.assertIn("套餐未启用", events[0]["message"])

    def test_create_codes_writes_audit_event(self):
        service = self._service("http://127.0.0.1:1")
        created = service.create_codes(
            plan_id=4,
            count=2,
            prefix="LOG",
            note="audit test",
            expires_at=None,
            audit_actor_type="test",
            audit_actor_id="case",
            audit_metadata={"source": "unit"},
        )

        events = service.list_audit_events(event_type="codes.created", code=None, limit=10)
        self.assertEqual(len(events), 1)
        self.assertEqual(events[0]["actor_type"], "test")
        self.assertEqual(events[0]["actor_id"], "case")
        self.assertEqual(events[0]["plan_id"], 4)
        self.assertEqual(events[0]["metadata"]["count"], 2)
        self.assertEqual(events[0]["metadata"]["source"], "unit")
        self.assertEqual(events[0]["metadata"]["codes"], [item["code"] for item in created])

    def test_web_ui_static_files_are_served(self):
        service = self._service("http://127.0.0.1:1")
        server = self._start_redeemer_server(service)
        base_url = f"http://127.0.0.1:{server.server_address[1]}"

        with urllib.request.urlopen(base_url + "/") as response:
            html = response.read().decode("utf-8")
            self.assertEqual(response.status, HTTPStatus.OK)
            self.assertIn("text/html", response.headers.get("Content-Type", ""))
            self.assertIn("订阅兑换手帐", html)

        with self.assertRaises(urllib.error.HTTPError) as context:
            urllib.request.urlopen(base_url + "/admin")
        self.assertEqual(context.exception.code, HTTPStatus.NOT_FOUND)

        with urllib.request.urlopen(base_url + "/xx/admin") as response:
            html = response.read().decode("utf-8")
            self.assertEqual(response.status, HTTPStatus.OK)
            self.assertIn("text/html", response.headers.get("Content-Type", ""))
            self.assertIn("管理员便签板", html)

        with urllib.request.urlopen(base_url + "/static/app.js") as response:
            script = response.read().decode("utf-8")
            self.assertEqual(response.status, HTTPStatus.OK)
            self.assertIn("application/javascript", response.headers.get("Content-Type", ""))
            self.assertIn("refreshCodes", script)

    def test_preview_redeem_api_does_not_claim_code(self):
        service = self._service("http://127.0.0.1:1")
        created = service.create_codes(plan_id=5, count=1, prefix="CHK", note="", expires_at=None)
        server = self._start_redeemer_server(service)
        base_url = f"http://127.0.0.1:{server.server_address[1]}"
        body = json.dumps({"code": created[0]["code"], "user_id": 321}).encode("utf-8")
        request = urllib.request.Request(
            base_url + "/api/v1/redeem/preview",
            data=body,
            method="POST",
            headers={"Content-Type": "application/json"},
        )

        with urllib.request.urlopen(request) as response:
            payload = json.loads(response.read().decode("utf-8"))
            self.assertEqual(response.status, HTTPStatus.OK)
            self.assertTrue(payload["success"])
            self.assertEqual(payload["data"]["plan_id"], 5)
            self.assertEqual(payload["data"]["user_id"], 321)

        items = service.list_codes(status=None, plan_id=5, limit=10)
        self.assertEqual(items[0]["status"], "active")
        self.assertIsNone(items[0]["pending_user_id"])

    def test_admin_api_requires_configured_prefix(self):
        service = self._service("http://127.0.0.1:1")
        server = self._start_redeemer_server(service)
        base_url = f"http://127.0.0.1:{server.server_address[1]}"
        body = json.dumps({"plan_id": 8, "count": 1, "prefix": "ADM"}).encode("utf-8")
        headers = {
            "Content-Type": "application/json",
            "X-Admin-Secret": "test-secret",
        }

        old_request = urllib.request.Request(
            base_url + "/api/v1/admin/codes",
            data=body,
            method="POST",
            headers=headers,
        )
        with self.assertRaises(urllib.error.HTTPError) as context:
            urllib.request.urlopen(old_request)
        self.assertEqual(context.exception.code, HTTPStatus.NOT_FOUND)

        prefixed_request = urllib.request.Request(
            base_url + "/xx/api/v1/admin/codes",
            data=body,
            method="POST",
            headers=headers,
        )
        with urllib.request.urlopen(prefixed_request) as response:
            payload = json.loads(response.read().decode("utf-8"))
            self.assertEqual(response.status, HTTPStatus.CREATED)
            self.assertTrue(payload["success"])
            self.assertEqual(payload["data"][0]["plan_id"], 8)

        audit_request = urllib.request.Request(
            base_url + "/xx/api/v1/admin/audit-events?event_type=codes.created&limit=5",
            method="GET",
            headers={"X-Admin-Secret": "test-secret"},
        )
        with urllib.request.urlopen(audit_request) as response:
            payload = json.loads(response.read().decode("utf-8"))
            self.assertEqual(response.status, HTTPStatus.OK)
            self.assertTrue(payload["success"])
            self.assertEqual(payload["data"][0]["event_type"], "codes.created")
            self.assertEqual(payload["data"][0]["actor_type"], "admin")


if __name__ == "__main__":
    unittest.main()
