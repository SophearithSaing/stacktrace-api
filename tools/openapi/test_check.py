"""Fixture tests for the standalone OpenAPI checker."""

import io
import socket
import tempfile
import unittest
import urllib.request
from contextlib import redirect_stdout
from pathlib import Path
from unittest.mock import patch

import yaml

import check


FEED_PATH = "/api/v1/accounts/{accountID}/feed"
FEED_HANDLER = 's.mux.HandleFunc("GET /api/v1/accounts/{accountID}/{resource}", h)'


def operation(operation_id="accountFeed"):
    """operation returns a minimal valid OpenAPI operation."""
    return {"operationId": operation_id, "responses": {"200": {"description": "ok"}}}


def document():
    """document returns a minimal independent OpenAPI fixture."""
    return {
        "openapi": "3.1.0",
        "info": {"title": "fixture", "version": "1"},
        "paths": {
            FEED_PATH: {
                "parameters": [{"name": "accountID", "in": "path", "required": True, "schema": {"type": "string"}}],
                "get": operation(),
            }
        },
        "components": {
            "schemas": {
                "Nullable": {
                    "type": "object",
                    "required": ["id"],
                    "properties": {"id": {"type": "string"}, "optional": {"type": ["string", "null"]}},
                    "examples": [{"id": "one"}, {"id": "two", "optional": None}],
                }
            }
        },
    }


class CheckerTest(unittest.TestCase):
    """CheckerTest covers valid and invalid source-independent fixtures."""

    def findings(self, value, handler=FEED_HANDLER):
        """findings runs the checker fixture."""
        return check.check(value, handler)

    def test_valid_nullable_and_omitted_values_pass_offline(self):
        """test_valid_nullable_and_omitted_values_pass_offline blocks network access."""
        with patch.object(socket.socket, "connect", side_effect=AssertionError("network used")) as connect, patch.object(
            urllib.request, "urlopen", side_effect=AssertionError("network used")
        ) as urlopen:
            self.assertEqual(self.findings(document()), [])
        connect.assert_not_called()
        urlopen.assert_not_called()

    def test_external_reference_with_example_stops_before_validation(self):
        """test_external_reference_with_example_stops_before_validation preserves offline behavior."""
        value = document()
        value["components"]["schemas"]["External"] = {"$ref": "https://example.invalid/schema", "examples": ["bad"]}
        with patch.object(socket.socket, "connect", side_effect=AssertionError("network used")) as connect, patch.object(
            urllib.request, "urlopen", side_effect=AssertionError("network used")
        ) as urlopen:
            findings = self.findings(value)
        self.assertTrue(any("external reference" in finding for finding in findings))
        self.assertFalse(any("schema External" in finding for finding in findings))
        connect.assert_not_called()
        urlopen.assert_not_called()

    def test_dynamic_references_remain_local_and_offline(self):
        """test_dynamic_references_remain_local_and_offline rejects remote schema retrieval."""
        value = document()
        value["components"]["schemas"]["ExternalDynamic"] = {
            "$dynamicRef": "https://example.invalid/schema",
            "examples": ["bad"],
        }
        with patch.object(socket.socket, "connect", side_effect=AssertionError("network used")) as connect, patch.object(
            urllib.request, "urlopen", side_effect=AssertionError("network used")
        ) as urlopen:
            findings = self.findings(value)
        self.assertTrue(any("external reference" in finding for finding in findings))
        connect.assert_not_called()
        urlopen.assert_not_called()

        value = document()
        value["components"]["schemas"]["Dynamic"] = {
            "$dynamicRef": "#/components/schemas/Nullable",
            "examples": [{"id": "valid"}],
        }
        self.assertEqual(self.findings(value), [])

    def test_media_examples_handle_absent_and_boolean_schemas(self):
        """test_media_examples_handle_absent_and_boolean_schemas distinguishes no schema from false."""
        value = document()
        value["paths"][FEED_PATH]["get"]["responses"]["200"]["content"] = {
            "application/json": {"example": "anything"}
        }
        self.assertEqual(self.findings(value), [])

        value["paths"][FEED_PATH]["get"]["responses"]["200"]["content"] = {
            "application/json": {"schema": False, "example": "anything"}
        }
        self.assertTrue(any("response 200 application/json example" in finding for finding in self.findings(value)))

        for schema in (True, {}):
            value["paths"][FEED_PATH]["get"]["responses"]["200"]["content"] = {
                "application/json": {"schema": schema, "example": "anything"}
            }
            self.assertEqual(self.findings(value), [])

    def test_parameter_header_and_reusable_media_schema_examples_validate(self):
        """test_parameter_header_and_reusable_media_schema_examples_validate covers all schema contexts."""
        value = document()
        value["paths"][FEED_PATH]["parameters"][0]["schema"]["examples"] = [42]
        value["components"].update(
            {
                "parameters": {
                    "bad": {
                        "name": "query",
                        "in": "query",
                        "schema": {"type": "string", "examples": [42]},
                    }
                },
                "headers": {"bad": {"schema": {"type": "string", "examples": [42]}}},
                "requestBodies": {
                    "bad": {
                        "content": {
                            "application/json": {
                                "schema": {"type": "string"},
                                "example": 42,
                            }
                        }
                    }
                },
                "responses": {
                    "bad": {
                        "description": "ok",
                        "headers": {"X-Bad": {"$ref": "#/components/headers/bad"}},
                        "content": {
                            "application/json": {
                                "schema": {"type": "string"},
                                "example": 42,
                            }
                        },
                    }
                },
            }
        )
        findings = self.findings(value)
        self.assertTrue(any("parameter accountID" in finding for finding in findings))
        self.assertTrue(any("component parameter bad" in finding for finding in findings))
        self.assertTrue(any("component header bad" in finding for finding in findings))
        self.assertTrue(any("component request body bad" in finding for finding in findings))
        self.assertTrue(any("component response bad" in finding for finding in findings))

    def test_invalid_operation_and_broken_references_fail_cleanly(self):
        """test_invalid_operation_and_broken_references_fail_cleanly covers invalid pointer forms."""
        value = document()
        value["paths"][FEED_PATH]["get"] = {"$ref": "#/components/schemas/Nullable"}
        self.assertTrue(any("strict document" in finding for finding in self.findings(value)))

        value = document()
        value["components"]["schemas"]["Broken"] = {"$ref": "#/components/missing"}
        self.assertTrue(any("unresolved reference" in finding for finding in self.findings(value)))

        value = document()
        value["components"]["responses"] = {"scalar": "not an object"}
        value["paths"][FEED_PATH]["get"]["responses"]["200"] = {"$ref": "#/components/responses/scalar"}
        self.assertTrue(any("strict document" in finding for finding in self.findings(value)))

    def test_path_item_and_reference_chains_validate_media_examples(self):
        """test_path_item_and_reference_chains_validate_media_examples resolves OpenAPI objects."""
        value = document()
        value["components"].update(
            {
                "examples": {"bad": {"value": {"id": 7}}},
                "requestBodies": {"body": {"$ref": "#/components/requestBodies/bodyAgain"}, "bodyAgain": {"content": {"application/json": {"schema": {"$ref": "#/x-schemas/nullable"}, "examples": {"bad": {"$ref": "#/components/examples/bad"}}}}}},
                "responses": {"response": {"$ref": "#/components/responses/responseAgain"}, "responseAgain": {"description": "ok", "content": {"application/json": {"schema": {"$ref": "#/x-schemas/nullable"}, "examples": {"bad": {"$ref": "#/components/examples/bad"}}}}}},
                "pathItems": {"feed": {"parameters": [{"name": "accountID", "in": "path", "required": True, "schema": {"type": "string"}}], "get": {"operationId": "accountFeed", "requestBody": {"$ref": "#/components/requestBodies/body"}, "responses": {"200": {"$ref": "#/components/responses/response"}}}}},
            }
        )
        value["x-schemas"] = {"nullable": {"$ref": "#/components/schemas/Nullable"}}
        value["paths"][FEED_PATH] = {"$ref": "#/components/pathItems/feed"}
        findings = self.findings(value)
        self.assertEqual(sum("example bad" in finding for finding in findings), 2)

    def test_reference_cycles_report_diagnostics_without_recursion(self):
        """test_reference_cycles_report_diagnostics_without_recursion handles cyclic objects."""
        value = document()
        value["components"]["responses"] = {"one": {"$ref": "#/components/responses/two"}, "two": {"$ref": "#/components/responses/one"}}
        value["paths"][FEED_PATH]["get"]["responses"]["200"] = {"$ref": "#/components/responses/one"}
        self.assertTrue(any("cyclic reference" in finding for finding in self.findings(value)))

    def test_nested_and_boolean_schema_examples_validate(self):
        """test_nested_and_boolean_schema_examples_validate validates every inline schema."""
        value = document()
        value["components"]["schemas"]["Nested"] = {"type": "object", "properties": {"uuid": {"type": "string", "format": "uuid", "examples": ["not-a-uuid"]}, "allowed": {"examples": ["anything"]}}}
        value["paths"][FEED_PATH]["get"]["responses"]["200"]["content"] = {"application/json": {"schema": True, "example": "anything"}}
        findings = self.findings(value)
        self.assertTrue(any("schema Nested.properties.uuid" in finding for finding in findings))
        self.assertFalse(any("allowed" in finding for finding in findings))

    def test_uuid_datetime_and_media_examples_fail(self):
        """test_uuid_datetime_and_media_examples_fail applies format validation."""
        value = document()
        value["components"]["schemas"]["Time"] = {"type": "string", "format": "date-time", "examples": ["not-a-time"]}
        value["paths"][FEED_PATH]["get"]["responses"]["200"]["content"] = {"application/json": {"schema": {"type": "string", "format": "uuid"}, "example": "not-a-uuid"}}
        findings = self.findings(value)
        self.assertTrue(any("schema Time" in finding for finding in findings))
        self.assertTrue(any("response 200 application/json example" in finding for finding in findings))

    def test_missing_and_duplicate_operation_ids_fail(self):
        """test_missing_and_duplicate_operation_ids_fail requires unique operation identifiers."""
        value = document()
        value["paths"]["/other"] = {"get": operation("accountFeed")}
        self.assertTrue(any("not unique" in finding for finding in self.findings(value)))
        value["paths"]["/other"]["get"] = operation("")
        self.assertTrue(any("missing nonempty operationId" in finding for finding in self.findings(value)))

    def test_route_drift_supports_all_source_method_tokens(self):
        """test_route_drift_supports_all_source_method_tokens preserves source method tokens."""
        source = "\n".join(
            [
                's.mux.HandleFunc("HEAD /head", h)',
                's.mux.HandleFunc("OPTIONS /options", h)',
                's.mux.HandleFunc("PATCH /patch", h)',
                's.mux.HandleFunc("M-SEARCH /search", h)',
                's.mux.HandleFunc("mixedCase /mixed", h)',
                's.mux.HandleFunc("TAB\t/tab", h)',
                's.mux.HandleFunc("/fallback", h)',
                'other.HandleFunc("GET /ignored", h)',
            ]
        )
        routes, _ = check.source_routes(source)
        self.assertEqual(routes, {("HEAD", "/head"), ("OPTIONS", "/options"), ("PATCH", "/patch"), ("M-SEARCH", "/search"), ("mixedCase", "/mixed"), ("TAB", "/tab")})
        findings = self.findings(document(), source)
        self.assertTrue(any("missing OpenAPI route: M-SEARCH /search" in finding for finding in findings))
        self.assertFalse(any("ignored" in finding for finding in findings))

    def test_strict_diagnostics_include_the_document_path(self):
        """test_strict_diagnostics_include_the_document_path makes structural failures actionable."""
        value = document()
        value["openapi"] = 3
        findings = self.findings(value)
        self.assertTrue(any("at $.openapi" in finding for finding in findings))

    def test_feed_exception_is_exactly_get_only(self):
        """test_feed_exception_is_exactly_get_only keeps the account resource exception narrow."""
        routes, _ = check.source_routes('s.mux.HandleFunc("POST /api/v1/accounts/{accountID}/{resource}", h)')
        self.assertEqual(routes, {("POST", "/api/v1/accounts/{accountID}/{resource}")})

    def test_main_reports_malformed_input_without_traceback(self):
        """test_main_reports_malformed_input_without_traceback handles invalid local files."""
        with tempfile.TemporaryDirectory() as temporary_directory:
            spec_path = Path(temporary_directory) / "openapi.yaml"
            server_path = Path(temporary_directory) / "server.go"
            spec_path.write_text("openapi: [")
            server_path.write_text(FEED_HANDLER)
            output = io.StringIO()
            with patch.object(check, "SPEC_PATH", spec_path), patch.object(check, "SERVER_PATH", server_path), redirect_stdout(output):
                result = check.main()
        self.assertEqual(result, 2)
        self.assertIn("cannot read OpenAPI document", output.getvalue())
        self.assertNotIn("Traceback", output.getvalue())

    def test_main_reports_isolated_environment_guidance(self):
        """test_main_reports_isolated_environment_guidance does not mutate the shared venv."""
        output = io.StringIO()
        with patch.object(check.sys, "prefix", "/tmp/not-the-openapi-venv"), redirect_stdout(output):
            result = check.main()
        self.assertEqual(result, 2)
        self.assertIn("run make openapi-setup first", output.getvalue())


if __name__ == "__main__":
    unittest.main()
