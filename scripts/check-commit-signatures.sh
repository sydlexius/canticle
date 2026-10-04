#!/usr/bin/env bash
# check-commit-signatures.sh -- refuse to push commits that carry no signature.
# main's ruleset has required_signatures, so an unsigned commit is blocked at
# merge time; this catches it before the push instead.
#
# Usage:
#   check-commit-signatures.sh            commits in <merge-base>..HEAD, where the
#                                         base is the merge base with origin/main,
#                                         else main
#   check-commit-signatures.sh <range>    an explicit rev-list range (A..B)
#   check-commit-signatures.sh --refs     read pre-push ref lines on STDIN
#                                         (githooks(5): "<local ref> <local sha>
#                                         <remote ref> <remote sha>") and check what
#                                         each ref would send: remote..local, or, for
#                                         a new branch (all-zero remote sha, or one
#                                         not present locally), the merge base with
#                                         origin/main (else main)..local. A delete
#                                         (all-zero local sha) sends nothing: skipped.
#
# Exit status:
#   0  every commit in range is signed (or the range is empty)
#   1  at least one unsigned commit; each is named on stderr with a fix hint
#   2  setup error (no base resolves, bad range, unreadable commit): FAIL CLOSED
#
# What "signed" means: the commit object has a `gpgsig` header. PRESENCE only, on
# purpose. This repo signs with SSH keys (gpg.format=ssh); `git verify-commit` and
# %G? need an allowed-signers file that may not be configured on every machine and
# would reject good commits. GitHub verifies the signature itself at merge time.
# Merge commits made on GitHub by `gh pr update-branch` are signed by GitHub and
# carry the header, so they pass with no special case.
#
# Why this exists beyond `commit.gpgsign=true`: `git commit-tree` ignores that
# setting, so a commit re-created with it (squash, reword) is unsigned.
set -uo pipefail

zero_re='^(0{40}|0{64})$'

base_for() { # base_for <sha> -> merge base with origin/main, else main; fails if none
  local ref b
  for ref in origin/main main; do
    b="$(git merge-base "$ref" "$1" 2>/dev/null)" && [ -n "$b" ] && { echo "$b"; return 0; }
  done
  return 1
}

ranges=()
case "${1:-}" in
  "")
    base="$(base_for HEAD)" || { echo "check-commit-signatures: cannot resolve a base (origin/main, main); refusing to pass" >&2; exit 2; }
    ranges+=("$base..HEAD")
    ;;
  --refs)
    while read -r _ lsha _ rsha; do
      [ -n "${lsha:-}" ] || continue
      [[ "$lsha" =~ $zero_re ]] && continue # delete: nothing is sent
      if [[ "${rsha:-}" =~ $zero_re ]] || ! git cat-file -e "${rsha:-x}^{commit}" 2>/dev/null; then
        base="$(base_for "$lsha")" || { echo "check-commit-signatures: cannot resolve a base for $lsha; refusing to pass" >&2; exit 2; }
        ranges+=("$base..$lsha")
      else
        ranges+=("$rsha..$lsha")
      fi
    done
    ;;
  -*) echo "usage: $0 [<range> | --refs]" >&2; exit 2 ;;
  *) ranges+=("$1") ;;
esac

bad=0
for r in "${ranges[@]+"${ranges[@]}"}"; do
  shas="$(git rev-list "$r" 2>/dev/null)" || { echo "check-commit-signatures: bad range '$r'" >&2; exit 2; }
  for sha in $shas; do
    hdr="$(git cat-file commit "$sha" 2>/dev/null)" || { echo "check-commit-signatures: cannot read $sha" >&2; exit 2; }
    # The header block ends at the first blank line; a message line must not count.
    if ! printf '%s\n' "$hdr" | sed '/^$/q' | grep -q '^gpgsig '; then
      echo "UNSIGNED: $(git log -1 --format='%h %s' "$sha")" >&2
      bad=1
    fi
  done
done

if [ "$bad" -eq 1 ]; then
  echo "fix: recreate each commit signed (git commit --amend -S --no-edit before the first push, or pass -S to git commit-tree); main requires signed commits" >&2
  exit 1
fi
echo "check-commit-signatures: PASS (all commits signed)"
