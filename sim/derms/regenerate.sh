#!/usr/bin/env bash
# Regenerates sim/derms/recordings/*.csv from derms.glm with the installed
# GridLAB-D and notes the version it ran under. Usage: regenerate.sh
set -euo pipefail
export LC_ALL=C

here=$(cd "$(dirname "$0")" && pwd)
gld=${GRIDLABD:-gridlabd}

version=$("$gld" --version 2>&1 | grep -m1 "^GridLAB-D") || version="(no version printed)"
case "$version" in
  "GridLAB-D 5.3.0"*) ;;
  *) echo "regenerate.sh: want GridLAB-D 5.3.0, found: $version" >&2; exit 1 ;;
esac

work=$(mktemp -d)
trap 'rm -rf "${work:?}"' EXIT

# One row per minute of the day. PV is a half-sine from 06:00 to 18:00
# peaking at the given watts. The battery charges 2 kW from 10:00 to 13:00
# and discharges 2 kW from 18:00 to 21:00 (negative P_Out charges).
pv_player() {
  awk -v peak="$1" 'BEGIN {
    for (m = 0; m < 1440; m++) {
      p = 0
      if (m >= 360 && m <= 1080) p = int(peak * sin(3.14159265358979 * (m - 360) / 720) + 0.5)
      printf "2020-01-01 %02d:%02d:00,%d\n", m / 60, m % 60, p
    }
  }'
}
bat_player() {
  awk 'BEGIN {
    for (m = 0; m < 1440; m++) {
      p = 0
      if (m >= 600 && m < 780) p = -2000
      else if (m >= 1080 && m < 1260) p = 2000
      printf "2020-01-01 %02d:%02d:00,%d\n", m / 60, m % 60, p
    }
  }'
}

cp "$here/derms.glm" "$work/derms.glm"
pv_player 4000 >"$work/agg-pv-1.player"
pv_player 3000 >"$work/solo-pv-1.player"
bat_player >"$work/agg-bat-1.player"

(cd "$work" && "$gld" derms.glm >gridlabd.log 2>&1) || {
  cat "$work/gridlabd.log" >&2
  echo "regenerate.sh: gridlabd failed" >&2
  exit 1
}

# The recorder's date, user and host header lines change on every run;
# dropping them makes a rerun byte-identical.
mkdir -p "$here/recordings"
for name in agg-bat-1 agg-pv-1 solo-pv-1; do
  grep -v -e '^# date' -e '^# user' -e '^# host' "$work/$name.csv" >"$here/recordings/$name.csv"
done
echo "$version" >"$here/recordings/GRIDLABD_VERSION"
echo "wrote $here/recordings"
