#!/usr/bin/env bash
set -euo pipefail
version=$(sh backend/scripts/resolve-version.sh)
if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
  echo 'Version must be a Docker-compatible semantic version (without build metadata).' >&2
  exit 1
fi
notes="changelog/$version/CHANGELOG.md"
if [[ ! -f "$notes" ]] || ! grep -q '[^[:space:]#]' "$notes"; then
  echo "Missing or empty release notes: $notes" >&2
  exit 1
fi
python3 - "$notes" <<'PYNOTES'
import pathlib, sys
body = pathlib.Path(sys.argv[1]).read_text().strip()
if body.startswith('# '):
    body = body.partition('\n')[2].strip()
if not body or not any(line.strip() and not line.lstrip().startswith('#') for line in body.splitlines()):
    raise SystemExit('Release notes must contain a nonempty body')
PYNOTES
{
  echo "version=$version"
  echo "tag=v$version"
  echo "ghcr=ghcr.io/$(printf '%s' "$GITHUB_REPOSITORY" | tr '[:upper:]' '[:lower:]')"
  echo "date=$(git show -s --format=%cI HEAD)"
  echo "commit=$(git rev-parse HEAD)"
} >> "$GITHUB_OUTPUT"
