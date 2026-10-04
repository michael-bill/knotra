import json
import os
import re
from pathlib import Path


def compare(values, package_root):
    budget = values["monthly_budget"]
    deadline = values["launch_days"]
    weights = values["weights"]
    expected_sources = {"sources/hosted.md", "sources/cms.md", "sources/custom.md"}
    evidence = values["evidence"]
    if {item["source"] for item in evidence} != expected_sources or len(evidence) != 3:
        raise ValueError(
            "Expected independent evidence for each of the three source files"
        )
    ranked = []
    for item in evidence:
        source = (package_root / item["source"]).read_text(encoding="utf-8")
        # Markdown wrapping may insert line breaks inside a quoted sentence;
        # preserve words and punctuation while ignoring layout whitespace.
        normalized_source = " ".join(source.split())
        if any(
            " ".join(quote.split()) not in normalized_source for quote in item["quotes"]
        ):
            raise ValueError(f"A quote does not occur verbatim in {item['source']}")
        # Reject extraction errors before doing arithmetic or asking for a recommendation.
        cost = float(re.search(r"Monthly cost: ([0-9.]+) USD", source).group(1))
        days = int(
            re.search(r"Estimated setup time: ([0-9]+) working days", source).group(1)
        )
        control = re.search(r"control\s*=\s*(\w+)", source).group(1)
        support = re.search(r"support\s*=\s*(\w+)", source).group(1)
        if (
            item["monthly_cost"],
            item["setup_days"],
            item["control"],
            item["support"],
        ) != (cost, days, control, support):
            raise ValueError(
                f"Extracted comparison facts disagree with {item['source']}"
            )
        scores = {
            "cost": max(0, 5 * (1 - cost / budget)),
            "speed": max(0, 5 * (1 - days / deadline)),
            "control": {"full": 5, "theme": 2, "limited": 1}[control],
            "support": {"managed": 5, "community": 2, "internal": 1}[support],
        }
        total = round(
            20
            * sum(scores[key] * weight for key, weight in weights.items())
            / sum(weights.values()),
            2,
        )
        constraints = []
        if cost > budget:
            constraints.append("monthly cost exceeds budget")
        if days > deadline:
            constraints.append("setup estimate exceeds launch window")
        ranked.append(
            {
                **item,
                "scores": scores,
                "weighted_score": total,
                "eligible": not constraints,
                "constraints": constraints,
            }
        )
    ranked.sort(
        key=lambda item: (not item["eligible"], -item["weighted_score"], item["source"])
    )
    eligible = [item for item in ranked if item["eligible"]]
    decision = {
        "selected_source": eligible[0]["source"] if eligible else None,
        "selected_name": eligible[0]["name"] if eligible else None,
        "eligible_sources": [item["source"] for item in eligible],
        "ineligible_sources": [
            item["source"] for item in ranked if not item["eligible"]
        ],
        "rule": "Select the highest weighted score among offers meeting both hard constraints; break ties by source filename. Select none when no offer meets both constraints.",
    }
    return {
        "decision": decision,
        "monthly_budget": budget,
        "launch_days": deadline,
        "weights": weights,
        "method": "Each criterion is scored 0–5. Cost=5*(1-cost/budget), speed=5*(1-days/window), both floored at 0. Control: full=5, theme=2, limited=1. Support: managed=5, community=2, internal=1. Weighted score=20*weighted average. Budget and launch window are hard eligibility constraints. Labor and taxes are excluded from the source prices.",
        "ranked": ranked,
    }


if __name__ == "__main__":
    values = json.loads(Path(os.environ["KNOTRA_INPUT_JSON"]).read_text())["values"]
    result = compare(values, Path("/package"))
    Path(os.environ["KNOTRA_OUTPUT_JSON"]).write_text(
        json.dumps({"comparison": result}), encoding="utf-8"
    )
