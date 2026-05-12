"""Tests for the GET /viz/schema.json endpoint."""

from __future__ import annotations

import asyncio
import json
import tempfile
import unittest
from pathlib import Path

from fastapi.testclient import TestClient

FIXTURE = Path(__file__).parent / "fixtures" / "BAK-1321" / "BAK-1321-direct-flatten.viz.json"


class TestVizSchemaRoute(unittest.TestCase):
    def setUp(self) -> None:
        from zing_ai.server.app import create_app
        from zing_ai.server.sessions import SessionManager

        self._tmp = tempfile.TemporaryDirectory()
        self.manager = SessionManager(data_dir=Path(self._tmp.name))
        self.cc_queues: list[asyncio.Queue] = []
        asgi_app = create_app(
            session_manager=self.manager,
            cc_queues=self.cc_queues,
            disable_polling=True,
        )
        self.client = TestClient(asgi_app, raise_server_exceptions=True)

    def tearDown(self) -> None:
        self.client.close()
        self._tmp.cleanup()

    def test_schema_endpoint_returns_200_with_dict_body(self) -> None:
        resp = self.client.get("/viz/schema.json")
        self.assertEqual(resp.status_code, 200)
        body = resp.json()
        self.assertIsInstance(body, dict)
        self.assertEqual(body["$id"], "https://zing.example/schemas/graph.json")
        self.assertIn("$defs", body)

    def test_returned_schema_validates_BAK1321_fixture(self) -> None:
        import jsonschema

        resp = self.client.get("/viz/schema.json")
        schema = resp.json()
        fixture = json.loads(FIXTURE.read_text())
        # Should validate without raising
        jsonschema.Draft202012Validator(schema).validate(fixture)


if __name__ == "__main__":
    unittest.main()
