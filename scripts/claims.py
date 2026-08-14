#!/usr/bin/env python3
"""Check the numbers devbay publishes about itself against the thing they count.

A project whose pitch is "measured, not asserted" cannot be the one with a stale
figure on its front page. The test count on the README and the site was wrong
twice in two days, both times because it is a number a person has to remember to
change -- and it is quoted in two files that spell it differently, so neither
one reminds you about the other.

Run by `make claims-check`, which `make check` runs, so a number goes out of
date in CI rather than in front of a reader.
"""

import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent


def suite_size():
    """What `go test ./... -list` reports: the count the README names as its source."""
    out = subprocess.run(
        ["go", "test", "./...", "-list", ".*"],
        cwd=ROOT, capture_output=True, text=True, check=False,
    ).stdout.splitlines()
    tests = sum(1 for line in out if re.match(r"^(Test|Example|Fuzz)", line))
    packages = sum(1 for line in out if line.startswith("ok "))
    return tests, packages


def claims():
    """Every place a test count is published, and what it currently says.

    Two spellings, because prose and a data file are not written the same way:

        README.md         323 tests across 19 packages
        site/lib/samples  figure: '323'
                          label: 'tests across 19 packages'
    """
    found = []

    readme = ROOT / "README.md"
    for m in re.finditer(r"(\d+) tests across (\d+) packages", readme.read_text()):
        found.append((readme.name, int(m.group(1)), int(m.group(2))))

    samples = ROOT / "site" / "lib" / "samples.ts"
    if samples.exists():
        text = samples.read_text()
        for m in re.finditer(
            r"figure:\s*'(\d+)',\s*\n\s*label:\s*'tests across (\d+) packages'", text
        ):
            found.append((samples.name, int(m.group(1)), int(m.group(2))))

    return found


def main():
    tests, packages = suite_size()
    if tests == 0:
        print("could not count the suite; is the Go toolchain available?", file=sys.stderr)
        return 1

    found = claims()
    if not found:
        # Not an error: the number may have been removed deliberately. Silence
        # here would be worse than a line saying so.
        print(f"no published test count to check (the suite has {tests})")
        return 0

    stale = [(f, t, p) for f, t, p in found if (t, p) != (tests, packages)]
    for f, t, p in stale:
        print(f"{f} says {t} tests across {p} packages; the suite is {tests} across {packages}")
    if stale:
        print("update the figure, or stop publishing a precise one")
        return 1

    print(f"claims match: {tests} tests across {packages} packages, in {len(found)} place(s)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
