-- +goose Up
-- +goose StatementBegin
-- Upstream licensor of a settled row (#1297). A multiplexing lane (innertube)
-- routes each track to one licensor. The worker stores it with provider_lane in
-- ONE statement (queue.SetProviderLane), reduced by lyrics.RecordedUpstream, the
-- rule the [upstream:] tag shares. The column FOLLOWS provider_lane, NOT THE
-- FILE: it equals the [upstream:] tag wherever a tag block is written (a tagged
-- .lrc, a provider instrumental marker), and is still recorded where the file
-- carries no tag block (an unsynced .txt, a demoted mis-synced .txt) or nothing
-- was written (a categorical result). NULL means no upstream recorded (a lane
-- that is its own upstream, a detector settle, a cache hit, or a row that
-- predates this column). Every path that clears the lane clears it. No index. A
-- FUTURE table rebuild (see 049) MUST carry it.
ALTER TABLE work_queue ADD COLUMN upstream TEXT;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE work_queue DROP COLUMN upstream;
-- +goose StatementEnd
