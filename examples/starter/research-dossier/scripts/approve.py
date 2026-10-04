import hashlib
import json
import os
import zipfile
from pathlib import Path


values = json.loads(Path(os.environ["KNOTRA_INPUT_JSON"]).read_text())["values"]
if values["approved"] is not True:
    raise ValueError("Publication requires approval")
dossier = Path("incoming/dossier.md").read_bytes()
comparison = Path("incoming/comparison.json").read_bytes()
review = {
    "approved": True,
    "reviewer": values["reviewer"],
    "comments": values["comments"],
    "source_dossier_sha256": hashlib.sha256(dossier).hexdigest(),
    "source_comparison_sha256": hashlib.sha256(comparison).hexdigest(),
}
review_bytes = (json.dumps(review, ensure_ascii=False, indent=2) + "\n").encode()
# The dossier stays byte-identical to the material the human reviewed. Approval
# is a separate record so a model cannot rewrite evidence after that decision.
Path("approved-dossier.md").write_bytes(dossier)
Path("review.json").write_bytes(review_bytes)
with zipfile.ZipFile("incoming/dossier.zip") as source:
    if source.read("dossier.md") != dossier:
        raise ValueError("Bundle does not contain the reviewed dossier")
    if source.read("comparison.json") != comparison:
        raise ValueError("Bundle does not contain the reviewed comparison")
    entries = {name: source.read(name) for name in source.namelist()}
entries["review.json"] = review_bytes
with zipfile.ZipFile("approved-dossier.zip", "w") as archive:
    for name, content in sorted(entries.items()):
        info = zipfile.ZipInfo(name, date_time=(1980, 1, 1, 0, 0, 0))
        info.compress_type = zipfile.ZIP_DEFLATED
        archive.writestr(info, content)
Path(os.environ["KNOTRA_OUTPUT_JSON"]).write_text("{}")
