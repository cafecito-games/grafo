#!/usr/bin/env python3
"""Aggregate MCP text-output costs; does not score correctness or model tokens."""
import argparse
import collections
import json
import pathlib
import tiktoken


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("transcripts", type=pathlib.Path, nargs="+")
    args = parser.parse_args()
    encoder = tiktoken.get_encoding("cl100k_base")
    cases = collections.defaultdict(lambda: dict(attempted_calls=0, valid_calls=0,
        response_bytes=0, response_proxy_tokens=0, handler_ms=0.0, wrapper_wall_ms=0.0))
    for path in args.transcripts:
        for line in path.read_text().splitlines():
            record = json.loads(line)
            row = cases[record["case"]]
            row["attempted_calls"] += 1
            # Empty-name mistakes in the original frozen adapter predate wrapper_error.
            valid = bool(record.get("request", {}).get("name")) and not record.get("wrapper_error", False)
            row["valid_calls"] += int(valid)
            row["handler_ms"] += record.get("milliseconds", 0)
            row["wrapper_wall_ms"] += record.get("wall_ms", 0)
            for content in (record.get("response") or {}).get("content", []):
                if content["type"] != "text":
                    continue
                text = content["text"]
                row["response_bytes"] += len(text.encode("utf-8"))
                row["response_proxy_tokens"] += len(encoder.encode(text, disallowed_special=()))
    totals = {key: sum(row[key] for row in cases.values()) for key in next(iter(cases.values()), {})}
    print(json.dumps(dict(cases=dict(cases), totals=totals, encoding="cl100k_base",
        excluded="Prompts, reasoning, billed model tokens, orchestration wrappers, duplicated structuredContent; final answers scored separately"), indent=2))


if __name__ == "__main__":
    main()
