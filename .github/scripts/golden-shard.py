#!/usr/bin/env python3
import argparse
from pathlib import Path
import re

parser = argparse.ArgumentParser(description="Select a disjoint slice of the golden corpus for go test -run.")
parser.add_argument("index", type=int, help="one-based shard index")
parser.add_argument("count", type=int, help="number of shards")
args = parser.parse_args()
if not 1 <= args.index <= args.count:
    parser.error("shard index must be between 1 and count")

corpus = sorted((Path(__file__).resolve().parents[2] / "test/e2e/golden").glob("*.yml"))
selected = corpus[args.index - 1 :: args.count]
if not selected:
    parser.error("shard has no golden playbooks")
print("^(" + "|".join(re.escape(path.name) for path in selected) + ")$")
