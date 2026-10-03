"""Fixture tests for the standalone OpenAPI checker."""
import socket
import unittest
from unittest.mock import patch

import check

HANDLER = 's.mux.HandleFunc("GET /api/v1/accounts/{accountID}/{resource}", h)'


def document():
    """document returns a minimal independent OpenAPI fixture."""
    return {"openapi": "3.1.0", "info": {"title": "fixture", "version": "1"}, "paths": {"/api/v1/accounts/{accountID}/feed": {"parameters": [{"name": "accountID", "in": "path", "required": True, "schema": {"type": "string"}}], "get": {"operationId": "accountFeed", "responses": {"200": {"description": "ok"}}}}}, "components": {"schemas": {"Nullable": {"type": "object", "required": ["id"], "properties": {"id": {"type": "string"}, "optional": {"type": ["string", "null"]}}, "examples": [{"id": "one"}]}}}}


class CheckerTest(unittest.TestCase):
    """CheckerTest covers valid and invalid source-independent fixtures."""
    def findings(self, value, handler=HANDLER):
        """findings runs the checker fixture."""
        return check.check(value, handler)

    def test_valid_nullable_feed_exception_and_offline(self):
        with patch.object(socket.socket, "connect", side_effect=AssertionError("network used")):
            self.assertEqual(self.findings(document()), [])

    def test_references_and_operation_ids_fail(self):
        value = document(); value["components"]["schemas"]["Broken"] = {"$ref": "https://example.invalid/x"}
        self.assertTrue(any("external reference" in item for item in self.findings(value)))
        value = document(); value["paths"]["/other"] = {"get": {"operationId": "accountFeed", "responses": {"200": {"description": "ok"}}}}
        self.assertTrue(any("duplicate operationId" in item for item in self.findings(value)))
        value = document(); value["paths"]["/api/v1/accounts/{accountID}/feed"]["get"] = {"$ref": "#/components/schemas/Nullable"}
        self.assertTrue(any("strict document" in item for item in self.findings(value)))

    def test_examples_formats_and_routes_fail(self):
        value = document(); value["components"]["schemas"]["UUID"] = {"type": "string", "format": "uuid", "examples": ["bad"]}; value["components"]["schemas"]["Time"] = {"type": "string", "format": "date-time", "examples": ["bad"]}
        self.assertEqual(sum("schema " in item for item in self.findings(value)), 2)
        value = document(); value["components"]["schemas"]["Nullable"]["examples"] = [{"id": 1}]
        self.assertTrue(any("schema Nullable example" in item for item in self.findings(value)))
        self.assertTrue(any("missing OpenAPI route: FOO" in item for item in self.findings(document(), 's.mux.HandleFunc("FOO /other", h)')))
        self.assertTrue(any("stale OpenAPI route" in item for item in self.findings(document(), 's.mux.HandleFunc("GET /other", h)')))


if __name__ == "__main__":
    unittest.main()
