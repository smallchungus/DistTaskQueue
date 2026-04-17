#!/usr/bin/env bash
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

hooks_dir="$(git rev-parse --git-path hooks)"
mkdir -p "$hooks_dir"

for hook in pre-commit pre-push; do
  src="scripts/hooks/$hook"
  dst="$hooks_dir/$hook"
  ln -sf "../../$src" "$dst"
  chmod +x "$src"
  echo "installed: $dst -> $src"
done
