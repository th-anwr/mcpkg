#!/usr/bin/env python3
"""Example script hook (enable it in pipeline.json).

Runs after the "health" stage. Reads one JSON object on stdin:
  {"stage": "health", "decision": "continue", "tool": {...}, "result": {...health...}}
Prints one JSON object on stdout:
  {"decision": "continue" | "skip_heavy" | "stop", "notes": [...], "review": "", "data": {...}}

Hooks can only make a decision stricter. Any language works: the contract is stdin/stdout JSON.
"""
import json
import sys

PERMISSIVE = {"MIT", "Apache-2.0", "BSD-2-Clause", "BSD-3-Clause", "ISC", "MPL-2.0", "0BSD", "Unlicense"}

payload = json.load(sys.stdin)
health = payload.get("result") or {}
license_id = (health.get("license") or "").strip()

out = {"decision": "continue", "notes": [], "data": {"license": license_id}}
if not license_id:
    out["notes"].append("no license detected: users may not be allowed to use it")
    out["review"] = "missing-license"
elif license_id not in PERMISSIVE:
    out["notes"].append(f"non-permissive license {license_id}: check terms before commercial use")

print(json.dumps(out))
