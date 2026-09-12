---
name: warn-hand-rolled-pr-poll
enabled: true
event: bash
action: warn
pattern: (?:while|until)\b[^\n]*gh\s+pr\s+(?:view|checks)|gh\s+pr\s+(?:view|checks)[^\n]*\bsleep\b|\bsleep\b[^\n]*gh\s+pr\s+(?:view|checks)|gh\s+pr\s+checks[^\n]*--(?:watch|interval)
---

Use `/pr-watch <pr>` instead of hand-rolling a PR-status poll.

CLAUDE.md: "Watch PRs with `pr-watch.sh` ... not hand-rolled Monitor loops."
`/pr-watch` waits silently and returns ONE terminal line (`settled` /
`review-blocked` / `timeout`) once CI has finished AND CodeRabbit has reviewed --
it handles the empty-`conclusion` and CR-trickle races a raw `gh pr checks` loop
gets wrong, and it does not burn turns re-polling.

This is a WARNING, not a block. What it flags is WAITING, never the command
name: a `while`/`until` loop around `gh pr view/checks`, either of those paired
with a `sleep` on the same line, or `gh pr checks --watch`/`--interval` (gh's
own built-in poll). If that is what you are doing, stop and arm `/pr-watch`.

A one-off SNAPSHOT is fine and does not fire: a bare `gh pr checks <pr>` prints
once and exits, exactly like `gh pr view <pr> --json state,mergeStateStatus,
reviewDecision` inside a merge gate or `/post-merge-cleanup`. A `for` loop over
a LIST of PRs (push-release step 5 reads each merged PR once) is iteration, not
polling, and is silent too.
