import copy
import importlib.util
import unittest
from pathlib import Path


ROOT = Path(__file__).parent / "research-dossier"
spec = importlib.util.spec_from_file_location(
    "research_compare", ROOT / "scripts/compare.py"
)
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


def inputs():
    offers = [
        ("hosted", "HarborHelp", 139, 2, "theme", "managed"),
        ("cms", "GrovePress", 59, 7, "full", "community"),
        ("custom", "AtlasDocs", 29, 18, "full", "internal"),
    ]
    return {
        "monthly_budget": 200,
        "launch_days": 10,
        "weights": {"cost": 25, "speed": 35, "control": 25, "support": 15},
        "evidence": [
            {
                "source": f"sources/{file}.md",
                "name": name,
                "monthly_cost": cost,
                "setup_days": days,
                "control": control,
                "support": support,
                "quotes": [
                    f"Monthly cost: {cost} USD.",
                    f"Estimated setup time: {days} working days.",
                ],
                "limitations": ["Fictional sample offer", "Labor costs excluded"],
            }
            for file, name, cost, days, control, support in offers
        ],
    }


class ResearchComparisonTests(unittest.TestCase):
    def test_weights_and_constraints_change_the_recommendation_basis(self):
        values = inputs()
        original = copy.deepcopy(values)
        result = module.compare(values, ROOT)
        self.assertEqual(values, original)
        self.assertEqual(
            [item["name"] for item in result["ranked"]],
            ["HarborHelp", "GrovePress", "AtlasDocs"],
        )
        self.assertAlmostEqual(result["ranked"][0]["weighted_score"], 60.62, places=2)
        self.assertFalse(result["ranked"][2]["eligible"])
        self.assertEqual(result["decision"]["selected_source"], "sources/hosted.md")
        self.assertEqual(
            result["decision"]["eligible_sources"],
            ["sources/hosted.md", "sources/cms.md"],
        )
        self.assertEqual(
            result["decision"]["ineligible_sources"], ["sources/custom.md"]
        )
        values["launch_days"] = 20
        flexible = module.compare(values, ROOT)
        self.assertEqual(flexible["ranked"][0]["name"], "GrovePress")
        self.assertTrue(all(item["eligible"] for item in flexible["ranked"]))
        self.assertEqual(flexible["decision"]["selected_source"], "sources/cms.md")
        values["monthly_budget"] = 20
        self.assertFalse(
            any(item["eligible"] for item in module.compare(values, ROOT)["ranked"])
        )
        self.assertIsNone(module.compare(values, ROOT)["decision"]["selected_source"])

    def test_rejects_invented_quotes_and_misread_numerical_facts(self):
        for change in (
            {"quotes": ["The vendor guarantees zero downtime."]},
            {"monthly_cost": 1},
        ):
            values = inputs()
            values["evidence"][0].update(change)
            with self.assertRaises(ValueError):
                module.compare(values, ROOT)

    def test_requires_each_independent_source_exactly_once(self):
        values = inputs()
        values["evidence"][2] = copy.deepcopy(values["evidence"][0])
        with self.assertRaisesRegex(ValueError, "each of the three source files"):
            module.compare(values, ROOT)

    def test_markdown_line_wrapping_does_not_change_quote_evidence(self):
        values = inputs()
        values["evidence"][1]["quotes"][
            0
        ] = "The search plugin's accessibility has not been tested."
        self.assertEqual(len(module.compare(values, ROOT)["ranked"]), 3)


if __name__ == "__main__":
    unittest.main()
