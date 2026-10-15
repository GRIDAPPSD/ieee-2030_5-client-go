#!/usr/bin/env bats
bats_require_minimum_version 1.5.0

# Tests for sim/derms/regenerate.sh. GridLAB-D is a stub on PATH, so these
# need no installation. Run with TMPDIR pointing at a scratch directory,
# because bats and regenerate.sh both root their temp directories there.

SCRIPT="$(cd "$BATS_TEST_DIRNAME/.." && pwd -P)/regenerate.sh"

# stub_gridlabd writes an executable that prints $1 for --version and
# otherwise writes empty recorder CSVs, as a run that produced no data.
stub_gridlabd() { # version-line
  cat >"$BATS_TEST_TMPDIR/gridlabd" <<STUB
#!/usr/bin/env bash
if [[ "\${1:-}" == "--version" ]]; then
  echo "$1"
  exit 0
fi
echo "stub gridlabd ran" >>"$BATS_TEST_TMPDIR/ran"
exit 1
STUB
  chmod +x "$BATS_TEST_TMPDIR/gridlabd"
  export GRIDLABD="$BATS_TEST_TMPDIR/gridlabd"
}

@test "regenerate.sh refuses a GridLAB-D of the wrong version" {
  stub_gridlabd "GridLAB-D 5.2.0"
  run -1 "$SCRIPT"
  [[ "$output" == *"want GridLAB-D 5.3.0, found: GridLAB-D 5.2.0"* ]]
  [ ! -e "$BATS_TEST_TMPDIR/ran" ]
}

@test "regenerate.sh refuses a GridLAB-D that prints no version" {
  stub_gridlabd "unrelated output"
  run -1 "$SCRIPT"
  [[ "$output" == *"found: (no version printed)"* ]]
  [ ! -e "$BATS_TEST_TMPDIR/ran" ]
}

@test "regenerate.sh accepts 5.3.0 and goes on to run GridLAB-D" {
  stub_gridlabd "GridLAB-D 5.3.0 (stub)"
  run -1 "$SCRIPT"
  [ -e "$BATS_TEST_TMPDIR/ran" ]
  [[ "$output" == *"regenerate.sh: gridlabd failed"* ]]
}
