#!/usr/bin/env python3
"""Generate reviewed up/down SQL from schema.sql using Atlas Community 1.3.1."""

import argparse
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("name", nargs="?", default="schema_change")
    parser.add_argument("--check", action="store_true", help="Fail if schema.sql needs a migration; do not write files")
    args = parser.parse_args()
    if not re.fullmatch(r"[a-z][a-z0-9_]*", args.name):
        parser.error("name must contain lowercase letters, digits and underscores")
    root = Path(__file__).resolve().parent.parent
    directory = root / "core/catalogschema/extensions"
    base_directory = root / "core/catalogschema/migrations"
    atlas = os.environ.get("ATLAS", "atlas")

    def run(*arguments):
        subprocess.run([atlas, *arguments], cwd=root, check=True)

    # Atlas replays only a disposable SQLite database. Generation never reads or
    # changes the user's catalog, and --check never writes into the repository.
    with tempfile.TemporaryDirectory(prefix="zotigo-migration-") as tmp:
        staging = Path(tmp) / "migrations"
        shutil.copytree(base_directory, staging)
        base_version = max(int(p.name.split("_", 1)[0]) for p in staging.glob("*.up.sql"))
        # Atlas sees one complete history; compatible additions are persisted
        # separately so generation never advances dev.31's base schema marker.
        for path in directory.glob("*.sql"):
            number, suffix = path.name.split("_", 1)
            (staging / f"{base_version + int(number):06d}_{suffix}").write_bytes(path.read_bytes())
        url = f"file://{staging}?format=golang-migrate"
        run("migrate", "hash", "--dir", url)
        before = set(staging.glob("*.sql"))
        run("migrate", "diff", args.name, "--env", "catalog", "--dir", url)
        generated = sorted(set(staging.glob("*.sql")) - before)
        if not generated:
            print("Catalog schema matches migration history.")
            return
        if args.check:
            raise SystemExit("schema.sql differs from migration history; generate and review a migration")
        if len(generated) != 2 or {p.name.rsplit(".", 2)[1] for p in generated} != {"up", "down"}:
            raise SystemExit("Expected an up/down pair; inspect whether this schema change is reversible")
        previous = max(int(p.name.split("_", 1)[0]) for p in directory.glob("*.up.sql"))
        version = previous + 1
        for path in generated:
            direction = path.name.rsplit(".", 2)[1]
            body = path.read_text()
            # Transaction ownership stays with the embedded Go executor.
            if re.search(r"(?im)^\s*(BEGIN|COMMIT|ROLLBACK)\b", body):
                raise SystemExit("Generated SQL contains transaction boundaries; review before importing")
            path.unlink()
            (Path(tmp) / f"{version:06d}_{args.name}.{direction}.sql").write_text(body)
        schema_go = root / "core/catalogschema/extensions.go"
        source, count = re.subn(r"const ExtensionVersion = \d+", f"const ExtensionVersion = {version}", schema_go.read_text())
        if count != 1:
            raise SystemExit("Cannot locate catalogschema.ExtensionVersion; repository was not changed")
        for path in Path(tmp).glob(f"{version:06d}_*.sql"):
            shutil.copyfile(path, directory / path.name)
            print(f"Generated {directory / path.name}")
        schema_go.write_text(source)
        print("Review old-reader/writer compatibility and both SQL files before committing. Base schema stays unchanged.")


if __name__ == "__main__":
    main()
