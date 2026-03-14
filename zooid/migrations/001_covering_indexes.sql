-- Covering index on event_tags: enables index-only scan for tag lookups.
-- The planner can now resolve (key, value) -> event_id without touching the heap.
CREATE INDEX CONCURRENTLY IF NOT EXISTS {{.Name}}__idx_event_tags_key_value_event_id
  ON {{.Name}}__event_tags(key, value, event_id);

-- Composite index on events: avoids post-filtering non-matching kinds after
-- the created_at index scan. Lets the planner satisfy both kind= and
-- ORDER BY created_at DESC from a single index.
CREATE INDEX CONCURRENTLY IF NOT EXISTS {{.Name}}__idx_events_kind_created_at
  ON {{.Name}}__events(kind, created_at DESC);
