#!/usr/bin/env bash
# Assert the product name is capitalized in USER-FACING PROSE.
#
# The logotype is "Canticle". Everything the reader sees -- README, docs/, and
# the release notes derived from them -- must spell it that way. The lowercase
# form is reserved for IDENTIFIERS, where it is not a name at all: the Go module
# path, the GHCR image ref, the binary, a filesystem path, a URL.
#
# WHY THIS IS NOT A `typos` RULE. typos matches word-for-word and cannot see
# context, so teaching it `canticle -> Canticle` would flag every
# `ghcr.io/sydlexius/canticle` and `github.com/sydlexius/canticle` in the tree.
# The distinction being enforced here is prose-vs-identifier, which requires
# masking the spans where the lowercase form is CORRECT before matching.
#
# Masked before matching: fenced code blocks, inline code spans, URLs, markdown
# link targets, and HTML tags (which carry src=/alt= attributes).
#
# Usage: check-product-name.sh [file ...]   (default: README.md + docs/*.md)
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

if [ "$#" -gt 0 ]; then
  files=("$@")
else
  files=(README.md)
  while IFS= read -r f; do files+=("$f"); done < <(find docs -maxdepth 1 -name '*.md' | sort)
fi

python3 - "${files[@]}" <<'PY'
import re, sys

# A name-shaped occurrence: not glued to a path separator, dot, dash, or word
# char on either side. That alone still matches inside code/URLs, which is why
# those spans are masked out first.
NAME = re.compile(r'(?<![/\w.-])canticle(?![\w./-])')
MASK = re.compile(r'```.*?```|`[^`]*`|https?://\S+|\[[^\]]*\]\([^)]*\)|<[^>]+>', re.S)

hits = 0
for path in sys.argv[1:]:
    try:
        src = open(path, encoding='utf-8').read()
    except FileNotFoundError:
        continue
    # Replace masked spans with same-length blanks so offsets stay truthful and
    # the reported line number matches the real file.
    masked = MASK.sub(lambda m: ' ' * len(m.group(0)), src)
    lines = src.split('\n')
    for m in NAME.finditer(masked):
        n = src[:m.start()].count('\n') + 1
        hits += 1
        print(f"{path}:{n}: {lines[n-1].strip()[:100]}")

if hits:
    print(f"\n{hits} lowercase 'canticle' in user-facing prose; the logotype is 'Canticle'.",
          file=sys.stderr)
    print("Lowercase is correct ONLY in identifiers (module path, image ref, binary, URLs),",
          file=sys.stderr)
    print("which this check already masks -- so every hit above is real prose.", file=sys.stderr)
    sys.exit(1)
PY
