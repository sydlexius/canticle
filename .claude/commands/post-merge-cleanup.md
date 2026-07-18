---
description: "Automate post-merge cleanup: update main, delete branches, prune refs"
argument-hint: "<branch-name>"
allowed-tools: ["Bash", "Glob", "Grep", "Read", "Edit"]
---

# Post-Merge Cleanup

Run after a PR is merged to clean up the working environment.

**Branch name:** $ARGUMENTS

If no branch name is provided, detect from the most recently merged PR:

```bash
gh pr list --state merged --limit 1 --json headRefName --jq '.[0].headRefName'
```

If that fails, stop and ask: "Which branch was just merged?"

---

## Step 1 -- Update local main

```bash
git checkout main && git pull --ff-only
```

If `pull --ff-only` fails, stop and explain. Do not force-pull.

---

## Step 2 -- Delete local branch

```bash
git branch -d "$branch"
```

If the branch is not fully merged, stop and warn. Do not force-delete.

---

## Step 3 -- Delete remote branch

```bash
encoded_branch=$(printf '%s' "$branch" | jq -sRr @uri)
gh api "repos/sydlexius/canticle/git/refs/heads/$encoded_branch" -X DELETE
```

If the remote branch is already deleted (404), note it and continue.

---

## Step 4 -- Prune stale remote refs

```bash
git fetch --prune
```

---

## Summary

After all steps, report:
- Whether main was updated
- Which branches were deleted (local and remote)
- Any warnings or errors encountered
