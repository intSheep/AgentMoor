"""Summarize sales.csv into a new report file. Amounts are integer cents."""

import argparse
import csv
import json
import os
from pathlib import Path
import re
import sys
import tempfile


def summarize(source):
    totals = {}
    with source.open(encoding="utf-8", newline="") as stream:
        rows = csv.reader(stream, strict=True)
        if next(rows, None) != ["region", "amount_cents"]:
            raise ValueError("expected region,amount_cents header")
        for row in rows:
            if (len(row) != 2 or not re.fullmatch(r"[A-Za-z0-9_.-]{1,128}", row[0])
                    or not re.fullmatch(r"-?[0-9]+", row[1])):
                raise ValueError(f"invalid sale on line {rows.line_num}")
            region, amount = row
            totals[region] = totals.get(region, 0) + int(amount)
    if not 1 <= len(totals) <= 100 or any(not -(2**63) <= v < 2**63 for v in totals.values()):
        raise ValueError("expected 1 to 100 regions with signed 64-bit totals")
    return {"totals_cents": totals}


def write_report(source, destination):
    report = summarize(source)
    with tempfile.NamedTemporaryFile(dir=destination.parent) as staged:
        staged.write((json.dumps(report, sort_keys=True, separators=(",", ":")) + "\n").encode("ascii"))
        staged.flush()
        os.link(staged.name, destination)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("source", type=Path)
    parser.add_argument("destination", type=Path)
    args = parser.parse_args()
    try:
        write_report(args.source, args.destination)
    except (OSError, ValueError, csv.Error) as error:
        sys.exit(f"report: {error}")
