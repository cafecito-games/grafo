#!/usr/bin/env python3
"""Replay fixed tool requests for timing; repetitions are not agent trials."""
import argparse
import json
import os
import pathlib
import statistics
import subprocess


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True)
    parser.add_argument("--repository", required=True)
    parser.add_argument("--index")
    parser.add_argument("--repeats", type=int, default=3)
    parser.add_argument("--output", type=pathlib.Path, required=True)
    parser.add_argument("transcripts", type=pathlib.Path, nargs="+")
    args = parser.parse_args()
    if args.repeats < 1:
        parser.error("repeats must be positive")
    records = [json.loads(line) for path in args.transcripts for line in path.read_text().splitlines()]
    requests = [record for record in records if record.get("request", {}).get("name") and not record.get("wrapper_error", False)]
    env = os.environ.copy()
    env.pop("GRAFO_EVALUATION_PREPARE", None)
    env.pop("GRAFO_EVALUATION_INDEX", None)
    if args.index:
        env["GRAFO_EVALUATION_INDEX"] = args.index
    results = []
    for repeat in range(args.repeats):
        payload = "".join(json.dumps(record["request"]) + "\n" for record in requests)
        completed = subprocess.run([args.binary, args.repository], input=payload, text=True,
            capture_output=True, check=True, timeout=max(120, len(requests) * 30), env=env)
        responses = [json.loads(line) for line in completed.stdout.splitlines()]
        if len(responses) != len(requests):
            raise RuntimeError("response count differs from request count")
        calls = [dict(case=request["case"], name=request["request"]["name"],
                      milliseconds=response["milliseconds"], error=response.get("error"),
                      is_error=(response.get("response") or {}).get("isError", False))
                 for request, response in zip(requests, responses)]
        results.append(dict(repeat=repeat, total_handler_ms=sum(call["milliseconds"] for call in calls), calls=calls))
    output = dict(binary=args.binary, index=args.index, runs=results,
                  median_total_handler_ms=statistics.median(row["total_handler_ms"] for row in results))
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(output, indent=2) + "\n")
    print(json.dumps({key: value for key, value in output.items() if key != "runs"}, indent=2))


if __name__ == "__main__":
    main()
