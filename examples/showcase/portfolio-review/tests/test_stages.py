import copy
import hashlib
import json
import os
import subprocess
import sys
import tempfile
import unittest
import zipfile
from pathlib import Path


PACKAGE = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(PACKAGE / "scripts"))
import stages


def source_evidence():
    return {
        "hosted": {
            "source": "sources/hosted.md",
            "name": "HarborHelp",
            "monthly_cost": 139,
            "setup_days": 2,
            "control": "theme",
            "support": "managed",
            "quotes": [
                "The offer includes search, hosting, backups, and a custom domain.",
                "No mobile usability test results are included in the offer.",
            ],
            "limitations": ["No uptime guarantee.", "Mobile usability is not tested."],
        },
        "cms": {
            "source": "sources/cms.md",
            "name": "GrovePress",
            "monthly_cost": 59,
            "setup_days": 7,
            "control": "full",
            "support": "community",
            "quotes": [
                "The search plugin's accessibility has not been tested.",
                "There are no limits on editor accounts in the offer.",
            ],
            "limitations": [
                "No support response commitment.",
                "Maintenance is estimated.",
            ],
        },
        "custom": {
            "source": "sources/custom.md",
            "name": "AtlasDocs",
            "monthly_cost": 29,
            "setup_days": 18,
            "control": "full",
            "support": "internal",
            "quotes": [
                "No prototype or customer usability results are included in the proposal.",
                "There is no included support service or response-time commitment.",
            ],
            "limitations": [
                "No prototype exists.",
                "Engineer availability is assumed.",
            ],
        },
    }


class PortfolioStagesTest(unittest.TestCase):
    def setUp(self):
        self.evidence = source_evidence()
        self.temp = tempfile.TemporaryDirectory()
        self.previous_cwd = Path.cwd()
        os.chdir(self.temp.name)

    def tearDown(self):
        os.chdir(self.previous_cwd)
        self.temp.cleanup()

    def comparison(self, scenario_id):
        return stages.score({"scenario_id": scenario_id, **self.evidence}, PACKAGE)[
            "comparison"
        ]

    def groups(self):
        result = {}
        for group_id, first, second in (
            ("launch", "rush", "tight"),
            ("cost", "lean", "build"),
            ("product", "care", "brand"),
        ):
            summaries = [
                stages.report({"comparison": self.comparison(scenario)}, PACKAGE)[
                    "summary"
                ]
                for scenario in (first, second)
            ]
            result[group_id] = stages.group(
                {
                    "id": group_id,
                    "title": group_id,
                    "question": "Which constraints change the choice?",
                    "first": summaries[0],
                    "second": summaries[1],
                },
                PACKAGE,
            )["group"]
        return result

    def test_source_verification_accepts_markdown_line_wrapping(self):
        for item in self.evidence.values():
            self.assertEqual(
                stages.verify({"evidence": item}, PACKAGE)["evidence"], item
            )

    def test_altered_quotation_is_rejected(self):
        item = copy.deepcopy(self.evidence["hosted"])
        item["quotes"][0] = "The offer includes a guaranteed uptime commitment."
        with self.assertRaisesRegex(ValueError, "quotation"):
            stages.verify({"evidence": item}, PACKAGE)

    def test_altered_fact_is_rejected(self):
        item = copy.deepcopy(self.evidence["cms"])
        item["setup_days"] = 2
        with self.assertRaisesRegex(ValueError, "facts"):
            stages.verify({"evidence": item}, PACKAGE)

    def test_all_scenario_choices_and_infeasibility(self):
        expected = {
            "rush": "sources/hosted.md",
            "lean": "sources/cms.md",
            "build": "sources/custom.md",
            "care": "sources/hosted.md",
            "brand": "sources/cms.md",
            "tight": None,
        }
        for scenario, source in expected.items():
            with self.subTest(scenario=scenario):
                comparison = self.comparison(scenario)
                self.assertEqual(comparison["decision"]["selected_source"], source)
                for item in comparison["ranked"]:
                    self.assertEqual(
                        item["eligible"],
                        item["monthly_cost"] <= comparison["monthly_budget"]
                        and item["setup_days"] <= comparison["launch_days"],
                    )
        self.assertEqual(self.comparison("tight")["decision"]["eligible_sources"], [])

    def test_portfolio_preserves_reports_sources_and_hashes(self):
        stages.portfolio(self.groups(), PACKAGE)
        data = json.loads(Path("comparison.json").read_text())
        self.assertEqual(len(data["scenarios"]), 6)
        with zipfile.ZipFile("dossier.zip") as archive:
            self.assertEqual(
                archive.read("dossier.md"), Path("dossier.md").read_bytes()
            )
            self.assertEqual(
                archive.read("comparison.json"), Path("comparison.json").read_bytes()
            )
            self.assertEqual(
                len(
                    [
                        name
                        for name in archive.namelist()
                        if name.startswith("scenarios/")
                    ]
                ),
                6,
            )
            self.assertEqual(
                len(
                    [name for name in archive.namelist() if name.startswith("briefs/")]
                ),
                3,
            )
            for source in stages.SOURCES:
                content = (PACKAGE / source).read_bytes()
                self.assertEqual(archive.read(source), content)
                self.assertEqual(
                    data["source_hashes"][source], hashlib.sha256(content).hexdigest()
                )

    def test_approval_publishes_identical_reviewed_bytes(self):
        stages.portfolio(self.groups(), PACKAGE)
        reviewed = Path("dossier.md").read_bytes()
        Path("incoming").mkdir()
        for filename in ("dossier.md", "comparison.json", "dossier.zip"):
            Path("incoming", filename).write_bytes(Path(filename).read_bytes())
        Path("input.json").write_text(
            json.dumps(
                {
                    "values": {
                        "approved": True,
                        "reviewer": "Stage test",
                        "comments": "All six scenarios reviewed.",
                    }
                }
            )
        )
        subprocess.run(
            [sys.executable, str(PACKAGE / "scripts" / "approve.py")],
            env={
                **os.environ,
                "KNOTRA_INPUT_JSON": str(Path("input.json").resolve()),
                "KNOTRA_OUTPUT_JSON": str(Path("output.json").resolve()),
            },
            check=True,
        )
        self.assertEqual(Path("approved-dossier.md").read_bytes(), reviewed)
        review = json.loads(Path("review.json").read_text())
        self.assertEqual(
            review["source_dossier_sha256"], hashlib.sha256(reviewed).hexdigest()
        )
        with zipfile.ZipFile("approved-dossier.zip") as archive:
            self.assertEqual(archive.read("dossier.md"), reviewed)
            self.assertEqual(
                archive.read("review.json"), Path("review.json").read_bytes()
            )


if __name__ == "__main__":
    unittest.main()
