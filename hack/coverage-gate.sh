#!/usr/bin/env bash
# Fails if a package's statement coverage in a Go cover profile is below its
# gate (spec §6.2). Usage: hack/coverage-gate.sh [cover.out]
set -euo pipefail
profile="${1:-cover.out}"
mod="github.com/nashant/upnp-nat-controller"
declare -A min=(
  ["$mod/internal/annotations"]=90
  ["$mod/internal/mapping"]=90
  ["$mod/internal/upnp"]=90
  ["$mod/internal/controller"]=75
)
declare -A seen=()
fail=0
while read -r pkg pct; do
  [[ -n "${min[$pkg]:-}" ]] || continue
  seen[$pkg]=1
  printf '%-30s %6.1f%%  (min %s%%)\n' "${pkg#"$mod"/}" "$pct" "${min[$pkg]}"
  if awk -v p="$pct" -v m="${min[$pkg]}" 'BEGIN { exit !(p + 0 < m + 0) }'; then
    echo "::error::${pkg#"$mod"/} coverage ${pct}% is below ${min[$pkg]}%"
    fail=1
  fi
done < <(awk -F'[: ,]' 'NR > 1 { f = $1; sub(/\/[^\/]*$/, "", f); n[f] += $(NF-1); if ($NF > 0) c[f] += $(NF-1) }
  END { for (p in n) printf "%s %.1f\n", p, 100 * c[p] / n[p] }' "$profile")
for pkg in "${!min[@]}"; do
  if [[ -z "${seen[$pkg]:-}" ]]; then
    echo "::error::${pkg#"$mod"/} has no coverage data (did its tests run?)"
    fail=1
  fi
done
exit "$fail"
