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
# Masked before matching: fenced code blocks, inline code spans (any run length
# of backticks, e.g. ``canticle``), URLs, markdown/HTML link and image TARGETS
# (the label stays prose -- "[canticle docs](url)" must still flag "canticle"),
# and HTML tags (which carry src=/alt= attributes).
#
# Usage:
#   check-product-name.sh [file ...]           check the WORKTREE copy (default:
#                                               README.md + docs/*.md)
#   check-product-name.sh --staged [file ...]  check the STAGED blob (git index),
#                                               not whatever is sitting in the
#                                               worktree -- what pre-commit hooks
#                                               must use, since the worktree can
#                                               differ from what is about to be
#                                               committed (a fix made after
#                                               `git add` but never re-staged)
#   check-product-name.sh --ref REF [file ...] check the blob at git ref REF
#                                               (e.g. HEAD) -- what a pre-push
#                                               gate should use, since HEAD is
#                                               what actually ships
#
# --staged and --ref read content via `git show`, never the filesystem, so
# neither mode can be fooled by an unstaged worktree edit.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

MODE=worktree
REF=
if [ "${1:-}" = "--staged" ]; then
  MODE=staged
  shift
elif [ "${1:-}" = "--ref" ]; then
  MODE=ref
  REF="${2:?--ref requires a git ref argument}"
  shift 2
fi

if [ "$#" -gt 0 ]; then
  files=("$@")
else
  files=(README.md)
  while IFS= read -r f; do files+=("$f"); done < <(find docs -maxdepth 1 -name '*.md' | sort)
fi

PRODNAME_MODE="$MODE" PRODNAME_REF="$REF" python3 - "${files[@]}" <<'PY'
import os
import re
import subprocess
import sys

MODE = os.environ.get('PRODNAME_MODE', 'worktree')
REF = os.environ.get('PRODNAME_REF', '')

# A name-shaped occurrence: not glued to a path separator, dot, dash, or word
# char on either side. That alone still matches inside code/URLs, which is why
# those spans are masked out first.
NAME = re.compile(r'(?<![/\w.-])canticle(?![\w./-])')

HTML_TAG = re.compile(r'<[^>]+>')
# Backtick-delimited inline code, any run length (`x`, ``x``, ```x```, ...);
# the closing run must match the opening run's length exactly (CommonMark
# semantics), via the backreference -- this is what a fixed `` `[^`]*` `` `
# cannot express, and why a double-backtick span like ``canticle`` used to
# read as bare prose.
CODE_SPAN = re.compile(r'(`+)(?:(?!\1).)*?\1', re.S)
# Markdown link/image: [label](target). Blanks ONLY the target, since the
# label is prose the reader sees -- "[canticle docs](url)" must still flag
# "canticle" in the label even though the url itself is an identifier.
LINK = re.compile(r'!?\[([^\]]*)\]\(([^)]*)\)')
BARE_URL = re.compile(r'https?://\S+')
FENCE_OPEN = re.compile(r'^(`{3,}|~{3,})')
FENCE_CLOSE = re.compile(r'^(`{3,}|~{3,})[ \t]*$')


def blank(m):
    return ' ' * len(m.group(0))


def blank_link_target(m):
    whole = m.group(0)
    start = m.start(2) - m.start(0)
    end = m.end(2) - m.start(0)
    return whole[:start] + (' ' * (end - start)) + whole[end:]


def mask_fences(src):
    # Fenced code blocks (``` or ~~~) are line-oriented, unlike every other
    # mask below, so they are handled first and separately: blanking whole
    # lines here means the backtick-pairing CODE_SPAN regex never sees the
    # fence markers and cannot mispair across a fence boundary.
    lines = src.split('\n')
    out = []
    in_fence = False
    fence_char = ''
    fence_len = 0
    for line in lines:
        stripped = line.lstrip(' \t')
        if not in_fence:
            m = FENCE_OPEN.match(stripped)
            if m:
                in_fence = True
                fence_char = m.group(1)[0]
                fence_len = len(m.group(1))
                out.append(' ' * len(line))
                continue
            out.append(line)
        else:
            m = FENCE_CLOSE.match(stripped)
            if m and m.group(1)[0] == fence_char and len(m.group(1)) >= fence_len:
                in_fence = False
            out.append(' ' * len(line))
    return '\n'.join(out)


def mask(src):
    # Order matters only where a construct can nest inside another: a code
    # span must be masked before LINK/BARE_URL run, so a link- or URL-shaped
    # string sitting inside backticks (an example in prose, say) is blanked
    # as code and never re-examined as a link target.
    masked = mask_fences(src)
    masked = HTML_TAG.sub(blank, masked)
    masked = CODE_SPAN.sub(blank, masked)
    masked = LINK.sub(blank_link_target, masked)
    masked = BARE_URL.sub(blank, masked)
    return masked


def read_source(path):
    if MODE == 'worktree':
        try:
            return open(path, encoding='utf-8').read()
        except FileNotFoundError:
            return None
    spec = f"{REF}:{path}" if MODE == 'ref' else f":{path}"
    result = subprocess.run(
        ['git', 'show', spec],
        capture_output=True, text=True, encoding='utf-8',
    )
    if result.returncode != 0:
        # Not present at this ref/stage (untracked, deleted, not staged) --
        # nothing to check, same as the worktree mode's missing-file skip.
        return None
    return result.stdout


hits = 0
for path in sys.argv[1:]:
    src = read_source(path)
    if src is None:
        continue
    # Replace masked spans with same-length blanks so offsets stay truthful and
    # the reported line number matches the real file.
    masked = mask(src)
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
