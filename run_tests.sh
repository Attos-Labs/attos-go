#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
echo "Running Go Enterprise Test Suite..."
go test -v -race -bench=. -benchmem -count=1 -timeout=120s ./...
echo "✓ All tests passed."
