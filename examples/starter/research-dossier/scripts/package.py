import json
import os
import zipfile
from pathlib import Path


def cell(value):
    return str(value).replace("|", "\\|").replace("\n", " ")


values = json.loads(Path(os.environ["KNOTRA_INPUT_JSON"]).read_text())["values"]
comparison = values["comparison"]
decision = comparison["decision"]
lines = [
    "# Help-center launch comparison",
    "",
    "Fictional bundled sample offers. This dossier compares supplied materials; it is not live web research or a verification of vendor claims.",
    "",
    "## Selection calculated by the engine",
    (
        f"Selected source: `{decision['selected_source']}` ({decision['selected_name']})."
        if decision["selected_source"]
        else "No option meets both hard constraints; no option was selected."
    ),
    "Eligible sources: "
    + (", ".join(f"`{source}`" for source in decision["eligible_sources"]) or "none")
    + ".",
    "Ineligible sources: "
    + (", ".join(f"`{source}`" for source in decision["ineligible_sources"]) or "none")
    + ".",
    decision["rule"],
    "",
    "## Model explanation and next checks",
    values["recommendation"],
    "",
    "## Reproducible comparison",
    comparison["method"],
    "",
    f"Monthly budget: ${comparison['monthly_budget']}; launch window: {comparison['launch_days']} working days.",
    "Weights: "
    + ", ".join(f"{key}={value}" for key, value in comparison["weights"].items())
    + ".",
    "",
    "| Option | USD/month | Setup days | Score / 100 | Meets constraints |",
    "| --- | ---: | ---: | ---: | --- |",
]
for item in comparison["ranked"]:
    lines.append(
        f"| {cell(item['name'])} | {item['monthly_cost']} | {item['setup_days']} | {item['weighted_score']} | {'Yes' if item['eligible'] else cell('; '.join(item['constraints']))} |"
    )
lines.extend(["", "## Evidence and source list"])
for item in comparison["ranked"]:
    lines.extend(["", f"### {item['name']}", f"Source: `{item['source']}`", ""])
    lines.extend(f"> {quote}" for quote in item["quotes"])
    lines.append("")
    lines.extend(f"- Limitation: {limitation}" for limitation in item["limitations"])
document = "\n".join(lines) + "\n"
data = json.dumps(comparison, ensure_ascii=False, indent=2) + "\n"
Path("dossier.md").write_text(document, encoding="utf-8")
Path("comparison.json").write_text(data, encoding="utf-8")
entries = {"dossier.md": document.encode(), "comparison.json": data.encode()}
for item in comparison["ranked"]:
    entries[item["source"]] = (Path("/package") / item["source"]).read_bytes()
with zipfile.ZipFile("dossier.zip", "w") as archive:
    for name, content in sorted(entries.items()):
        info = zipfile.ZipInfo(name, date_time=(1980, 1, 1, 0, 0, 0))
        info.compress_type = zipfile.ZIP_DEFLATED
        archive.writestr(info, content)
Path(os.environ["KNOTRA_OUTPUT_JSON"]).write_text("{}")
