#!/usr/bin/env bash
set -euo pipefail
prefix="${HOME}/.local"
if [[ "${1:-}" == "--prefix" && $# -eq 2 ]]; then
  prefix="$2"
elif [[ $# -ne 0 ]]; then
  echo 'Usage: ./scripts/install.sh [--prefix DIRECTORY]' >&2
  exit 2
fi
case "$(uname -s)" in
  Darwin|Linux) ;;
  *) echo 'wbot supports macOS and Linux.' >&2; exit 1 ;;
esac
if ! command -v go >/dev/null 2>&1; then
  echo 'Go 1.24+ is required. Install it from https://go.dev/dl/ first.' >&2
  exit 1
fi
root="$(cd "$(dirname "$0")/.." && pwd)"
mkdir -p "$prefix/bin"
(cd "$root" && go build -trimpath -o "$prefix/bin/wbot" ./cmd/wbot)
printf 'Installed: %s/bin/wbot\n' "$prefix"
printf 'Next: wbot init --webhook https://your-agent.example/events\n'
