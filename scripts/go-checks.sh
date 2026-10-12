#!/usr/bin/env bash
set -euo pipefail
export GOTOOLCHAIN=local
expected="go$(awk '/^go / {print $2}' go.mod)"
test "$(go env GOVERSION)" = "$expected"
test -z "$(gofmt -l cmd internal)"
go vet ./...
go test -race ./...
python3 scripts/test_oci_evidence.py
SRA_TOOL_DIR=$(mktemp -d)
trap 'rm -rf "$SRA_TOOL_DIR"' EXIT
GOBIN="$SRA_TOOL_DIR" go install golang.org/x/vuln/cmd/govulncheck@v1.9.0
for architecture in amd64 arm64; do
  GOOS=linux GOARCH="$architecture" "$SRA_TOOL_DIR/govulncheck" ./...
done
