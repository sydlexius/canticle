#!/usr/bin/env bash
# check-push-receipt.sh -- decide whether a push is already covered by a gate
# receipt, so .githooks/pre-push does not re-run the gate that /prep-pr's
# gate-runner.py just ran on the same tree.
#
# Reads the pre-push ref lines on STDIN, exactly as git hands them to the hook
# (githooks(5)): "<local ref> SP <local sha> SP <remote ref> SP <remote sha>".
#
# Exit status:
#   0  covered: the push may skip the gate (one PASS line on stdout)
#   1  not covered: run the normal hook (the reason is one NOTE line on stderr)
#
# FAIL-CLOSED. Every doubt returns 1 and the hook runs its checks as usual. The
# receipt is reused only when ALL of these hold:
#   - $(git rev-parse --git-dir)/prep-pr-receipt.json exists (PREP_PR_RECEIPT overrides the path)
#   - it is a JSON object with schema=gate-receipt/v1, producer=gate-runner,
#     result=pass, and a 40-hex tree_sha
#   - EVERY non-delete ref being pushed resolves (<sha>^{tree}) to that tree
#   - the working tree is clean (no staged, unstaged or untracked changes)
#
# Why the TREE and not the commit: gate-runner records HEAD^{tree}, and the tree
# is exactly the content the gate judged. A commit that only rewrites the message
# (amend, reword) keeps the tree, so it stays covered, which is correct: nothing
# the gate reads has changed. safe-push.sh binds its own receipt check the same
# way.
#
# Why a clean working tree: the gate ran over the files on disk, and the receipt
# records only what HEAD's tree was when it finished. If files are dirty now, the
# gated files may not have been the committed ones. Clean-now is the proxy
# safe-push.sh uses too; it is not proof of clean-then, which is why the tree
# match is the load-bearing check.
#
# A delete ref (local sha all zeros) sends no content, so it needs no gate. A
# push made up only of deletes is covered without consulting the receipt.
set -uo pipefail

note() { echo "pre-push: NOTE: $1; running the push checks." >&2; exit 1; }

zero_re='^(0{40}|0{64})$' # a delete is ALL zeros, full length; never a mere 0 prefix
sha_re='^[0-9a-fA-F]{40}([0-9a-fA-F]{24})?$'

pushed_shas=()
saw_line=0
while read -r local_ref local_sha remote_ref remote_sha; do
  [ -n "${local_ref:-}" ] || continue
  saw_line=1
  if ! [[ "${local_sha:-}" =~ $sha_re ]]; then
    note "unrecognized pre-push ref line"
  fi
  [[ "$local_sha" =~ $zero_re ]] && continue # delete: nothing is sent
  pushed_shas+=("$local_sha")
  : "$remote_ref" "$remote_sha"
done

[ "$saw_line" -eq 1 ] || note "no refs on stdin"
if [ "${#pushed_shas[@]}" -eq 0 ]; then
  echo "pre-push: PASS (delete-only push; nothing to gate)"
  exit 0
fi

git_dir="$(git rev-parse --git-dir 2>/dev/null)" || note "cannot resolve the git dir"
receipt="${PREP_PR_RECEIPT:-$git_dir/prep-pr-receipt.json}"
[ -f "$receipt" ] || note "no gate receipt at $receipt"
command -v python3 >/dev/null 2>&1 || note "python3 not found, cannot read the receipt"

# STDOUT is the tree on success, or the reason on failure. stderr is dropped so a
# stray interpreter warning never reaches the comparison below.
if ! r_tree=$(python3 -c 'import json, re, sys
def die(msg):
    print(msg)
    sys.exit(1)
try:
    with open(sys.argv[1], encoding="utf-8") as f:
        d = json.load(f)
except Exception as e:
    die("receipt is not readable JSON (%s)" % type(e).__name__)
if not isinstance(d, dict):
    die("receipt is not a JSON object")
for k, want in (("schema", "gate-receipt/v1"), ("producer", "gate-runner"), ("result", "pass")):
    if d.get(k) != want:
        die("receipt %s is %s, expected %s" % (k, json.dumps(d.get(k)), json.dumps(want)))
t = d.get("tree_sha")
if not isinstance(t, str) or not re.fullmatch("[0-9a-fA-F]{40}", t):
    die("receipt tree_sha is not a 40-hex SHA")
print(t.lower())' "$receipt" 2>/dev/null); then
  note "$(printf '%s' "${r_tree:-receipt could not be checked}" | tr '\n' ' ')"
fi

for sha in "${pushed_shas[@]}"; do
  tree="$(git rev-parse --verify --quiet "$sha^{tree}" 2>/dev/null)" || note "cannot resolve the tree of $sha"
  [ "$tree" = "$r_tree" ] || note "stale receipt: it gated tree ${r_tree:0:12}, but $sha is tree ${tree:0:12}"
done

status="$(git status --porcelain --untracked-files=normal 2>/dev/null)" || note "cannot read git status"
[ -z "$status" ] || note "working tree has uncommitted or untracked changes"

echo "pre-push: PASS (gate receipt covers tree ${r_tree:0:12}; skipping the re-run)"
exit 0
