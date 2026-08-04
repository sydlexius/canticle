-- +goose Up
-- +goose StatementBegin
-- Bound oversized work_queue.last_error values already stored (#731).
--
-- WHY A MIGRATION AND NOT ONLY THE CODE FIX. The accompanying change caps
-- captured subprocess output at its three capture sites, so no NEW oversized
-- value can be written. It does nothing for values already in the database, and
-- those persist indefinitely: a 'deferred' row keeps its last_error until it
-- eventually succeeds, and a row whose source file is corrupt never will. Every
-- install that has ever fed a corrupt audio file to the detector is carrying
-- these rows right now, so shipping the code fix alone would leave the actual
-- symptom -- a report screen rendering hundreds of kilobytes -- in place on
-- exactly the deployments that hit the bug.
--
-- On the deployment this was diagnosed against, ONE row held 531,138 bytes of
-- raw ffmpeg stderr: 34% of every last_error byte in the database (1,549,278
-- total across all rows). The content is ffmpeg's per-frame decode diagnostics
-- on a corrupt MP3 -- one 'Header missing' line per bad frame until the
-- decode-error-rate ceiling aborts the run.
--
-- THE POLICY MIRRORS ffmpeg.BoundOutput DELIBERATELY. Head and tail are kept
-- and the middle is elided, because the two ends carry different information:
-- the opening lines name the stream and decoder that failed, and the closing
-- lines carry the cause that actually terminated the run. A plain head
-- truncation would keep the symptom and discard the diagnosis. The retained
-- sizes and the marker text are kept in step with internal/ffmpeg/capture.go;
-- TestMigrationBoundMatchesBoundOutput pins the two together so a change to the
-- Go cap that forgets this file fails the build rather than drifting silently.
--
-- CHARACTERS VS BYTES. SQLite's length() and substr() count CHARACTERS, while
-- the Go cap counts BYTES. Two consequences, both benign. First, substr can
-- never split a multi-byte rune, so the UTF-8 hazard handled explicitly in Go is
-- structurally absent here. Second, for non-ASCII text this cap is CONSERVATIVE
-- (a 4096-character result is at most 4096 bytes only when every character is
-- ASCII), so a row bounded here is always at or under the Go byte cap, never
-- over. Erring small is the safe direction for a value whose whole problem was
-- being too large.
--
-- ONLY OVERSIZED ROWS ARE TOUCHED. The WHERE clause means an ordinary
-- last_error -- the overwhelming majority, all well under the cap -- is left
-- byte-identical. The statement is re-runnable: a row already bounded is shorter
-- than the threshold and no longer matches, so re-applying this is a no-op
-- independently of goose.
--
-- LOSSY BY DESIGN, AND THAT IS THE POINT. The elided middle is the same line
-- repeated thousands of times; it carries no information the head and tail do
-- not already give. The omitted-byte count is written into the marker so the
-- elision is self-describing and a reader can never mistake a bounded value for
-- a complete one.
--
-- THIS IS NOT REDACTION. It bounds SIZE only. Secrets are kept out of these
-- strings at their construction sites (#431); a size cap is as likely to
-- preserve a secret as to cut one, and must never be relied on as a control.
--
-- Side effect: the update_work_queue_updated_at trigger (migration 012) fires on
-- each touched row, so updated_at is rewritten to the deploy timestamp for the
-- rows corrected here. Cosmetic, and the blast radius is tiny (single-digit rows
-- on the reference database, versus ~48k for migration 032, which documented the
-- same effect). Noted so the bump is not mistaken for real churn.
-- THE RETAINED SIZES ARE DERIVED, NOT HARDCODED. The marker embeds the omitted
-- count, so its length varies with that number's magnitude -- a fixed head/tail
-- split is therefore off by a character or two depending on the input, and a
-- first draft of this statement overshot the cap by exactly one byte because of
-- it. Computing the head from the marker's ACTUAL rendered length keeps the
-- result at or under the cap for every input instead of for the one that was
-- tested. TestMigration043BoundMatchesBoundOutput pins this against
-- ffmpeg.BoundOutput so the two cannot drift apart silently.
--
-- The tail is fixed at 2033 and the head absorbs the variation, because the tail
-- carries the terminating cause -- the most valuable line in the stream -- and
-- should not shrink to pay for a longer count.
UPDATE work_queue
SET last_error =
    substr(
        last_error,
        1,
        4096 - 2033 - 2 - length('... [' || (length(last_error) - 4096) || ' bytes omitted] ...')
    )
    || char(10)
    || '... [' || (length(last_error) - 4096) || ' bytes omitted] ...'
    || char(10)
    || substr(last_error, -2033)
WHERE length(last_error) > 4096;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Irreversible by design: the elided text is discarded, not stashed, so there is
-- nothing to restore it from. That is deliberate -- retaining a copy would keep
-- the bytes this migration exists to remove. Deliberate no-op; restore from
-- backup if a full stderr stream must be recovered. (Matches migrations 028 and
-- 032.)
SELECT 1;
-- +goose StatementEnd
