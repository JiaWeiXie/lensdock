#!/usr/bin/env python3
"""Stdlib-only helpers for the e2e shell scripts (the Linux test host has no jq).

Usage: e2e_tool.py <command> [args]
  json-get FILE PATH [DEFAULT]       print a value; PATH is dotted, integers index lists
  timeout SECONDS CMD [ARG...]       run CMD, kill it after SECONDS (exit 124), pass its exit code
  cpu-delta BEFORE AFTER T0 T1 CLK [CG0_USEC CG1_USEC]
                                     per-unit CPU as % of one vCPU between two readyz?detail=1 samples
  vnc-summary ARCH S0.json S1.json S2.json S3.json
                                     apply the pre-registered SPIKE-02 rule, print the verdict JSON
  matrix-cell DIR NAME EXPECT RT02_RC  summarise one sandbox-matrix cell directory as JSON
  matrix-merge CELL.json...          merge cell summaries into matrix.json on stdout
  strip-token FILE                   print the JSON document of FILE without any capabilityToken member
"""
import json
import os
import subprocess
import sys

VNC_RULE_THRESHOLD_PERCENT = 10.0  # 5901 marginal CPU in S2, % of one vCPU, amd64 only


def load(path):
    with open(path, encoding="utf-8") as f:
        return json.load(f)


def dig(obj, path):
    for part in path.split("."):
        if part == "":
            continue
        if isinstance(obj, list):
            obj = obj[int(part)]
        else:
            obj = obj[part]
    return obj


def cmd_json_get(args):
    path, key = args[0], args[1]
    try:
        value = dig(load(path), key)
    except (KeyError, IndexError, ValueError, TypeError, OSError, json.JSONDecodeError):
        if len(args) > 2:
            print(args[2])
            return 0
        return 1
    print(value if isinstance(value, str) else json.dumps(value))
    return 0


def cmd_timeout(args):
    seconds = float(args[0])
    try:
        return subprocess.run(args[1:], timeout=seconds, check=False).returncode
    except subprocess.TimeoutExpired:
        print("e2e_tool: command timed out after %ss" % args[0], file=sys.stderr)
        return 124


def units(path):
    detail = load(path)
    return {u["name"]: u for u in detail["detail"]["units"]}


def cmd_cpu_delta(args):
    before, after = units(args[0]), units(args[1])
    t0, t1, clk = float(args[2]), float(args[3]), float(args[4])
    seconds = t1 - t0
    out = {"seconds": round(seconds, 3), "clkTck": clk, "units": {}}
    for name, a in after.items():
        b = before.get(name)
        if b is None or a["pid"] != b["pid"]:
            out["units"][name] = {"note": "restarted or new during the window; not comparable"}
            continue
        ticks = a["cpuTicks"] - b["cpuTicks"]
        out["units"][name] = {"cpuTicksDelta": ticks, "percentOfOneVcpu": round(100.0 * ticks / clk / seconds, 2)}
    if len(args) >= 7:
        usec = float(args[6]) - float(args[5])
        out["containerPercentOfOneVcpu"] = round(100.0 * usec / 1e6 / seconds, 2)
    print(json.dumps(out, indent=2))
    return 0


def pct(scenario, unit):
    return scenario["units"].get(unit, {}).get("percentOfOneVcpu")


def cmd_vnc_summary(args):
    arch = args[0]
    s0, s1, s2, s3 = (load(p) for p in args[1:5])
    view0, view2 = pct(s0, "vnc-view"), pct(s2, "vnc-view")
    marginal = None if view0 is None or view2 is None else round(view2 - view0, 2)
    exceeds = marginal is not None and marginal > VNC_RULE_THRESHOLD_PERCENT
    if marginal is None:
        verdict = "INCONCLUSIVE: vnc-view unit CPU missing from a sample"
    elif arch != "amd64":
        verdict = "NON-DECISIVE on %s (rule applies to amd64 only); marginal %.2f%%" % (arch, marginal)
    elif exceeds:
        verdict = "RECOMMEND single x11vnc with -viewpasswd (5901 marginal %.2f%% > %.0f%% of one vCPU)" % (marginal, VNC_RULE_THRESHOLD_PERCENT)
    else:
        verdict = "KEEP two x11vnc instances (5901 marginal %.2f%% <= %.0f%% of one vCPU)" % (marginal, VNC_RULE_THRESHOLD_PERCENT)
    summary = {
        "rule": "amd64 only: vnc-view CPU in S2 minus S0 > %.0f%% of one vCPU => single x11vnc -viewpasswd" % VNC_RULE_THRESHOLD_PERCENT,
        "arch": arch,
        "vncViewPercent": {"S0": view0, "S1": pct(s1, "vnc-view"), "S2": view2, "S3": pct(s3, "vnc-view")},
        "vncControlPercent": {"S0": pct(s0, "vnc-control"), "S1": pct(s1, "vnc-control"), "S2": pct(s2, "vnc-control"), "S3": pct(s3, "vnc-control")},
        "marginal5901InS2": marginal,
        "ruleTriggered": bool(exceeds and arch == "amd64"),
        "verdict": verdict,
    }
    print(json.dumps(summary, indent=2))
    return 0


def read_text(path):
    try:
        with open(path, encoding="utf-8", errors="replace") as f:
            return f.read()
    except OSError:
        return ""


def cmd_matrix_cell(args):
    d, name, expect, rc = args[0], args[1], args[2], int(args[3])
    detail_text = read_text(os.path.join(d, "readyz-detail.json"))
    try:
        detail = json.loads(detail_text)
    except json.JSONDecodeError:
        detail = {}
    procs = read_text(os.path.join(d, "procs.txt"))
    chromium_procs = [l for l in procs.splitlines() if l.startswith("cmdline: /usr/lib/chromium/")]
    ready = bool(detail.get("ready"))
    seen_in = [f for f in ("readyz-detail.json", "acquire.json", "workspace.json", "container.log")
               if "SANDBOX_PREFLIGHT_FAILED" in read_text(os.path.join(d, f))]
    sandbox = detail.get("detail", {}).get("sandbox", {})
    cell = {
        "name": name,
        "expect": expect,
        "ready": ready,
        "readyzStatus": read_text(os.path.join(d, "readyz.status")).strip(),
        "lockCode": detail.get("lockCode", ""),
        "sandboxPreflightFailedSeenIn": seen_in,
        "chromiumProcessCount": len(chromium_procs),
        "noSandboxFlagSeen": "--no-sandbox" in procs,
        "supervisorSandboxVerified": sandbox.get("verified"),
        "rt02ExitCode": rc if rc >= 0 else None,
    }
    if expect == "ready":
        ok = ready and rc == 0 and not cell["noSandboxFlagSeen"]
    else:  # preflight-failed
        ok = (not ready) and bool(seen_in) and cell["chromiumProcessCount"] == 0 and not cell["noSandboxFlagSeen"]
    cell["verdict"] = "as expected" if ok else "UNEXPECTED"
    print(json.dumps(cell, indent=2))
    return 0


def cmd_matrix_merge(args):
    cells = [load(p) for p in args]
    print(json.dumps({"cells": cells, "allAsExpected": all(c["verdict"] != "UNEXPECTED" for c in cells)}, indent=2))
    return 0


def strip_token(obj):
    """Drop every capabilityToken member, at any depth."""
    if isinstance(obj, dict):
        return {k: strip_token(v) for k, v in obj.items() if k != "capabilityToken"}
    if isinstance(obj, list):
        return [strip_token(v) for v in obj]
    return obj


def cmd_strip_token(args):
    # A body that is not JSON is not echoed: it could carry anything, including a token.
    try:
        doc = strip_token(load(args[0]))
    except (OSError, ValueError):
        doc = {"unparsableResponseBody": True}
    print(json.dumps(doc, indent=2))
    return 0


COMMANDS = {
    "json-get": (cmd_json_get, 2),
    "timeout": (cmd_timeout, 2),
    "cpu-delta": (cmd_cpu_delta, 5),
    "vnc-summary": (cmd_vnc_summary, 5),
    "matrix-cell": (cmd_matrix_cell, 4),
    "matrix-merge": (cmd_matrix_merge, 1),
    "strip-token": (cmd_strip_token, 1),
}


def main(argv):
    if len(argv) < 2 or argv[1] not in COMMANDS:
        print(__doc__, file=sys.stderr)
        return 2
    fn, min_args = COMMANDS[argv[1]]
    if len(argv) - 2 < min_args:
        print(__doc__, file=sys.stderr)
        return 2
    return fn(argv[2:])


if __name__ == "__main__":
    sys.exit(main(sys.argv))
