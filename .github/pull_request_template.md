## Summary

- What changed and why (focus on the why; 1-3 bullets).
- Note any user-visible behavior change.
- Call out anything reviewers should look at first.

## Linked issue

Closes #

(Use `Part of #N` for a slice of a larger issue that is not yet fully resolved. GitHub binds the keyword to the ONE number right after it -- repeat it per issue: `Closes #A, closes #B`.)

## Pre-flight checklist

- [ ] Pre-push gate green locally (`make gate`), or run via `/prep-pr` which invokes it.
- [ ] Code review pass complete; critical and important findings fixed before pushing.
- [ ] Commits squashed into one clean commit before the first push (reviewers read the diff at PR open).
- [ ] Label set on `gh pr create --label ...` (no CI gate enforces this, but it keeps the tracker usable).
- [ ] UAT performed for user-visible changes (run the binary, not just the test suite).
- [ ] Screenshot attached for any web-UI change, taken from the rendered page at branch HEAD.
- [ ] `make ui` re-run if any `.templ` or CSS source changed. Generated `*_templ.go` and
      `web/static/css/output.css` are gitignored and built on demand -- do **not** commit them.
- [ ] Migration added under `internal/db/migrations/` if this is a one-time data correction
      (goose gives run-once and transactionality for free; do not hand-roll it in Go).

## Test plan

- [ ] `make gate` passes locally. It runs, in order: conflict markers, gofmt, `make ui`,
      `go build`, `go test -race` + coverage, patch coverage (Codecov parity), coverage-floor
      ratchet, codecov report validation, golangci-lint, actionlint, govulncheck.
- [ ] New tests were confirmed to **fail against unfixed code** before the fix was written.
      Tests that only ever passed are regression guards -- label them as such rather than
      presenting them as proof the fix works.
- [ ] Patch coverage meets the Codecov threshold (`make patch-cover`).
- [ ] Which **surface** this change fixes is stated explicitly, and was verified by looking at
      that surface. `/metrics` reads `provider_outcomes`; the dashboard reads `lane_attempts`.
      Correcting one does not correct the other, and a DB-level check will not catch it.
- [ ] Manual UAT steps (list the specific flows exercised):
  - [ ]
- [ ] Reviewer follow-ups (anything you want a second pair of eyes on):
  - [ ]

## Not covered

What this change does **not** verify -- the layers left untested, and anything deferred to a
follow-up issue.
