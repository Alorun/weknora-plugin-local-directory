#!/usr/bin/env bash
set -euo pipefail
# The SDK is not published yet. Both modules are workspace mains; no invented
# downloadable version or committed absolute replace is necessary.
plugin_root=$(cd "$(dirname "$0")/.." && pwd)
sdk_root=$(cd "${1:?usage: with-sdk.sh /path/to/WeKnora command [args...]}" && pwd)
shift
test -f "$sdk_root/pkg/plugin/sdk/contract.go"
workspace=$(mktemp -d /tmp/wk-local-sdk-XXXXXXXX)
cleanup() { find "$workspace" -depth -delete; }
trap cleanup EXIT
export GOWORK="$workspace/go.work"
go work init "$plugin_root" "$sdk_root"
cd "$plugin_root"
"$@"
