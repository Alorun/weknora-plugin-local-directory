#!/usr/bin/env bash
set -euo pipefail
plugin_root=$(cd "$(dirname "$0")/.." && pwd)
sdk_root=${1:?usage: build.sh /path/to/WeKnora [new-artifact-directory]}
output=${2:-"$plugin_root/dist/community.local-directory"}
case "$output" in /*) ;; *) echo "artifact output must be an absolute path" >&2; exit 1 ;; esac
# Never overwrite an installed/active artifact. Build a new package directory.
if test -e "$output"; then
  echo "output already exists; choose a new artifact directory: $output" >&2
  exit 1
fi
mkdir -p "$output/bin"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 bash "$plugin_root/scripts/with-sdk.sh" "$sdk_root" \
  go build -trimpath -buildvcs=false -o "$output/bin/plugin-linux-amd64" .
cp "$plugin_root/plugin.yaml" "$output/plugin.yaml"
printf 'artifact: %s\n' "$output"
