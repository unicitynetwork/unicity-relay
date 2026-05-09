-- Denormalize the event's `kind` onto event_tags so the tag-filter CTE in
-- buildSelectQuery can pre-filter by kind via a covering index. Without
-- this, hot groups whose tag rows are dominated by membership events
-- (NIP-29 kinds 9000/9021) hash-join ~97k tag rows for `h='<group>'`
-- just to throw away ~95% on the kind filter — see issue #23.
--
-- Two statements:
--   1. ADD COLUMN nullable. Instant on PG 11+ (metadata-only).
--   2. CREATE INDEX IF NOT EXISTS for the matching covering index.
--      Idempotent — runs on every fresh schema, no-ops elsewhere.
--
-- The runner enforces a 30s per-statement deadline. On an empty or
-- small event_tags this is comfortable. On a busy production schema
-- with hundreds of thousands of rows, the inline CREATE INDEX would
-- exceed that budget and migration apply would fail — by design.
-- That fail-fast is the contract: production deploys MUST first run
-- a one-shot CREATE INDEX CONCURRENTLY plus the kind backfill via
-- the dbops task, so by the time this migration runs both the
-- column and the index already exist and the IF NOT EXISTS pair are
-- a no-op. Exact ops commands live in the PR description for issue
-- #23. Until the backfill is verified complete, the read path
-- emits `kind IN (...) OR kind IS NULL` so un-backfilled rows still
-- match.
ALTER TABLE {{.Name}}__event_tags ADD COLUMN IF NOT EXISTS kind INTEGER;
CREATE INDEX IF NOT EXISTS {{.Name}}__idx_event_tags_key_value_kind_event_id
  ON {{.Name}}__event_tags(key, value, kind, event_id);
