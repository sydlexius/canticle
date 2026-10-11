# internal/orchestrator

- No threshold lives here: the timing decision is `lyrics.DecidePromotion`'s. A result ends the dispatch only if it is suitable AND would be promoted as-is; a categorical result falls through to the other lanes.
