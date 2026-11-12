#!/usr/bin/env python3
"""Run local capability/correctness probes; never calls a model or starts a daemon."""
import json
from pathlib import Path
import shutil
import subprocess
import tempfile

artifacts = Path(__file__).resolve().parent
repo = artifacts.parents[2]
with tempfile.TemporaryDirectory(prefix="fae-capability-audit-") as directory:
    work = Path(directory)
    replacements = {}
    for package in ("scheduler", "daemon", "provider", "engine"):
        source = work / f"{package}_repro_test.go"
        shutil.copyfile(artifacts / f"{package}_repro_test.go.txt", source)
        replacements[str(repo / package / "audit_capability_overlay_test.go")] = str(source)
    overlay = work / "overlay.json"
    overlay.write_text(json.dumps({"Replace": replacements}))
    commands = [
        (["go", "test", "-overlay", str(overlay), "./scheduler", "./daemon", "./engine", "-run", "^TestAudit", "-v", "-count=1", "-timeout=30s"], 0),
        # These assertions describe required behavior and fail on the audited code.
        (["go", "test", "-overlay", str(overlay), "./provider", "-run", "^TestAuditReviewIndependentReadWriteScopes$", "-v", "-count=1", "-timeout=30s"], 1),
    ]
    for name, arguments in (("cycle", []), ("recursion", []), ("footprint", ["1800"]), ("nested-item", []), ("nested", [])):
        source = work / f"{name}.go"
        shutil.copyfile(artifacts / f"{name}.go.txt", source)
        commands.append((["go", "run", str(source), *arguments], 0))
    unexpected = False
    for command, expected in commands:
        print("\n$", " ".join(command), flush=True)
        result = subprocess.run(command, cwd=repo, check=False, timeout=90)
        print(f"exit={result.returncode}; audited expectation={expected}", flush=True)
        unexpected |= result.returncode != expected
    raise SystemExit(1 if unexpected else 0)
