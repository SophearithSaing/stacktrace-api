#!/usr/bin/env python3
"""Check the raw OpenAPI contract without network access or installation."""

from __future__ import annotations

import re
import sys
from pathlib import Path
from typing import Any

if sys.version_info < (3, 11):
    print("OpenAPI checks require Python 3.11+; run make openapi-setup with a supported Python.")
    raise SystemExit(2)

try:
    import yaml
    from jsonschema import Draft202012Validator, FormatChecker
    from openapi_spec_validator import OpenAPIV31SpecValidator
    from referencing import Registry, Resource
    from referencing.exceptions import NoSuchResource
    from referencing.jsonschema import DRAFT202012
except ImportError:
    print("OpenAPI dependencies are unavailable; run make openapi-setup first.")
    raise SystemExit(2)


ROOT = Path(__file__).resolve().parents[2]
SPEC_PATH = ROOT / "docs/openapi.yaml"
SERVER_PATH = ROOT / "internal/api/server.go"
OPERATION_KEYS = {"get", "put", "post", "delete", "options", "head", "patch", "trace"}
REFERENCE_KEYS = {"$ref", "$dynamicRef"}


def pointer(document: Any, reference: str) -> Any:
    """pointer resolves one internal JSON Pointer reference from document."""
    if not reference.startswith("#/"):
        raise ValueError("reference must be an internal JSON Pointer")
    value = document
    for token in reference[2:].split("/"):
        token = token.replace("~1", "/").replace("~0", "~")
        if isinstance(value, list):
            value = value[int(token)]
        elif isinstance(value, dict):
            value = value[token]
        else:
            raise TypeError("pointer traverses a scalar")
    return value


def references(document: Any, value: Any, location: str = "$") -> list[str]:
    """references returns invalid or unresolved reference diagnostics in value."""
    findings: list[str] = []
    if isinstance(value, dict):
        for key, child in value.items():
            child_location = f"{location}.{key}"
            if key in REFERENCE_KEYS and isinstance(child, str):
                if not child.startswith("#/"):
                    findings.append(f"{child_location}: external reference {child!r} is not allowed")
                else:
                    try:
                        pointer(document, child)
                    except (KeyError, IndexError, TypeError, ValueError):
                        findings.append(f"{child_location}: unresolved reference {child!r}")
            findings.extend(references(document, child, child_location))
    elif isinstance(value, list):
        for index, child in enumerate(value):
            findings.extend(references(document, child, f"{location}[{index}]"))
    return findings


def reference_cycles(document: Any, value: Any, location: str = "$", in_schema: bool = False) -> list[str]:
    """reference_cycles returns diagnostics for local Reference Object cycles."""
    findings: list[str] = []
    if in_schema:
        return findings
    if isinstance(value, dict):
        reference = value.get("$ref")
        if isinstance(reference, str) and reference.startswith("#/"):
            seen: set[str] = set()
            current = reference
            while True:
                if current in seen:
                    findings.append(f"{location}.$ref: cyclic reference {current!r}")
                    break
                seen.add(current)
                try:
                    target = pointer(document, current)
                except (KeyError, IndexError, TypeError, ValueError):
                    break
                if not isinstance(target, dict) or not isinstance(target.get("$ref"), str):
                    break
                current = target["$ref"]
                if not current.startswith("#/"):
                    break
        for key, child in value.items():
            child_is_schema = key == "schema" or (location == "$.components" and key == "schemas")
            findings.extend(reference_cycles(document, child, f"{location}.{key}", child_is_schema))
    elif isinstance(value, list):
        for index, child in enumerate(value):
            findings.extend(reference_cycles(document, child, f"{location}[{index}]", in_schema))
    return findings


def resolve_object(document: dict[str, Any], value: Any, location: str, kind: str, findings: list[str]) -> dict[str, Any] | None:
    """resolve_object follows a local Reference Object chain to an object."""
    seen: set[str] = set()
    while isinstance(value, dict) and "$ref" in value:
        reference = value["$ref"]
        if not isinstance(reference, str) or not reference.startswith("#/"):
            findings.append(f"{location}: invalid {kind} reference")
            return None
        if reference in seen:
            findings.append(f"{location}: cyclic {kind} reference {reference!r}")
            return None
        seen.add(reference)
        try:
            value = pointer(document, reference)
        except (KeyError, IndexError, TypeError, ValueError):
            findings.append(f"{location}: unresolved {kind} reference {reference!r}")
            return None
    if not isinstance(value, dict):
        findings.append(f"{location}: {kind} reference must resolve to an object")
        return None
    return value


def schema_validator(document: dict[str, Any], schema: Any) -> Draft202012Validator:
    """schema_validator creates a format-aware validator with document-local references."""
    wrapper = dict(document)
    wrapper.update({"$ref": "#/schema", "schema": schema})

    def reject_retrieve(uri: str) -> Resource[Any]:
        """reject_retrieve prevents schema validation from retrieving remote resources."""
        raise NoSuchResource(ref=uri)

    registry = Registry(retrieve=reject_retrieve).with_resource(
        "", Resource.from_contents(wrapper, default_specification=DRAFT202012)
    )
    return Draft202012Validator(wrapper, registry=registry, format_checker=FormatChecker())


def validate_examples(document: dict[str, Any]) -> list[str]:
    """validate_examples validates schema and media-type examples against local schemas."""
    findings: list[str] = []

    def check_examples(schema: Any, examples: Any, location: str) -> None:
        if schema is None or not isinstance(examples, list):
            return
        try:
            validator = schema_validator(document, schema)
        except (TypeError, ValueError) as error:
            findings.append(f"{location}: invalid schema: {error}")
            return
        for index, example in enumerate(examples):
            try:
                error = next(validator.iter_errors(example), None)
            except Exception as error:
                findings.append(f"{location}[{index}]: cannot validate example: {error}")
                continue
            if error:
                findings.append(f"{location}[{index}]: {error.message}")

    def schema_nodes(value: Any, location: str) -> None:
        if isinstance(value, dict):
            if "examples" in value:
                check_examples(value, value["examples"], location)
            for key in ("$defs", "definitions", "dependentSchemas", "patternProperties", "properties"):
                children = value.get(key)
                if isinstance(children, dict):
                    for name, child in children.items():
                        schema_nodes(child, f"{location}.{key}.{name}")
            for key in ("additionalProperties", "additionalItems", "contains", "contentSchema", "else", "if", "items", "not", "propertyNames", "then", "unevaluatedItems", "unevaluatedProperties"):
                if key in value:
                    schema_nodes(value[key], f"{location}.{key}")
            for key in ("allOf", "anyOf", "oneOf", "prefixItems"):
                children = value.get(key)
                if isinstance(children, list):
                    for index, child in enumerate(children):
                        schema_nodes(child, f"{location}.{key}[{index}]")

    def parameter_examples(parameter: Any, location: str) -> None:
        parameter = resolve_object(document, parameter, location, "parameter", findings)
        if parameter is not None and "schema" in parameter:
            name = parameter.get("name")
            if isinstance(name, str):
                location = f"{location} {name}"
            schema_nodes(parameter["schema"], location + " schema")

    def header_examples(header: Any, location: str) -> None:
        header = resolve_object(document, header, location, "header", findings)
        if header is not None and "schema" in header:
            schema_nodes(header["schema"], location + " schema")

    def media_examples(media: Any, location: str) -> None:
        media = resolve_object(document, media, location, "media type", findings)
        if media is None:
            return
        schema = media.get("schema")
        if schema is not None:
            schema_nodes(schema, f"{location} schema")
        if "example" in media:
            check_examples(schema, [media["example"]], location + " example")
        examples = media.get("examples", {})
        if isinstance(examples, dict):
            for key, example in examples.items():
                example = resolve_object(document, example, f"{location} example {key}", "example", findings)
                if example is not None and "value" in example:
                    check_examples(schema, [example["value"]], location + f" example {key}")

    def request_body_examples(request_body: Any, location: str) -> None:
        request_body = resolve_object(document, request_body, location, "request body", findings)
        if request_body is not None:
            for kind, media in request_body.get("content", {}).items():
                media_examples(media, f"{location} {kind}")

    def response_examples(response: Any, location: str) -> None:
        response = resolve_object(document, response, location, "response", findings)
        if response is not None:
            for name, header in response.get("headers", {}).items():
                header_examples(header, f"{location} header {name}")
            for kind, media in response.get("content", {}).items():
                media_examples(media, f"{location} {kind}")

    components = document.get("components", {})
    for name, schema in components.get("schemas", {}).items():
        schema_nodes(schema, f"schema {name}")
    for name, parameter in components.get("parameters", {}).items():
        parameter_examples(parameter, f"component parameter {name}")
    for name, header in components.get("headers", {}).items():
        header_examples(header, f"component header {name}")
    for name, request_body in components.get("requestBodies", {}).items():
        request_body_examples(request_body, f"component request body {name}")
    for name, response in components.get("responses", {}).items():
        response_examples(response, f"component response {name}")

    for path, item in document.get("paths", {}).items():
        item = resolve_object(document, item, f"path {path}", "path item", findings)
        if item is None:
            continue
        for parameter in item.get("parameters", []):
            parameter_examples(parameter, f"path {path} parameter")
        for method, operation in item.items():
            if method not in OPERATION_KEYS:
                continue
            operation = resolve_object(document, operation, f"{method.upper()} {path}", "operation", findings)
            if operation is None:
                continue
            for parameter in operation.get("parameters", []):
                parameter_examples(parameter, f"{method.upper()} {path} parameter")
            request_body = operation.get("requestBody")
            if request_body is not None:
                request_body_examples(request_body, f"{method.upper()} {path} request")
            for status, response in operation.get("responses", {}).items():
                response_examples(response, f"{method.upper()} {path} response {status}")
    return findings


def operations(document: dict[str, Any]) -> tuple[set[tuple[str, str]], list[str]]:
    """operations returns documented operations and operation-ID diagnostics."""
    found: set[tuple[str, str]] = set()
    findings: list[str] = []
    seen: set[str] = set()
    for path, item in document.get("paths", {}).items():
        item = resolve_object(document, item, f"path {path}", "path item", findings)
        if item is None:
            continue
        for method, operation in item.items():
            if method not in OPERATION_KEYS:
                continue
            operation = resolve_object(document, operation, f"{method.upper()} {path}", "operation", findings)
            if operation is None:
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
    """source_routes reads explicit s.mux method/path registrations from server source."""
    routes: set[tuple[str, str]] = set()
    findings: list[str] = []
    for method, path in re.findall(r's\.mux\.HandleFunc\("([^\s",/]+)\s+([^"]+)"', server_source):
        if method == "GET" and path == "/api/v1/accounts/{accountID}/{resource}":
            path = "/api/v1/accounts/{accountID}/feed"
        routes.add((method, path))
    return routes, findings


def error_location(error: Any) -> str:
    """error_location formats a validation error's absolute document path."""
    location = "$"
    for token in error.absolute_path:
        if isinstance(token, int):
            location += f"[{token}]"
        else:
            location += f".{token}"
    return location


def check(document: dict[str, Any], server_source: str) -> list[str]:
    """check returns all raw-document, example, operation, and route diagnostics."""
    if not isinstance(document, dict):
        return ["OpenAPI document must be an object"]
    findings = references(document, document)
    findings.extend(reference_cycles(document, document))
    if findings:
        return findings
    try:
        structural_errors = list(OpenAPIV31SpecValidator(document).iter_errors())
    except Exception as error:
        return [f"strict document: validation failed: {error}"]
    if structural_errors:
        return [f"strict document at {error_location(error)}: {error.message}" for error in structural_errors]
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
    if Path(sys.prefix).resolve() != (ROOT / "tools/openapi/.venv").resolve():
        print("OpenAPI dependencies are unavailable; run make openapi-setup first.")
        return 2
    try:
        document = yaml.safe_load(SPEC_PATH.read_text())
    except (OSError, yaml.YAMLError) as error:
        print(f"cannot read OpenAPI document {SPEC_PATH}: {error}")
        return 2
    try:
        server_source = SERVER_PATH.read_text()
    except OSError as error:
        print(f"cannot read API source {SERVER_PATH}: {error}")
        return 2
    findings = check(document, server_source)
    if findings:
        print("FAIL OpenAPI validation")
        print(*findings, sep="\n")
        return 1
    documented, _ = operations(document)
    schemas = document.get("components", {}).get("schemas", {})
    examples = sum(len(schema.get("examples", [])) for schema in schemas.values() if isinstance(schema, dict))
    print(f"PASS OpenAPI: {len(documented)} routes, {examples} schema examples")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
