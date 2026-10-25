#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.11"
# dependencies = []
# ///
"""Generate docker/browser/seccomp/chromium.json from the pinned moby default profile.

The moby default profile only allows clone/unshare/setns/chroot-with-namespaces when the
container holds CAP_SYS_ADMIN (or CAP_SYS_CHROOT for chroot). The LensDock runtime container runs
with `cap_drop: ALL`, and Chromium's user-namespace sandbox creates namespaces from an unprivileged
process, so this script adds unconditional rules for exactly those syscalls. clone3 is left as in
moby (ENOSYS without CAP_SYS_ADMIN). Everything else is the unmodified moby profile.

Usage: uv run --script tools/gen_seccomp.py -o docker/browser/seccomp/chromium.json

Output is deterministic: same pinned input, same bytes. Bump MOBY_COMMIT/MOBY_SHA256 together.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import sys
import urllib.request
from pathlib import Path
from typing import Any

# moby/moby tag docker-v29.9.2 (peeled commit). The profile now lives in the vendored
# github.com/moby/profiles module inside the moby tree.
MOBY_COMMIT = "0367cbeb49b9d98cb0b4f51e95f7b54b4803e76c"
MOBY_PATH = "vendor/github.com/moby/profiles/seccomp/default.json"
MOBY_SHA256 = "785b2429264afba4d594320337cb17f144f3c7d51585f9805eef72e28f4f9334"
MOBY_URL = f"https://raw.githubusercontent.com/moby/moby/{MOBY_COMMIT}/{MOBY_PATH}"

FETCH_TIMEOUT_SECONDS = 30
MAX_PROFILE_BYTES = 1 << 20

# Namespace flags that Chromium does NOT use (CLONE_NEWNS|NEWCGROUP|NEWUTS|NEWIPC). The
# Chromium sandbox only needs CLONE_NEWUSER|CLONE_NEWPID|CLONE_NEWNET, so clone/unshare are
# allowed whenever none of these four are requested.
CLONE_NEWNS = 0x00020000
CLONE_NEWCGROUP = 0x02000000
CLONE_NEWUTS = 0x04000000
CLONE_NEWIPC = 0x08000000
UNUSED_NS_MASK = CLONE_NEWNS | CLONE_NEWCGROUP | CLONE_NEWUTS | CLONE_NEWIPC
S390_ARCHES = ["s390", "s390x"]


class ProfileError(Exception):
    """The upstream profile does not have the shape this generator was written against."""


def fetch(url: str) -> bytes:
    request = urllib.request.Request(url, headers={"User-Agent": "lensdock-gen-seccomp"})
    with urllib.request.urlopen(request, timeout=FETCH_TIMEOUT_SECONDS) as response:
        data = response.read(MAX_PROFILE_BYTES + 1)
    if len(data) > MAX_PROFILE_BYTES:
        raise ProfileError(f"profile larger than {MAX_PROFILE_BYTES} bytes")
    return data


def verify(data: bytes) -> None:
    actual = hashlib.sha256(data).hexdigest()
    if actual != MOBY_SHA256:
        raise ProfileError(f"sha256 mismatch for {MOBY_URL}: expected {MOBY_SHA256}, got {actual}")


def is_rule(rule: dict[str, Any], name: str, action: str) -> bool:
    return rule.get("names") == [name] and rule.get("action") == action


def drop_single(syscalls: list[dict[str, Any]], name: str, action: str, expect: int) -> int:
    """Remove moby rules that are superseded by this script's rules, failing if the shape moved."""
    kept = [rule for rule in syscalls if not is_rule(rule, name, action)]
    removed = len(syscalls) - len(kept)
    if removed != expect:
        raise ProfileError(f"expected {expect} moby rule(s) for {name}/{action}, found {removed}")
    syscalls[:] = kept
    return removed


def masked_flags_rule(name: str, index: int, comment: str, **conditions: Any) -> dict[str, Any]:
    rule: dict[str, Any] = {
        "names": [name],
        "action": "SCMP_ACT_ALLOW",
        "args": [{"index": index, "value": UNUSED_NS_MASK, "op": "SCMP_CMP_MASKED_EQ"}],
    }
    rule.update(conditions)
    rule["comment"] = comment
    return rule


def build(profile: dict[str, Any]) -> dict[str, Any]:
    syscalls = profile.get("syscalls")
    if profile.get("defaultAction") != "SCMP_ACT_ERRNO" or not isinstance(syscalls, list):
        raise ProfileError("unexpected moby profile layout (defaultAction/syscalls)")

    # The two moby clone rules (generic and s390 argument order) are replaced by rules that also
    # admit CLONE_NEWUSER|CLONE_NEWPID|CLONE_NEWNET. moby's clone3 -> ERRNO(ENOSYS) rule is kept
    # on purpose: clone3 namespace flags sit in a user-space struct that seccomp cannot inspect,
    # so allowing clone3 would make the clone/unshare flag masks meaningless. glibc and Chromium
    # fall back to clone() on ENOSYS (verified against Debian trixie chromium 156).
    drop_single(syscalls, "clone", "SCMP_ACT_ALLOW", 2)

    syscalls.extend(
        [
            masked_flags_rule(
                "clone",
                0,
                "Chromium userns sandbox: CLONE_NEWUSER|CLONE_NEWPID|CLONE_NEWNET without CAP_SYS_ADMIN;"
                " CLONE_NEWNS/NEWUTS/NEWIPC/NEWCGROUP stay denied",
                excludes={"arches": S390_ARCHES},
            ),
            masked_flags_rule(
                "clone",
                1,
                "s390 passes flags as the second clone argument; same policy as above",
                includes={"arches": S390_ARCHES},
            ),
            masked_flags_rule(
                "unshare",
                0,
                "Chromium userns sandbox: unshare(CLONE_NEWUSER|CLONE_NEWPID|CLONE_NEWNET|CLONE_FS)"
                " without CAP_SYS_ADMIN; CLONE_NEWNS/NEWUTS/NEWIPC/NEWCGROUP stay denied",
            ),
            {
                "names": ["setns"],
                "action": "SCMP_ACT_ALLOW",
                "comment": "Join a namespace; the kernel still requires privilege in the target namespace",
            },
            {
                "names": ["chroot"],
                "action": "SCMP_ACT_ALLOW",
                "comment": "Chromium userns sandbox chroots into an empty directory inside its new user namespace;"
                " the kernel requires CAP_SYS_CHROOT in that namespace, not in the container",
            },
        ]
    )

    return {
        "comment": (
            f"Generated by tools/gen_seccomp.py from moby/moby {MOBY_COMMIT} {MOBY_PATH} "
            f"(sha256 {MOBY_SHA256}). Do not edit; rerun the generator."
        ),
        **profile,
    }


def render(profile: dict[str, Any]) -> str:
    return json.dumps(profile, indent=2, ensure_ascii=False) + "\n"


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("-o", "--output", required=True, type=Path, help="output path, or - for stdout")
    args = parser.parse_args()

    try:
        data = fetch(MOBY_URL)
        verify(data)
        text = render(build(json.loads(data)))
    except (ProfileError, OSError, ValueError) as err:
        print(f"gen_seccomp: {err}", file=sys.stderr)
        return 1

    if str(args.output) == "-":
        sys.stdout.write(text)
        return 0
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(text, encoding="utf-8", newline="\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
