#!/usr/bin/env python3
"""Record one real MCP call; source-bearing logs belong outside the repository."""
import argparse
import json
import os
import pathlib
import subprocess
import time


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True)
    parser.add_argument("--repository", required=True)
    parser.add_argument("--index", help="Explicit read-only index override")
    parser.add_argument("--transcript", type=pathlib.Path, required=True)
    parser.add_argument("--case", required=True)
    parser.add_argument("--request", required=True, help='JSON: {"name":"find_symbols","arguments":{"queries":["term"]}}')
    args = parser.parse_args()
    request = json.loads(args.request)
    if not isinstance(request, dict) or not request.get("name"):
        parser.error("request must be an object with a nonempty name")
    env = os.environ.copy()
    env.pop("GRAFO_EVALUATION_PREPARE", None)
    env.pop("GRAFO_EVALUATION_INDEX", None)
    if args.index:
        env["GRAFO_EVALUATION_INDEX"] = args.index
    started = time.monotonic()
    result = subprocess.run([args.binary, args.repository], input=json.dumps(request) + "\n",
                            text=True, capture_output=True, check=True, timeout=120, env=env)
    record = json.loads(result.stdout)
    record.update(request=request, case=args.case, binary=args.binary, index=args.index,
                  wall_ms=(time.monotonic() - started) * 1000)
    args.transcript.parent.mkdir(parents=True, exist_ok=True)
    with args.transcript.open("a") as stream:
        stream.write(json.dumps(record) + "\n")
    if record.get("error"):
        print(record["error"])
    for content in (record.get("response") or {}).get("content", []):
        if content["type"] == "text":
            print(content["text"])


if __name__ == "__main__":
    main()
