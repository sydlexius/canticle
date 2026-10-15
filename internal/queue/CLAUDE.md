# internal/queue

- **A row that records a file or a hand edit is never reopened.** Automatic paths (upgrade sweep, word recheck, categorical reopen) must keep the hand-protected predicate in the statement itself.
- **`work_queue.upstream` is written in the SAME statement as `provider_lane`** and follows the lane, NOT the file. Attribution commits with the lane or not at all (see the settle comment in `queue.go`).
- **The row identity is `(artist_key, title_key, edition)`** (#1525, migration 075). `edition` is set on insert from `SetEditionRule` (nil rule: always `''`) and is never updated in place; every `ON CONFLICT` names all three columns, and a lookup that assumes one row per `(artist_key, title_key)` is wrong once a rule is set. A future `work_queue` rebuild must carry the column and the `idx_work_queue_identity` index.
