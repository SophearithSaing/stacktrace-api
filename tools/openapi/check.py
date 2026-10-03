#!/usr/bin/env python3
"""Check the raw OpenAPI contract without network access or installation."""

from __future__ import annotations

import re
import sys
from pathlib import Path
from typing import Any

import yaml
from jsonschema import Draft202012Validator, FormatChecker
from openapi_spec_validator import OpenAPIV31SpecValidator


ROOT = Path(__file__).resolve().parents[2]
SPEC_PATH = ROOT / "docs/openapi.yaml"
SERVER_PATH = ROOT / "internal/api/server.go"
OPERATION_KEYS = {"get", "put", "post", "delete", "options", "head", "patch", "trace"}


def pointer(document: Any, reference: str) -> Any:
    """pointer resolves one internal JSON Pointer reference from document."""
    if not reference.startswith("#/"):
        raise ValueError("reference must be an internal JSON Pointer")
    value = document
    for token in reference[2:].split("/"):
        token = token.replace("~1", "/").replace("~0", "~")
        if isinstance(value, list):
            value = value[int(token)]
        else:
            value = value[token]
    return value


def references(value: Any, location: str = "$") -> list[str]:
    """references returns invalid or unresolved reference diagnostics in value."""
    findings: list[str] = []
    if isinstance(value, dict):
        for key, child in value.items():
            child_location = f"{location}.{key}"
            if key == "$ref" and isinstance(child, str):
                if not child.startswith("#/"):
                    findings.append(f"{child_location}: external reference {child!r} is not allowed")
                else:
                    try:
                        pointer(value_root, child)
                    except (KeyError, IndexError, ValueError):
                        findings.append(f"{child_location}: unresolved reference {child!r}")
            findings.extend(references(child, child_location))
    elif isinstance(value, list):
        for index, child in enumerate(value):
            findings.extend(references(child, f"{location}[{index}]"))
    return findings


def schema_validator(document: dict[str, Any], schema: Any) -> Draft202012Validator:
    """schema_validator creates a format-aware validator with document-local references."""
    wrapper = {"$ref": "#/schema", "schema": schema, "components": document.get("components", {})}
    return Draft202012Validator(wrapper, format_checker=FormatChecker())


def validate_examples(document: dict[str, Any]) -> list[str]:
    """validate_examples validates schema and media-type examples against local schemas."""
    findings: list[str] = []

    def check_examples(schema: Any, examples: Any, location: str) -> None:
        if not schema or not isinstance(examples, list):
            return
        validator = schema_validator(document, schema)
        for index, example in enumerate(examples):
            error = next(validator.iter_errors(example), None)
            if error:
                findings.append(f"{location}[{index}]: {error.message}")

    for name, schema in document.get("components", {}).get("schemas", {}).items():
        if isinstance(schema, dict):
            check_examples(schema, schema.get("examples"), f"schema {name} example")

    for path, item in document.get("paths", {}).items():
        if not isinstance(item, dict):
            continue
        for method, operation in item.items():
            if method not in OPERATION_KEYS or not isinstance(operation, dict):
                continue
            media_locations: list[tuple[str, Any]] = []
            request = operation.get("requestBody", {}).get("content", {})
            media_locations.extend((f"{method.upper()} {path} request {kind}", media) for kind, media in request.items())
            for status, response in operation.get("responses", {}).items():
                for kind, media in response.get("content", {}).items():
                    media_locations.append((f"{method.upper()} {path} response {status} {kind}", media))
            for location, media in media_locations:
                if not isinstance(media, dict):
                    continue
                schema = media.get("schema")
                if "example" in media:
                    check_examples(schema, [media["example"]], location + " example")
                for key, example in media.get("examples", {}).items():
                    if isinstance(example, dict) and "value" in example:
                        check_examples(schema, [example["value"]], location + f" example {key}")
    return findings


def operations(document: dict[str, Any]) -> tuple[set[tuple[str, str]], list[str]]:
    """operations returns documented operations and operation-ID diagnostics."""
    found: set[tuple[str, str]] = set()
    findings: list[str] = []
    seen: set[str] = set()
    for path, item in document.get("paths", {}).items():
        if not isinstance(item, dict):
            continue
        for method, operation in item.items():
            if method not in OPERATION_KEYS:
                continue
            if not isinstance(operation, dict):
                findings.append(f"{method.upper()} {path}: operation must be an object")
                continue
            found.add((method.upper(), path))
            operation_id = operation.get("operationId")
            if not isinstance(operation_id, str) or not operation_id:
                findings.append(f"{method.upper()} {path}: missing nonempty operationId")
            elif operation_id in seen:
                findings.append(f"{method.upper()} {path}: duplicate operationId {operation_id!r}")
            else:
                seen.add(operation_id)
    return found, findings


def source_routes(server_source: str) -> tuple[set[tuple[str, str]], list[str]]:
    """source_routes reads explicit method/path registrations from server source."""
    routes: set[tuple[str, str]] = set()
    findings: list[str] = []
    for method, path in re.findall(r'HandleFunc\("([A-Za-z]+) ([^"]+)"', server_source):
        if method.upper() != method:
            findings.append(f"source route {method} {path}: method token must be uppercase")
        routes.add((method.upper(), path.replace("/{accountID}/{resource}", "/{accountID}/feed")))
    return routes, findings


def check(document: dict[str, Any], server_source: str) -> list[str]:
    """check returns all raw-document, example, operation, and route diagnostics."""
    global value_root
    value_root = document
    findings = references(document)
    if not findings:
        findings.extend(f"strict document: {error.message}" for error in OpenAPIV31SpecValidator(document).iter_errors())
    findings.extend(validate_examples(document))
    documented, operation_findings = operations(document)
    source, source_findings = source_routes(server_source)
    findings.extend(operation_findings)
    findings.extend(source_findings)
    findings.extend(f"missing OpenAPI route: {method} {path}" for method, path in sorted(source - documented))
    findings.extend(f"stale OpenAPI route: {method} {path}" for method, path in sorted(documented - source))
    return findings


def main() -> int:
    """main runs the repository checker and prints concise diagnostics."""
    if sys.version_info < (3, 11):
        print("OpenAPI checks require Python 3.11+; run make openapi-setup with a supported Python.")
        return 2
    if Path(sys.prefix).resolve() != (ROOT / "tools/openapi/.venv").resolve():
        print("OpenAPI dependencies are unavailable; run make openapi-setup first.")
        return 2
    document = yaml.safe_load(SPEC_PATH.read_text())
    findings = check(document, SERVER_PATH.read_text())
    if findings:
        print("FAIL OpenAPI validation")
        print(*findings, sep="\n")
        return 1
    documented, _ = operations(document)
    examples = sum(len(schema.get("examples", [])) for schema in document["components"]["schemas"].values() if isinstance(schema, dict))
    print(f"PASS OpenAPI: {len(documented)} routes, {examples} schema examples")
    return 0


value_root: Any = {}

if __name__ == "__main__":
    raise SystemExit(main())
