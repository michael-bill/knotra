"""Create a unique file and verify the same bytes after an engine restart."""

import json
import os
from pathlib import Path
import sys
import uuid


context = json.loads(Path(os.environ["KNOTRA_INPUT_JSON"]).read_text(encoding="utf-8"))
values = context["values"]
if sys.argv[1] == "prepare":
    nonce = str(uuid.uuid4())
    document = {"nonce": nonce, "marker": values["marker"]}
    Path("checkpoint.json").write_text(json.dumps(document), encoding="utf-8")
    output = {"nonce": nonce}
elif sys.argv[1] == "verify":
    path = context["artifacts"]["document"]["path"]
    document = json.loads(Path(path).read_text(encoding="utf-8"))
    assert document == {"nonce": values["nonce"], "marker": values["marker"]}
    assert values["approved"] is True
    output = {"nonce": document["nonce"], "verified": True}
else:
    raise ValueError(f"unknown stage: {sys.argv[1]}")
Path(os.environ["KNOTRA_OUTPUT_JSON"]).write_text(json.dumps(output), encoding="utf-8")
