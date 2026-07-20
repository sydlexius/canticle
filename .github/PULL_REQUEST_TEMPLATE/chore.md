## Summary

- What changed and why (focus on the why; 1-3 bullets).
- Note anything reviewers should look at first.

## Linked issue

Closes #

(Use `Part of #N` for a slice of a larger issue that is not yet fully resolved.)

## Pre-flight checklist

- [ ] Pre-push gate green locally (`make gate`), or run via `/prep-pr` which invokes it.
- [ ] Code review pass complete; critical and important findings fixed before pushing.
- [ ] Commits squashed into one clean commit before the first push.
- [ ] Label set on `gh pr create --label ...` (`chore`, `ci`, `docker`, or `documentation`).
- [ ] No user-visible behavior change -- if there is one, use the default PR template instead.

## Test plan

- [ ] `make gate` passes locally.
- [ ] For a CI or workflow change: `actionlint` clean, Actions pinned to commit SHAs (with `# vX`),
      `persist-credentials: false` on checkout steps, and no `paths-ignore` on a trigger that has
      required status checks (a check that never runs stays pending forever).
- [ ] For a tool-version bump: `make sync-tool-versions` passes (the golangci-lint pin must agree
      across `ci.yml` and `.pre-commit-config.yaml`).
- [ ] Reviewer follow-ups (anything you want a second pair of eyes on):
  - [ ]
