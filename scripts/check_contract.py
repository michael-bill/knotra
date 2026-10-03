#!/usr/bin/env python3
"""Check contract fixtures; this is not Knotra's parser or semantic validator."""

import json
import sys
from collections import Counter
from pathlib import Path

from jsonschema import Draft202012Validator
from jsonschema.exceptions import SchemaError
from ruamel.yaml import YAML
from ruamel.yaml.error import YAMLError


ROOT = Path(__file__).resolve().parents[1]
FIXTURES = ROOT / "contracts/v1/fixtures"


def read_yaml(path):
    loader = YAML(typ="safe")
    loader.version = (1, 2)
    loader.allow_duplicate_keys = False
    return loader.load(path.read_text(encoding="utf-8"))


def main():
    manifest = json.loads((FIXTURES / "manifest.json").read_text(encoding="utf-8"))
    schema = json.loads((FIXTURES / manifest["schema"]).read_text(encoding="utf-8"))
    Draft202012Validator.check_schema(schema)
    validator = Draft202012Validator(schema)
    data_schema_validator = Draft202012Validator(
        {"$ref": "#/$defs/DataSchema", "$defs": schema["$defs"]}
    )
    counts = Counter()
    failures = []
    ids = set()

    for case in manifest["cases"]:
        case_id = case["id"]
        if case_id in ids:
            failures.append(f"{case_id}: duplicate manifest ID")
        ids.add(case_id)
        expected = case["expected"]
        try:
            document = read_yaml(FIXTURES / case["document"])
            parsed = "accept"
        except YAMLError:
            parsed = "reject"
        counts[f"parse {parsed}"] += 1
        if parsed != expected["parse"]:
            failures.append(f"{case_id}: parse {parsed}, expected {expected['parse']}")
        if parsed == "reject":
            if expected["structural"] != "not_applicable":
                failures.append(
                    f"{case_id}: cannot check structure after parse rejection"
                )
            continue

        accepted = validator.is_valid(document)
        structural = "accept" if accepted else "reject"
        counts[f"structural {structural}"] += 1
        if structural != expected["structural"]:
            failures.append(
                f"{case_id}: structural {structural}, expected {expected['structural']}"
            )

        # Supporting-file checks are limited to positive static-contract fixtures.
        if case["document"].startswith("positive/") and accepted:
            if (
                "engineProfile" in case
                and not (FIXTURES / case["engineProfile"]).is_file()
            ):
                failures.append(f"{case_id}: engine profile file is missing")
            if "packageRoot" not in case:
                continue
            package_root = FIXTURES / case["packageRoot"]
            for filename in document["spec"].get("files", []):
                if not (package_root / filename).is_file():
                    failures.append(
                        f"{case_id}: declared package file is missing: {filename}"
                    )
            for source in document["spec"].get("schemas", {}).values():
                if not isinstance(source, dict) or "file" not in source:
                    continue
                data_schema = read_yaml(package_root / source["file"])
                Draft202012Validator.check_schema(data_schema)
                if not data_schema_validator.is_valid(data_schema):
                    failures.append(f"{case_id}: data schema exceeds the v1 profile")

    print("Metaschema: PASS (JSON Schema Draft 2020-12)")
    print(f"Fixtures: {len(manifest['cases'])}")
    print(
        f"Parse: {counts['parse accept']} accepted, {counts['parse reject']} rejected"
    )
    print(
        "Structural: "
        f"{counts['structural accept']} accepted, {counts['structural reject']} rejected"
    )
    print("Semantic: NOT RUN (manifest expectations only)")
    print("Runtime/CEL/providers/MCP/commands: NOT RUN")
    if failures:
        for failure in failures:
            print(f"FAIL: {failure}", file=sys.stderr)
        return 1
    print("PASS: all checked fixture expectations match")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (OSError, ValueError, KeyError, YAMLError, SchemaError) as error:
        print(f"ERROR: {error}", file=sys.stderr)
        sys.exit(1)
