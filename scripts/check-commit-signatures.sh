#!/usr/bin/env bash
# check-commit-signatures.sh -- refuse to push commits that carry no signature.
# main's ruleset has required_signatures, so an unsigned commit is blocked at
# merge time; this catches it before the push instead.
#
# Usage:
#   check-commit-signatures.sh            commits in <merge-base>..HEAD, where the
#                                         base is the merge base with origin/main;
#                                         when origin/main does not resolve, every
#                                         commit of HEAD no remote-tracking ref has
#                                         (all of HEAD if there are none)
#   check-commit-signatures.sh <range>    an explicit rev-list range (A..B)
#   check-commit-signatures.sh --refs     read pre-push ref lines on STDIN
#                                         (githooks(5): "<local ref> <local sha>
#                                         <remote ref> <remote sha>") and check what
#                                         each ref would send: remote..local, or, for
#                                         a new branch (all-zero remote sha, or one
#                                         not present locally), every commit of local
#                                         that no remote-tracking ref has (all of its
#                                         history if there are none). A delete
#                                         (all-zero local sha) sends nothing: skipped.
#
# Exit status:
#   0  every commit in range is signed (or the range is empty)
#   1  at least one unsigned commit; each is named on stderr with a fix hint
#   2  setup error (bad range, unreadable commit): FAIL CLOSED. A missing base is NOT
#      an error: it widens the check to every commit no remote has. A LOCAL branch is
#      never a boundary, since it is no evidence the remote holds those commits.
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

ranges=()
case "${1:-}" in
  "")
    if base="$(git merge-base origin/main HEAD 2>/dev/null)" && [ -n "$base" ]; then
      ranges+=("$base..HEAD")
    else
      ranges+=("HEAD --not --remotes")
    fi
    ;;
  --refs)
    while read -r _ lsha _ rsha; do
      [ -n "${lsha:-}" ] || continue
      [[ "$lsha" =~ $zero_re ]] && continue # delete: nothing is sent
      if [[ "${rsha:-}" =~ $zero_re ]] || ! git cat-file -e "${rsha:-x}^{commit}" 2>/dev/null; then
        ranges+=("$lsha --not --remotes") # new remote ref: what no remote is known to have
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
  # shellcheck disable=SC2086 # reason: a range entry is deliberately several rev-list words
  shas="$(git rev-list $r 2>/dev/null)" || { echo "check-commit-signatures: bad range '$r'" >&2; exit 2; }
  for sha in $shas; do
    raw="$(git cat-file commit "$sha" 2>/dev/null)" || { echo "check-commit-signatures: cannot read $sha" >&2; exit 2; }
    # The header block ends at the first blank line; a message line must not count.
    # Pure bash: an early-closing pipeline (sed q | grep -q) SIGPIPEs the writer on a
    # large message and reads a signed commit as unsigned under pipefail.
    hdr="${raw%%$'\n\n'*}"
    if [[ $'\n'"$hdr" != *$'\n'"gpgsig "* ]]; then
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
