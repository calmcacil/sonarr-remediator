#!/usr/bin/env bash
set -euo pipefail
export GOTOOLCHAIN=local
expected="go$(awk '/^go / {print $2}' go.mod)"
test "$(go env GOVERSION)" = "$expected"
SRA_UNFORMATTED=$(gofmt -l cmd internal)
if test -n "$SRA_UNFORMATTED"; then
  printf 'Go formatting required:\n%s\n' "$SRA_UNFORMATTED" >&2
  exit 1
fi
go vet ./...
go test -race ./...
python -m unittest discover -s scripts -p 'test_*.py'
for architecture in amd64 arm64; do
  GOOS=linux GOARCH="$architecture" govulncheck ./...
done
