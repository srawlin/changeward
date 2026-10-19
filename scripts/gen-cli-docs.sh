#!/usr/bin/env bash
# Regenerate docs/cli.md from the CLI's own help text: scripts/gen-cli-docs.sh [path/to/changegate]
set -euo pipefail
bin=${1:-bin/changegate}
out=docs/cli.md
subs() { "$bin" "$@" --help 2>/dev/null | awk '/^Available Commands:/{f=1;next} /^$/{f=0} f{print $1}' | grep -vE '^(help|completion)$' || true; }
section() {
  local level=$1; shift
  printf '%s `changegate %s`\n\n```text\n' "$level" "$*"
  "$bin" "$@" --help | sed -e '/^Global Flags:/,$d' -e 's/[[:space:]]*$//'
  printf '```\n\n'
}
{
  echo "# CLI reference"
  echo
  echo "Generated from \`changegate --help\` by \`scripts/gen-cli-docs.sh\`. Every command also accepts \`--json\`"
  echo "for machine-readable output."
  echo
  for c in $(subs); do
    section "##" "$c"
    for s in $(subs "$c"); do section "###" "$c" "$s"; done
  done
} > "$out"
echo "wrote $out"
