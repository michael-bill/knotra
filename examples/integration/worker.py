"""Deterministic data processing stages for the real integration pipeline."""

import csv
import json
import os
from pathlib import Path
import sys
import time


def execute(mode, values, artifacts):
    if mode == "prepare":
        assert os.environ["DATASET_LABEL"]
        rows = [2, 4, 6]
        with Path("dataset.csv").open("w", newline="", encoding="utf-8") as stream:
            writer = csv.writer(stream)
            writer.writerow(["value", "marker"])
            writer.writerows([value, values["marker"]] for value in rows)
        return {"rows": rows, "env_seen": True, "marker": values["marker"]}
    if mode == "double":
        # Complete later items first to exercise ordered concurrent aggregation.
        time.sleep(0.5 * (3 - values["index"]))
        return {"value": values["value"] * 2}
    if mode == "increment":
        time.sleep(0.35)
        return {"count": values["count"] + 1}
    if mode == "fallback":
        return {"approved": False}
    if mode == "bundle":
        assert values["approved"] is True
        assert values["doubled"] == [4, 8, 12]
        assert values["iterations"] == 2
        report = Path(artifacts["report"]["path"]).read_text(encoding="utf-8")
        assert "12" in report and "3" in report
        total = sum(values["doubled"])
        report += f"\nDoubled total: {total}\nIterations: 2\nApproved: true\nRun marker: {values['marker']}\n"
        Path("final.md").write_text(report, encoding="utf-8")
        return {"total": total}
    if mode == "verify":
        report = Path(artifacts["report"]["path"]).read_text(encoding="utf-8")
        assert "Doubled total: 24" in report
        assert "Iterations: 2" in report
        assert "Approved: true" in report
        assert f"Run marker: {values['marker']}" in report
        return {"verified": True}
    raise ValueError(f"unknown stage: {mode}")


if __name__ == "__main__":
    context = json.loads(Path(os.environ["KNOTRA_INPUT_JSON"]).read_text(encoding="utf-8"))
    result = execute(sys.argv[1], context["values"], context["artifacts"])
    Path(os.environ["KNOTRA_OUTPUT_JSON"]).write_text(json.dumps(result), encoding="utf-8")
