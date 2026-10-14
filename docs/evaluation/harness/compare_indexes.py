#!/usr/bin/env python3
"""Read-only equality digests for original graph evidence across schema migration."""
import argparse
import hashlib
import json
import pathlib
import sqlite3
import time
import sys


def quote(identifier):
    return '"' + identifier.replace('"', '""') + '"'


def digest(connection, table):
    columns = [row[1] for row in connection.execute("PRAGMA table_info(" + quote(table) + ")")]
    query = "SELECT * FROM " + quote(table)
    if table == "meta":
        query += " WHERE key != 'lexical_version'"
    query += " ORDER BY " + ",".join(quote(column) for column in columns)
    result = hashlib.sha256()
    count = 0
    for row in connection.execute(query):
        # Tagged blob encoding preserves distinction from ordinary text.
        encoded = [dict(blob_hex=value.hex()) if isinstance(value, bytes) else value for value in row]
        result.update((json.dumps(encoded, ensure_ascii=True, separators=(",", ":")) + "\n").encode())
        count += 1
    return dict(columns=columns, rows=count, sha256=result.hexdigest())


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("baseline", type=pathlib.Path)
    parser.add_argument("candidate", type=pathlib.Path)
    parser.add_argument("--output", type=pathlib.Path, required=True)
    args = parser.parse_args()
    started = time.monotonic()
    connections = [sqlite3.connect(path.resolve().as_uri() + "?mode=ro", uri=True) for path in [args.baseline, args.candidate]]
    try:
        tables = [row[0] for row in connections[0].execute("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name != 'goose_db_version' ORDER BY name")]
        versions = [c.execute("SELECT MAX(version_id) FROM goose_db_version WHERE is_applied=1").fetchone()[0] for c in connections]
        comparisons = {}
        for table in tables:
            values = [digest(c, table) for c in connections]
            comparisons[table] = dict(equal=values[0] == values[1], baseline=values[0], candidate=values[1])
            print(table + ": " + ("equal" if values[0] == values[1] else "DIFFERENT"), file=sys.stderr, flush=True)
        result = dict(schema_versions=versions, equal=all(v["equal"] for v in comparisons.values()),
                      excluded="goose migration bookkeeping; candidate-only derived tables; meta lexical_version", tables=comparisons,
                      wall_seconds=time.monotonic()-started)
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(json.dumps(result, indent=2) + "\n")
        print(json.dumps({key: value for key, value in result.items() if key != "tables"}, indent=2))
        if not result["equal"]:
            raise SystemExit(1)
    finally:
        for c in connections:
            c.close()


if __name__ == "__main__":
    main()
