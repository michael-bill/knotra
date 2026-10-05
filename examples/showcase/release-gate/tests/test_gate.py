import copy
import json
import stat
import sys
import tempfile
import unittest
import zipfile
from pathlib import Path


PACKAGE = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(PACKAGE / "scripts"))
import gate
from build_pipeline import build
from pack_snapshot import pack


class ReleaseGateTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.work = self.root / "work"
        (self.work / "incoming").mkdir(parents=True)
        self.snapshot = self.work / "incoming/repository.zip"
        pack(PACKAGE / "sample", self.snapshot)
        self.files = gate.snapshot_files(self.snapshot)
        self.manifest = gate.load_manifest(self.files)
        self.ids = [item["id"] for item in self.manifest["services"]]
        self.inventory = gate.prepare({"expected_services": self.ids}, self.work)[
            "inventory"
        ]

    def evidence(self):
        inputs = {"inventory": self.inventory}
        for service in self.manifest["services"]:
            for kind in ("tests", "api", "deployment"):
                result = gate.check({"service": service["id"], "kind": kind}, self.work)
                inputs[service["id"] + "_" + kind] = result["report"]
        return gate.aggregate(inputs, self.work)["evidence"]

    def test_real_regressions_and_all_release_gates(self):
        evidence = self.evidence()
        self.assertTrue(evidence["eligible"])
        self.assertEqual(len(evidence["checks"]), 18)
        self.assertEqual(
            sum(
                r["metrics"]["tests"]
                for r in evidence["checks"]
                if r["kind"] == "tests"
            ),
            36,
        )
        self.assertTrue(gate.require_green({"eligible": True}, self.work)["allowed"])

    def test_regression_failure_and_missing_tests_block(self):
        service = self.manifest["services"][0]
        broken = dict(self.files)
        broken["orders/service.py"] = b"def total(items): return -1\n"
        passed, _, _ = gate.regression(broken, service, self.work)
        self.assertFalse(passed)
        empty = {
            name: data
            for name, data in self.files.items()
            if name != "orders/test_service.py"
        }
        # Use a fresh checkout; an old test must not survive a different snapshot.
        fresh = self.root / "empty"
        fresh.mkdir()
        self.assertFalse(gate.regression(empty, service, fresh)[0])

    def test_breaking_request_and_response_schemas_block(self):
        service = self.manifest["services"][0]
        original = json.loads(self.files[service["candidate_api"]])
        for mutate in (
            lambda api: api["operations"].clear(),
            lambda api: next(iter(api["operations"].values()))["response"][
                "properties"
            ].pop("status"),
            lambda api: next(iter(api["operations"].values()))["response"][
                "properties"
            ]["status"]["enum"].append("unknown"),
            lambda api: next(iter(api["operations"].values()))["request"][
                "required"
            ].append("tenant"),
        ):
            api = copy.deepcopy(original)
            next(iter(api["operations"].values()))["request"]["properties"][
                "tenant"
            ] = {"type": "string"}
            mutate(api)
            files = {**self.files, service["candidate_api"]: json.dumps(api).encode()}
            try:
                result = gate.compatibility(files, service)[0]
            except ValueError:
                result = False
            self.assertFalse(result)

    def test_unsupported_schema_is_never_green(self):
        service = self.manifest["services"][0]
        api = json.loads(self.files[service["candidate_api"]])
        next(iter(api["operations"].values()))["request"]["oneOf"] = []
        with self.assertRaisesRegex(ValueError, "Unsupported"):
            gate.compatibility(
                {**self.files, service["candidate_api"]: json.dumps(api).encode()},
                service,
            )

    def test_ambiguous_json_and_unknown_deployment_fields_fail(self):
        for text in [
            '{"privileged":true,"privileged":false}',
            '{"limit":NaN}',
            '{"limit":Infinity}',
        ]:
            with self.assertRaises(ValueError):
                gate.read_json(text)
        service = self.manifest["services"][0]
        config = json.loads(self.files[service["deployment"]])
        config["securityContext"] = {"privileged": True}
        with self.assertRaisesRegex(ValueError, "nine declared"):
            gate.deployment_policy(
                {**self.files, service["deployment"]: json.dumps(config).encode()},
                service,
            )

    def test_unsafe_deployment_blocks_and_gate_cannot_override_it(self):
        service = self.manifest["services"][0]
        config = json.loads(self.files[service["deployment"]])
        for key, value in [
            ("privileged", True),
            ("image", "image:latest"),
            ("run_as_uid", 0),
            ("canary_percent", 50),
            ("memory_mib", True),
        ]:
            with self.subTest(key=key):
                unsafe = {**config, key: value}
                self.assertFalse(
                    gate.deployment_policy(
                        {
                            **self.files,
                            service["deployment"]: json.dumps(unsafe).encode(),
                        },
                        service,
                    )[0]
                )
        with self.assertRaisesRegex(ValueError, "Release blocked"):
            gate.require_green({"eligible": False}, self.work)

    def test_snapshot_rejects_traversal_duplicates_and_links(self):
        for names in [["../escape"], ["/absolute"], ["a\\b"], ["same", "same"]]:
            with self.subTest(names=names):
                path = self.root / "bad.zip"
                with zipfile.ZipFile(path, "w") as archive:
                    for name in names:
                        archive.writestr(name, "content")
                with self.assertRaises(ValueError):
                    gate.snapshot_files(path)
        path = self.root / "link.zip"
        with zipfile.ZipFile(path, "w") as archive:
            entry = zipfile.ZipInfo("link")
            entry.external_attr = (stat.S_IFLNK | 0o777) << 16
            archive.writestr(entry, "/etc/passwd")
        with self.assertRaisesRegex(ValueError, "Links"):
            gate.snapshot_files(path)

    def test_service_inventory_cannot_silently_skip_a_service(self):
        with self.assertRaisesRegex(ValueError, "service set"):
            gate.prepare({"expected_services": self.ids[:-1]}, self.work)

    def test_missing_check_and_mixed_snapshots_are_rejected(self):
        evidence = self.evidence()
        inputs = {item["id"]: item for item in evidence["checks"]}
        inputs["inventory"] = self.inventory
        with self.assertRaisesRegex(ValueError, "missing or duplicate"):
            gate.aggregate(
                {key: value for key, value in inputs.items() if key != "orders/tests"},
                self.work,
            )
        inputs["orders/tests"]["snapshot_sha256"] = "0" * 64
        with self.assertRaisesRegex(ValueError, "different repository snapshots"):
            gate.aggregate(inputs, self.work)

    def test_citations_and_publication_preserve_reviewed_bytes(self):
        evidence = self.evidence()
        reviews = {}
        for scope, kind in [
            ("security", "deployment"),
            ("compatibility", "api"),
            ("operations", "tests"),
        ]:
            items = [item for item in evidence["checks"] if item["kind"] == kind][:2]
            review = {
                "summary": "Assess canary behavior.",
                "recommendations": [
                    "Check canary metrics.",
                    "Confirm rollback ownership.",
                ],
                "citations": [
                    {"check_id": item["id"], "quote": item["statement"]}
                    for item in items
                ],
            }
            reviews[scope] = gate.verify_review(
                {"scope": scope, "review": review, "evidence": evidence}, self.work
            )["review"]
            forged = copy.deepcopy(review)
            forged["citations"][0]["quote"] += " Security certified."
            with self.assertRaisesRegex(ValueError, "citation"):
                gate.verify_review(
                    {"scope": scope, "review": forged, "evidence": evidence}, self.work
                )
        gate.assemble({"evidence": evidence, **reviews}, self.work)
        for name in (
            "release-readiness.md",
            "release-checks.json",
            "release-candidate.zip",
        ):
            (self.work / "incoming" / name).write_bytes((self.work / name).read_bytes())
        values = {
            "approved": True,
            "reviewer": "Release owner",
            "change_ticket": "CHG-123",
            "comments": "Evidence reviewed",
        }
        gate.publish(values, self.work)
        with zipfile.ZipFile(self.work / "authorized-release.zip") as archive:
            self.assertEqual(
                archive.read("release-readiness.md"),
                (self.work / "release-readiness.md").read_bytes(),
            )
            record = json.loads(archive.read("authorization.json"))
            self.assertEqual(
                record["snapshot_sha256"], gate.digest(archive.read("repository.zip"))
            )
        (self.work / "incoming/release-readiness.md").write_text("Changed after review")
        with self.assertRaisesRegex(ValueError, "differs"):
            gate.publish(values, self.work)

    def test_generated_lanes_scale_with_inventory(self):
        pipeline = build(self.ids)
        self.assertEqual(len(pipeline["spec"]["nodes"]), 30)
        self.assertEqual(len(build(self.ids + ["support"])["spec"]["nodes"]), 33)
        self.assertEqual(
            pipeline["spec"]["nodes"]["freeze_snapshot"]["inputs"]["expected_services"][
                "bind"
            ]["value"],
            self.ids,
        )


if __name__ == "__main__":
    unittest.main()
