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
-- symptom -- a report screen and a /metrics label rendering hundreds of
-- kilobytes -- in place on exactly the deployments that hit the bug.
--
-- On the deployment this was diagnosed against, ONE row held 531,138 bytes of
-- raw ffmpeg stderr: 34% of every last_error byte in the database (1,549,278
-- total across all rows). The content is ffmpeg's per-frame decode diagnostics
-- on a corrupt MP3 -- one 'Header missing' line per bad frame until the
-- decode-error-rate ceiling aborts the run.
--
-- BYTES, NOT CHARACTERS -- AND THIS IS THE SUBTLE PART. SQLite's length() and
-- substr() count CHARACTERS; Go's cap counts BYTES. A first draft of this
-- migration used length()/substr() throughout and was WRONG IN BOTH DIRECTIONS
-- on non-ASCII input:
--
--   * A 12,000-character value of 2-byte runes bounded to 4,096 CHARACTERS --
--     8,162 BYTES, double the cap it was supposed to enforce.
--   * A 3,000-character value of 4-byte runes is 12,000 bytes, but length()
--     reports 3,000, so `WHERE length(...) > 4096` never matched and the row
--     escaped bounding ENTIRELY.
--
-- The predicate below therefore measures length(CAST(... AS BLOB)), the true
-- byte length, so an oversized-in-bytes row can never evade it.
--
-- THE CUT IS DELIBERATELY MORE CONSERVATIVE THAN ffmpeg.BoundOutput. The Go
-- helper cuts at a byte offset and walks back to a rune boundary. SQL has no
-- cheap equivalent: a byte-wise substr over a BLOB would split a rune and put
-- invalid UTF-8 into a TEXT column (and from there into the HTML report), while
-- a character-wise substr cannot predict its own byte length. So the character
-- budget here is derived from the byte budget assuming the WORST CASE of 4
-- bytes per character. That is exact for the pathological population (ffmpeg
-- stderr is ASCII) in the sense that it never exceeds the cap, and for ASCII it
-- retains ~1,046 characters rather than the ~4,065 the Go helper would keep.
--
-- Retaining less is the correct direction to err for a value whose entire defect
-- was being too large, and it costs nothing real: on the row that motivated this,
-- the whole diagnostic payload -- the line naming the failing decoder and the
-- 'Decode error rate exceeds maximum' line that terminated the run -- is roughly
-- 450 characters. The middle that is dropped is one line repeated thousands of
-- times.
--
-- Because the cut is character-wise it can never split a rune, so the output is
-- always valid UTF-8 without any boundary repair.
--
-- HEAD AND TAIL, NOT A PREFIX. Both ends are kept because they carry different
-- information: the opening lines name the stream and decoder that failed, and
-- the closing lines carry the cause that actually terminated the run. A plain
-- head truncation would keep the symptom and discard the diagnosis.
--
-- ONLY OVERSIZED ROWS ARE TOUCHED, and the statement is re-runnable: a bounded
-- row is far under the byte threshold and no longer matches, so re-applying this
-- is a no-op independently of goose. An ordinary last_error is left
-- byte-identical.
--
-- LOSSY BY DESIGN, AND THAT IS THE POINT. The omitted BYTE count is written into
-- the marker so the elision is self-describing and a reader can never mistake a
-- bounded value for a complete one.
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
--
-- The repeated marker subexpression is spelled out rather than factored into a
-- CTE so the statement stays a single self-contained UPDATE; SQLite evaluates it
-- per row either way.
UPDATE work_queue
SET last_error =
    substr(
        last_error,
        1,
        ((4096 - length('... [' || (length(CAST(last_error AS BLOB)) - 4096) || ' bytes omitted] ...') - 2) / 4) / 2
    )
    || char(10)
    || '... [' || (length(CAST(last_error AS BLOB)) - 4096) || ' bytes omitted] ...'
    || char(10)
    || substr(
        last_error,
        -(
            ((4096 - length('... [' || (length(CAST(last_error AS BLOB)) - 4096) || ' bytes omitted] ...') - 2) / 4)
            - ((4096 - length('... [' || (length(CAST(last_error AS BLOB)) - 4096) || ' bytes omitted] ...') - 2) / 4) / 2
        )
    )
WHERE length(CAST(last_error AS BLOB)) > 4096;
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
