"""Pack an explicit release snapshot, not the user's entire working directory."""

import argparse
from pathlib import Path

from gate import load_manifest, write_zip


def pack(root, output):
    root = root.resolve()
    output = output.resolve()
    if output.is_relative_to(root):
        raise ValueError("Write the ZIP outside its source directory")
    files = {}
    for path in sorted(root.rglob("*")):
        if path.is_symlink():
            raise ValueError("Snapshot links are not supported")
        if path.is_file() and "__pycache__" not in path.parts:
            files[path.relative_to(root).as_posix()] = path.read_bytes()
    load_manifest(files)
    write_zip(output, files)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("root", type=Path)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    pack(args.root, args.output)
