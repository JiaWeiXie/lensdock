#!/bin/bash
# Process snapshot of the runtime container. Streamed into the container on stdin
# (never interpolated with test data):
#
#   docker exec -i lensdock-runtime /bin/bash -s < tests/e2e/internal/harness/procsnap.sh
#
# Output, one block per process, parsed by harness.ParseProcSnapshot:
#   === <pid>
#   cmdline: <argv joined by spaces>
#   <selected /proc/<pid>/status lines>
set -u
for d in /proc/[0-9]*; do
  pid=${d#/proc/}
  [ -r "$d/status" ] || continue
  printf '=== %s\n' "$pid"
  if [ -r "$d/cmdline" ]; then
    printf 'cmdline: '
    tr '\0' ' ' <"$d/cmdline"
    printf '\n'
  fi
  grep -E '^(Name|State|PPid|Uid|Gid|Groups|NoNewPrivs|Seccomp|Seccomp_filters|CapInh|CapPrm|CapEff|CapBnd|CapAmb|NSpid|NSpgid):' "$d/status" 2>/dev/null
done
