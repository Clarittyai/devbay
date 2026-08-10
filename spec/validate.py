#!/usr/bin/env python3
"""Reference validator for devbay.yaml — M−1 only.

This exists to make the M−1 spec gate falsifiable before any Go is written.
It checks the JSON Schema plus the semantic rules a schema cannot express.
It is throwaway: internal/manifest is the real implementation.

Usage: validate.py <manifest.yaml> [...]
"""
import re
import sys
import json
import pathlib

import yaml
from jsonschema import Draft202012Validator

SCHEMA = json.loads((pathlib.Path(__file__).parent / "devbay.schema.json").read_text())
ALLOWLIST = set(SCHEMA["$defs"]["argv0_allowlist"]["default"])
REF = re.compile(r"\$\{bay\.([a-z0-9-]+)\.(url|public_url|host|port|name|user|password|ports\.[a-z0-9-]+)\}")

# Entropy heuristic for R3, which a schema cannot express.
def looks_secret(v: str) -> bool:
    if "${" in v or len(v) < 24:
        return False
    charset = len(set(v))
    return charset >= 16 and not any(c in v for c in " /:\n")


def check(path: str) -> list[str]:
    doc = yaml.safe_load(open(path))
    errs, warns = [], []

    for e in sorted(Draft202012Validator(SCHEMA).iter_errors(doc), key=lambda e: list(e.path)):
        errs.append(f"schema: {'/'.join(map(str, e.path)) or '<root>'}: {e.message}")

    services = doc.get("services", {}) or {}
    tasks = doc.get("tasks", {}) or {}

    # R2 — argv[0] outside the allowlist is permitted but needs approval.
    for owner, argv in [
        *((f"services/{n}/{k}", s[k]) for n, s in services.items() for k in ("install", "start", "run") if k in s),
        *((f"services/{n}/health.cmd", s["health"]["cmd"]) for n, s in services.items() if "cmd" in s.get("health", {})),
        *((f"tasks/{n}/run", t["run"]) for n, t in tasks.items()),
    ]:
        if argv and argv[0] not in ALLOWLIST:
            warns.append(f"R2 approval required: {owner}: argv[0]={argv[0]!r} -> {' '.join(argv)}")

    # R3 — entropy heuristic beyond the schema's prefix screen.
    for n, s in services.items():
        for k, v in (s.get("env") or {}).items():
            if isinstance(v, str) and looks_secret(v):
                errs.append(f"R3: services/{n}/env/{k}: looks like a literal credential; use ${{secret:...}}")

    # needs must reference declared services and form a DAG.
    for n, s in services.items():
        for dep in s.get("needs", []):
            if dep not in services:
                errs.append(f"services/{n}/needs: unknown service {dep!r}")
    for n, t in tasks.items():
        for dep in t.get("needs", []):
            if dep not in services:
                errs.append(f"tasks/{n}/needs: unknown service {dep!r}")
        if (i := t.get("in")) and i not in services:
            errs.append(f"tasks/{n}/in: unknown service {i!r}")

    seen, stack = set(), set()
    def visit(n, chain):
        if n in stack:
            errs.append(f"needs: dependency cycle: {' -> '.join(chain + [n])}")
            return
        if n in seen:
            return
        stack.add(n); seen.add(n)
        for d in services.get(n, {}).get("needs", []):
            if d in services:
                visit(d, chain + [n])
        stack.discard(n)
    for n in services:
        visit(n, [])

    # R7 — every interpolated reference must resolve.
    for n, s in services.items():
        for k, v in (s.get("env") or {}).items():
            for svc, field in REF.findall(v if isinstance(v, str) else ""):
                if svc not in services:
                    errs.append(f"R7: services/{n}/env/{k}: references unknown service {svc!r}")
                elif field.startswith("ports."):
                    pn = field.split(".", 1)[1]
                    if pn not in (services[svc].get("ports") or {}):
                        errs.append(f"R7: services/{n}/env/{k}: {svc!r} has no named port {pn!r}")
                elif field in ("url", "port", "public_url") and not services[svc].get("port"):
                    errs.append(f"R7: services/{n}/env/{k}: {svc!r} declares no port, so .{field} is unresolvable")

    # seed.after must name oneshot services.
    for n, s in services.items():
        for step in (s.get("seed") or {}).get("after", []):
            if step not in services:
                errs.append(f"services/{n}/seed/after: unknown service {step!r}")
            elif services[step].get("kind") != "oneshot":
                errs.append(f"services/{n}/seed/after: {step!r} must be kind: oneshot")

    # Exactly one primary.
    longrunning = {n: s for n, s in services.items() if s.get("kind", "service") == "service"}
    primaries = [n for n, s in longrunning.items() if s.get("primary")]
    ported = [n for n, s in longrunning.items() if s.get("port")]
    if len(primaries) > 1:
        errs.append(f"primary: {len(primaries)} services claim primary: {primaries}")
    elif not primaries and len(ported) != 1:
        errs.append(f"primary: {len(ported)} services expose a port, so one must set primary: true")

    # R5 quality — flag the weakest probe.
    for n, s in longrunning.items():
        if s.get("health", {}).get("process"):
            warns.append(f"R5 weak probe: services/{n} uses liveness-only `process`; prefer `log`")

    for w in warns:
        print(f"  warn  {w}")
    return errs


if __name__ == "__main__":
    failed = 0
    for p in sys.argv[1:]:
        print(f"\n{p}")
        errs = check(p)
        for e in errs:
            print(f"  ERROR {e}")
        print("  " + ("FAIL" if errs else "OK"))
        failed += bool(errs)
    sys.exit(1 if failed else 0)
