"""Measure real CLI multi-call baselines for locally supplied questions.

Usage: python3 docs/spikes/run-251-cli.py /path/to/repository /path/to/results /path/to/local-cases.json
Uses the built ./grafo. Each existing CLI call performs its normal freshness
check; the prototype deliberately excludes freshness, so latency is not an
apples-to-apples persistent MCP comparison.
"""
import json
import pathlib
import subprocess
import sys
import time

root, output, corpus = sys.argv[1:]
out = pathlib.Path(output)
rows = []
for entry in json.loads(pathlib.Path(corpus).read_text()):
    case, term = entry["id"], entry["baseline"]
    commands = [["find", term, "--limit", "5"]]
    responses = []
    start = time.monotonic()
    def call(args):
        response = subprocess.run(["./grafo", *args, "--repo", root, "--json"],
                                  capture_output=True, check=True, timeout=120)
        value = json.loads(response.stdout)
        responses.append(value)
        return value
    candidates = call(commands[0])
    for node in candidates[:3]:
        selector = node["id"]
        for args in [["neighbors", selector, "--direction", "both", "--relation", "calls,references,tests", "--depth", "1", "--limit", "12"],
                     ["find-tests", selector, "--depth", "2", "--limit", "12"],
                     ["source", selector, "--context-lines", "0", "--max-lines", "30"]]:
            commands.append(args)
            call(args)
    elapsed = (time.monotonic() - start) * 1000
    encoded = json.dumps(responses, separators=(",", ":"), ensure_ascii=False).encode()
    (out / f"{case}-cli.json").write_bytes(encoded)
    row = {"case": case, "calls": len(commands), "wall_ms": round(elapsed, 1),
           "compact_json_bytes": len(encoded), "commands": commands}
    rows.append(row)
    print(json.dumps(row), flush=True)
(out / "cli-summary.json").write_text(json.dumps(rows, indent=2) + "\n")
