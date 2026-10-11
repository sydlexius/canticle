# internal/queue

- **A row that records a file or a hand edit is never reopened.** Automatic paths (upgrade sweep, word recheck, categorical reopen) must keep the hand-protected predicate in the statement itself.
- **`work_queue.upstream` is written in the SAME statement as `provider_lane`** and follows the lane, NOT the file. Attribution commits with the lane or not at all (see the settle comment in `queue.go`).
