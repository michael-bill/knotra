import hashlib
import json
import os
import re
import sys
import zipfile
from pathlib import Path

from compare import compare


SOURCES = ("sources/hosted.md", "sources/cms.md", "sources/custom.md")


def verify(values, package_root):
    evidence = values["evidence"]
    if evidence["source"] not in SOURCES:
        raise ValueError("Evidence must refer to a declared source")
    source = (package_root / evidence["source"]).read_text(encoding="utf-8")
    normalized = " ".join(source.split())
    if any(" ".join(quote.split()) not in normalized for quote in evidence["quotes"]):
        raise ValueError("A quotation does not occur in the supplied source")
    expected = (
        float(re.search(r"Monthly cost: ([0-9.]+) USD", source).group(1)),
        int(re.search(r"Estimated setup time: ([0-9]+) working days", source).group(1)),
        re.search(r"control\s*=\s*(\w+)", source).group(1),
        re.search(r"support\s*=\s*(\w+)", source).group(1),
    )
    actual = tuple(
        evidence[key] for key in ("monthly_cost", "setup_days", "control", "support")
    )
    if actual != expected:
        raise ValueError("Extracted facts disagree with the supplied source")
    return {"evidence": evidence}


def score(values, package_root):
    scenarios = json.loads(
        (package_root / "scenarios.json").read_text(encoding="utf-8")
    )
    scenario = scenarios[values["scenario_id"]]
    comparison = compare(
        {
            "evidence": [values[key] for key in ("hosted", "cms", "custom")],
            "monthly_budget": scenario["monthly_budget"],
            "launch_days": scenario["launch_days"],
            "weights": scenario["weights"],
        },
        package_root,
    )
    comparison["scenario"] = scenario
    return {"comparison": comparison}


def report(values, _package_root):
    comparison = values["comparison"]
    scenario = comparison["scenario"]
    decision = comparison["decision"]
    lines = [
        f"## {scenario['title']}",
        "",
        scenario["question"],
        "",
        f"Monthly budget: ${comparison['monthly_budget']}; launch window: {comparison['launch_days']} working days.",
        "Weights: "
        + ", ".join(f"{key}={value}" for key, value in comparison["weights"].items())
        + ".",
        "",
        (
            f"Calculated selection: **{decision['selected_name']}** (`{decision['selected_source']}`)."
            if decision["selected_source"]
            else "**No offer meets both hard constraints.** Increase the budget or launch window before selecting an offer."
        ),
        "",
        "| Source | USD/month | Setup days | Score / 100 | Eligibility |",
        "| --- | ---: | ---: | ---: | --- |",
    ]
    for item in comparison["ranked"]:
        reason = "Eligible" if item["eligible"] else "; ".join(item["constraints"])
        lines.append(
            f"| `{item['source']}` | {item['monthly_cost']} | {item['setup_days']} | {item['weighted_score']} | {reason} |"
        )
    eligible = [item for item in comparison["ranked"] if item["eligible"]]
    if len(eligible) > 1:
        gap = round(eligible[0]["weighted_score"] - eligible[1]["weighted_score"], 2)
        lines.extend(["", f"The two leading eligible offers are {gap} points apart."])
    lines.extend(
        [
            "",
            "Next check: " + scenario["next_check"],
            "",
            "Labor and taxes are excluded.",
        ]
    )
    text = "\n".join(lines) + "\n"
    Path("scenario.md").write_text(text, encoding="utf-8")
    return {"summary": {"comparison": comparison, "markdown": text}}


def group(values, _package_root):
    summaries = [values["first"], values["second"]]
    ids = [item["comparison"]["scenario"]["id"] for item in summaries]
    if len(set(ids)) != 2:
        raise ValueError("A decision brief requires two distinct scenarios")
    text = (
        f"# {values['title']}\n\n"
        + values["question"]
        + "\n\n"
        + "\n".join(item["markdown"] for item in summaries)
    )
    Path("brief.md").write_text(text, encoding="utf-8")
    return {
        "group": {
            "id": values["id"],
            "title": values["title"],
            "question": values["question"],
            "scenarios": summaries,
            "markdown": text,
        }
    }


def portfolio(values, package_root):
    groups = [values[key] for key in ("launch", "cost", "product")]
    summaries = [summary for group in groups for summary in group["scenarios"]]
    scenarios = [summary["comparison"] for summary in summaries]
    ids = [comparison["scenario"]["id"] for comparison in scenarios]
    if len(ids) != 6 or len(set(ids)) != 6:
        raise ValueError("The portfolio must contain six distinct scenarios")
    evidence = {item["source"]: item for item in scenarios[0]["ranked"]}
    hashes = {
        source: hashlib.sha256((package_root / source).read_bytes()).hexdigest()
        for source in SOURCES
    }
    lines = [
        "# Help-center launch portfolio",
        "",
        "Six team scenarios evaluated against three fictional sample offers. Agents read the supplied materials; code verifies quotations and facts, enforces constraints and calculates every selection. This is not live market research.",
        "",
        "## Decisions at a glance",
        "",
        "| Team scenario | Budget / month | Launch days | Calculated selection |",
        "| --- | ---: | ---: | --- |",
    ]
    for comparison in scenarios:
        lines.append(
            f"| {comparison['scenario']['title']} | ${comparison['monthly_budget']} | {comparison['launch_days']} | {comparison['decision']['selected_name'] or 'No eligible offer'} |"
        )
    lines.extend(["", "## Scoring method", "", scenarios[0]["method"], ""])
    lines.extend(group["markdown"] for group in groups)
    lines.extend(["", "## Verified quotations and remaining evidence gaps"])
    for source in SOURCES:
        item = evidence[source]
        lines.extend(["", f"### {item['name']}", "", f"Source: `{source}`", ""])
        lines.extend(f"> {quote}" for quote in item["quotes"])
        lines.append("")
        lines.extend(
            f"- Evidence gap: {limitation}" for limitation in item["limitations"]
        )
    lines.extend(["", "## Source integrity", ""])
    lines.extend(f"- `{source}`: `{digest}`" for source, digest in hashes.items())
    document = "\n".join(lines) + "\n"
    comparison_data = {
        "scenarios": scenarios,
        "source_hashes": hashes,
        "review_scope": "All six calculated decisions, source evidence and explicit next checks.",
    }
    data = json.dumps(comparison_data, ensure_ascii=False, indent=2) + "\n"
    Path("dossier.md").write_text(document, encoding="utf-8")
    Path("comparison.json").write_text(data, encoding="utf-8")
    entries = {"dossier.md": document.encode(), "comparison.json": data.encode()}
    entries.update({source: (package_root / source).read_bytes() for source in SOURCES})
    for summary in summaries:
        scenario_id = summary["comparison"]["scenario"]["id"]
        entries[f"scenarios/{scenario_id}.md"] = summary["markdown"].encode()
    for brief in groups:
        entries[f"briefs/{brief['id']}.md"] = brief["markdown"].encode()
    with zipfile.ZipFile("dossier.zip", "w") as archive:
        for name, content in sorted(entries.items()):
            info = zipfile.ZipInfo(name, date_time=(1980, 1, 1, 0, 0, 0))
            info.compress_type = zipfile.ZIP_DEFLATED
            archive.writestr(info, content)
    return {}


if __name__ == "__main__":
    stage = {
        "verify": verify,
        "score": score,
        "report": report,
        "group": group,
        "portfolio": portfolio,
    }[sys.argv[1]]
    values = json.loads(Path(os.environ["KNOTRA_INPUT_JSON"]).read_text())["values"]
    result = stage(values, Path("/package"))
    Path(os.environ["KNOTRA_OUTPUT_JSON"]).write_text(
        json.dumps(result), encoding="utf-8"
    )
