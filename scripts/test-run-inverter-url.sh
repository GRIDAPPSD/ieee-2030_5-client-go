#!/usr/bin/env bash
# IEEE-026: verify `make run-inverter` honors a SERVER_URL env override.
#
# Uses `make -n` to get the recipe expansion without actually building or
# starting the simulator. Greps the expansion for the override value.
#
# Exit status: 0 on pass, non-zero on fail.

set -euo pipefail

EXPECT_URL="https://example:9443"
DRY_RUN=$(SERVER_URL="${EXPECT_URL}" make -n run-inverter 2>/dev/null)

if ! grep -Fq -- "--server ${EXPECT_URL}" <<<"${DRY_RUN}"; then
  echo "FAIL: SERVER_URL override not honored by 'make run-inverter'"
  echo "      Expected '--server ${EXPECT_URL}' in dry-run output."
  echo "      Got:"
  printf '%s\n' "${DRY_RUN}" | sed 's/^/        /'
  exit 1
fi

DEFAULT_RUN=$(make -n run-inverter 2>/dev/null)
if ! grep -Fq -- "--server https://localhost:8443" <<<"${DEFAULT_RUN}"; then
  echo "FAIL: default SERVER_URL does not resolve to https://localhost:8443"
  echo "      Got:"
  printf '%s\n' "${DEFAULT_RUN}" | sed 's/^/        /'
  exit 1
fi

echo "PASS: run-inverter honors SERVER_URL override and default is https://localhost:8443"
