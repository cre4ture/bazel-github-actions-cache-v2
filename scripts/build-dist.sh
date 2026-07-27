#!/usr/bin/env bash
set -euo pipefail

project_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
go_version=$(tr -d '[:space:]' < "$project_dir/.go-version")
actual_version=$(go version | awk '{print $3}' | sed 's/^go//')
if [[ "$actual_version" != "$go_version" ]]; then
  echo "Go $go_version is required for reproducible release binaries; found $actual_version" >&2
  exit 1
fi

version=${VERSION:-dev}
ldflags="-s -w -buildid= -X main.version=$version"
for architecture in amd64 arm64; do
  output="$project_dir/dist/cache-server-linux-$architecture"
  CGO_ENABLED=0 GOOS=linux GOARCH="$architecture" \
    go build -buildvcs=false -trimpath -ldflags "$ldflags" -o "$output" ./cmd/cache-server
  chmod 0755 "$output"
done
(
  cd "$project_dir/dist"
  sha256sum cache-server-linux-* > SHA256SUMS
)
