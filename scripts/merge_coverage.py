#!/usr/bin/env python3
"""Merge multiple Go coverage profiles (same mode) into one combined profile."""
import sys

if len(sys.argv) < 3:
    print(f"Usage: {sys.argv[0]} <output.out> <in1.out> [in2.out ...]", file=sys.stderr)
    sys.exit(2)

out_path = sys.argv[1]
inputs = sys.argv[2:]

mode_line = None
counts = {}

for path in inputs:
    with open(path, "r", encoding="utf-8") as f:
        for idx, raw in enumerate(f):
            line = raw.strip()
            if not line:
                continue
            if idx == 0 and line.startswith("mode:"):
                if mode_line is None:
                    mode_line = line
                continue
            parts = line.rsplit(" ", 2)
            if len(parts) != 3:
                continue
            block, stmts, cnt = parts[0], parts[1], int(parts[2])
            key = (block, stmts)
            counts[key] = counts.get(key, 0) + cnt

if mode_line is None:
    print("ERROR: no coverage data found", file=sys.stderr)
    sys.exit(1)

with open(out_path, "w", encoding="utf-8") as out:
    out.write(mode_line + "\n")
    for (block, stmts) in sorted(counts.keys()):
        out.write(f"{block} {stmts} {counts[(block, stmts)]}\n")
