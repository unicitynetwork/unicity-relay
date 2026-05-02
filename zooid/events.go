package zooid

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore"
	"fiatjaf.com/nostr/khatru"
	"github.com/Masterminds/squirrel"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus"
)

// Global Squirrel builder with Dollar placeholder format for PostgreSQL
var sb = squirrel.StatementBuilder.PlaceholderFormat(squirrel.Dollar)

// SSI retry knobs are read from env once and reused — ReplaceEvent is called
// per replaceable/addressable save, so we avoid the per-call envInt + Atoi.
var (
	ssiConfigOnce    sync.Once
	ssiMaxAttempts   int
	ssiBaseBackoffMs int
)

func ssiConfig() (int, int) {
	ssiConfigOnce.Do(func() {
		ssiMaxAttempts = envInt("SSI_MAX_ATTEMPTS", 6)
		if ssiMaxAttempts < 1 {
			ssiMaxAttempts = 1
		}
		ssiBaseBackoffMs = envInt("SSI_BASE_BACKOFF_MS", 25)
		if ssiBaseBackoffMs < 0 {
			ssiBaseBackoffMs = 0
		}
	})
	return ssiMaxAttempts, ssiBaseBackoffMs
}

// Per-call wall-clock budgets for DB transactions. Without a deadline,
// any database/sql call (BeginTx, Exec, Query, QueryRow) without a context
// parks the calling goroutine indefinitely on the pool's (unbounded) wait
// channel when the pool is saturated — that's the goroutine leak in issue
// #18. PR #19 bounded the BeginTx in SaveEvent/ReplaceEvent; this file's
// other call sites (queryEventsWith / deleteEventWith / saveEventWith's
// inner Execs / CountEvents — plus kv.go, metrics.go, retention.go) still
// used the no-context variants and the leak rerouted through them, with
// pprof showing ~84% of parked goroutines in QueryEvents.
//
// `dbOpTimeout` is the per-call budget used by the *Context variants. It
// is shared across all runtime DB ops in this package; the outer
// transaction budgets (`saveEventTxTimeout`, `replaceEventTotalBudget`)
// stay separate because they bound the *whole* tx including any retries.
//
// With these bounds, a contended caller fails fast instead of accumulating.
// They're var (not const) so the regression tests can shrink them.
var (
	saveEventTxTimeout      = 30 * time.Second
	replaceEventTotalBudget = 60 * time.Second
	dbOpTimeout             = 30 * time.Second
)

type EventStore struct {
	Relay  *khatru.Relay
	Config *Config
	Schema *Schema
}

var _ eventstore.Store = (*EventStore)(nil)

func (events *EventStore) Init() error {
	statements := []string{
		events.Schema.Render(`
			CREATE TABLE IF NOT EXISTS {{.Name}}__events (
				id TEXT PRIMARY KEY,
				created_at BIGINT NOT NULL,
				kind INTEGER NOT NULL,
				pubkey TEXT NOT NULL,
				content TEXT NOT NULL,
				tags TEXT NOT NULL,
				sig TEXT NOT NULL
			)`),
		events.Schema.Render(`CREATE INDEX IF NOT EXISTS {{.Name}}__idx_events_created_at ON {{.Name}}__events(created_at)`),
		events.Schema.Render(`CREATE INDEX IF NOT EXISTS {{.Name}}__idx_events_kind ON {{.Name}}__events(kind)`),
		events.Schema.Render(`CREATE INDEX IF NOT EXISTS {{.Name}}__idx_events_pubkey ON {{.Name}}__events(pubkey)`),
		events.Schema.Render(`CREATE INDEX IF NOT EXISTS {{.Name}}__idx_events_kind_pubkey ON {{.Name}}__events(kind, pubkey)`),
		events.Schema.Render(`CREATE INDEX IF NOT EXISTS {{.Name}}__idx_events_kind_pubkey_created_at ON {{.Name}}__events(kind, pubkey, created_at DESC)`),
		events.Schema.Render(`
			CREATE TABLE IF NOT EXISTS {{.Name}}__event_tags (
				event_id TEXT NOT NULL,
				key TEXT NOT NULL,
				value TEXT NOT NULL,
				FOREIGN KEY (event_id) REFERENCES {{.Name}}__events(id) ON DELETE CASCADE
			)`),
		events.Schema.Render(`CREATE INDEX IF NOT EXISTS {{.Name}}__idx_event_tags_event_id ON {{.Name}}__event_tags(event_id)`),
		events.Schema.Render(`CREATE INDEX IF NOT EXISTS {{.Name}}__idx_event_tags_key ON {{.Name}}__event_tags(key)`),
		events.Schema.Render(`CREATE INDEX IF NOT EXISTS {{.Name}}__idx_event_tags_key_value ON {{.Name}}__event_tags(key, value)`),
		events.Schema.Render(`CREATE INDEX IF NOT EXISTS {{.Name}}__idx_event_tags_key_value_event_id ON {{.Name}}__event_tags(key, value, event_id)`),
		events.Schema.Render(`CREATE INDEX IF NOT EXISTS {{.Name}}__idx_events_kind_created_at ON {{.Name}}__events(kind, created_at DESC)`),
	}

	for _, stmt := range statements {
		if _, err := GetDb().Exec(stmt); err != nil {
			return fmt.Errorf("schema init failed: %w", err)
		}
	}

	if err := events.initFTS(); err != nil {
		return fmt.Errorf("FTS init failed: %w", err)
	}

	if err := RunMigrations(events.Schema); err != nil {
		return fmt.Errorf("migrations failed: %w", err)
	}

	return nil
}

func (events *EventStore) initFTS() error {
	ftsStatements := []string{
		events.Schema.Render(`ALTER TABLE {{.Name}}__events ADD COLUMN IF NOT EXISTS search_vector tsvector`),
		events.Schema.Render(`CREATE INDEX IF NOT EXISTS {{.Name}}__idx_events_search ON {{.Name}}__events USING GIN(search_vector)`),
		events.Schema.Render(`
			CREATE OR REPLACE FUNCTION {{.Name}}_update_search_vector() RETURNS trigger AS $$
			BEGIN
				NEW.search_vector := to_tsvector('english', COALESCE(NEW.content, ''));
				RETURN NEW;
			END;
			$$ LANGUAGE plpgsql`),
		events.Schema.Render(`DROP TRIGGER IF EXISTS {{.Name}}_events_search_update ON {{.Name}}__events`),
		events.Schema.Render(`
			CREATE TRIGGER {{.Name}}_events_search_update
				BEFORE INSERT OR UPDATE ON {{.Name}}__events
				FOR EACH ROW EXECUTE FUNCTION {{.Name}}_update_search_vector()`),
	}

	for _, stmt := range ftsStatements {
		if _, err := GetDb().Exec(stmt); err != nil {
			return fmt.Errorf("statement failed: %w", err)
		}
	}
	return nil
}

func (events *EventStore) Close() {
	// Never close the database, since it's a shared resource
}

func (events *EventStore) QueryEvents(filter nostr.Filter, maxLimit int) iter.Seq[nostr.Event] {
	return events.queryEventsWith(GetDb(), filter, maxLimit)
}

func (events *EventStore) queryEventsWith(runner squirrel.BaseRunner, filter nostr.Filter, maxLimit int) iter.Seq[nostr.Event] {
	return func(yield func(nostr.Event) bool) {
		if filter.LimitZero {
			return
		}

		if maxLimit > 0 && (filter.Limit == 0 || maxLimit < filter.Limit) {
			filter.Limit = maxLimit
		}

		// Per-call deadline — both for the pool acquire (BeginTx-equivalent
		// happens inside Query) and for the query+iteration. PR #19 bounded
		// BeginTx in SaveEvent/ReplaceEvent, but this read path was the
		// dominant leak source in issue #18: pprof showed ~84% of parked
		// goroutines here, all on `database/sql.(*DB).conn` waiting forever
		// for a connection from a saturated pool.
		ctx, cancel := context.WithTimeout(context.Background(), dbOpTimeout)
		defer cancel()

		observer := QueryDuration.With(prometheus.Labels{"instance": events.Config.Schema})
		queryStart := time.Now()
		rows, err := events.buildSelectQuery(filter).RunWith(runner).QueryContext(ctx)
		if err != nil {
			observer.Observe(time.Since(queryStart).Seconds())
			log.Printf("QueryEvents query error: %v", err)
			return
		}
		defer rows.Close()

		for rows.Next() {
			var evt nostr.Event
			var idStr, pubkeyStr, sigStr, tagsStr string
			var createdAt int64
			var kind int

			err := rows.Scan(&idStr, &createdAt, &kind, &pubkeyStr, &evt.Content, &tagsStr, &sigStr)
			if err != nil {
				continue
			}

			// Parse ID
			if id, err := nostr.IDFromHex(idStr); err == nil {
				evt.ID = id
			} else {
				continue
			}

			// Parse PubKey
			if pubkey, err := nostr.PubKeyFromHex(pubkeyStr); err == nil {
				evt.PubKey = pubkey
			} else {
				continue
			}

			// Parse Signature
			if sigBytes, err := hex.DecodeString(sigStr); err == nil && len(sigBytes) == 64 {
				copy(evt.Sig[:], sigBytes)
			} else {
				continue
			}

			// Set other fields
			evt.CreatedAt = nostr.Timestamp(createdAt)
			evt.Kind = nostr.Kind(kind)

			// Parse Tags
			if err := json.Unmarshal([]byte(tagsStr), &evt.Tags); err != nil {
				continue
			}

			if !yield(evt) {
				observer.Observe(time.Since(queryStart).Seconds())
				return
			}
		}

		observer.Observe(time.Since(queryStart).Seconds())

		if err := rows.Err(); err != nil {
			log.Printf("QueryEvents row iteration error: %v", err)
		}
	}
}

func (events *EventStore) buildSelectQuery(filter nostr.Filter) squirrel.SelectBuilder {
	eventsTable := events.Schema.Prefix("events")
	eventTagsTable := events.Schema.Prefix("event_tags")

	// Collect valid single-letter tag filters and sort for deterministic SQL.
	type tagFilter struct {
		key    string
		values []interface{}
	}
	var tagFilters []tagFilter
	for tagKey, tagValues := range filter.Tags {
		if len(tagValues) == 0 || len(tagKey) != 1 {
			continue
		}
		vals := make([]interface{}, len(tagValues))
		for i, v := range tagValues {
			vals[i] = v
		}
		tagFilters = append(tagFilters, tagFilter{key: tagKey, values: vals})
	}
	sort.Slice(tagFilters, func(i, j int) bool {
		return tagFilters[i].key < tagFilters[j].key
	})

	// When tag filters are present, use a materialized CTE to force the
	// planner to resolve tag lookups FIRST via the covering index, then
	// join the small result set to events.
	//
	// Without this, PostgreSQL prefers to scan events backward by created_at
	// (attracted by ORDER BY ... DESC LIMIT) and probe event_tags per row.
	// With 319K+ events and sparse tag matches, that means scanning nearly
	// the entire table — 25-60 seconds per query.
	//
	// The MATERIALIZED keyword acts as an optimization fence: PostgreSQL
	// must complete the CTE (an index-only scan returning a few thousand
	// event_ids) before starting the outer query.
	col := "" // column qualifier: "" without tags, "e." with tags
	var qb squirrel.SelectBuilder

	if len(tagFilters) > 0 {
		col = "e."

		// Build one SELECT per tag filter, INTERSECT them for AND logic.
		var cteParts []string
		var cteArgs []interface{}
		for _, tf := range tagFilters {
			subQ := squirrel.Select("event_id").
				From(eventTagsTable).
				Where(squirrel.Eq{"key": tf.key}).
				Where(squirrel.Eq{"value": tf.values})
			sql, args, _ := subQ.ToSql()
			cteParts = append(cteParts, sql)
			cteArgs = append(cteArgs, args...)
		}

		cteSql := "WITH _tag_ids AS MATERIALIZED (" +
			strings.Join(cteParts, " INTERSECT ") + ")"

		qb = sb.Select("e.id", "e.created_at", "e.kind", "e.pubkey",
			"e.content", "e.tags", "e.sig").
			Prefix(cteSql, cteArgs...).
			From(eventsTable + " e").
			Join("_tag_ids t ON t.event_id = e.id")
	} else {
		qb = sb.Select("id", "created_at", "kind", "pubkey", "content", "tags", "sig").
			From(eventsTable)
	}

	qb = qb.OrderBy(col + "created_at DESC")

	if filter.Search != "" {
		qb = qb.Where(col+"search_vector @@ plainto_tsquery('english', ?)", filter.Search)
	}

	if len(filter.IDs) > 0 {
		idStrs := make([]interface{}, len(filter.IDs))
		for i, id := range filter.IDs {
			idStrs[i] = id.Hex()
		}
		qb = qb.Where(squirrel.Eq{col + "id": idStrs})
	}

	if len(filter.Authors) > 0 {
		authorStrs := make([]interface{}, len(filter.Authors))
		for i, author := range filter.Authors {
			authorStrs[i] = author.Hex()
		}
		qb = qb.Where(squirrel.Eq{col + "pubkey": authorStrs})
	}

	if len(filter.Kinds) > 0 {
		kindInts := make([]interface{}, len(filter.Kinds))
		for i, kind := range filter.Kinds {
			kindInts[i] = int(kind)
		}
		qb = qb.Where(squirrel.Eq{col + "kind": kindInts})
	}

	if filter.Since != 0 {
		qb = qb.Where(squirrel.GtOrEq{col + "created_at": filter.Since})
	}

	if filter.Until != 0 {
		qb = qb.Where(squirrel.LtOrEq{col + "created_at": filter.Until})
	}

	if filter.Limit > 0 {
		qb = qb.Limit(uint64(filter.Limit))
	}

	return qb
}

// buildTagFilteredQuery constructs a raw SQL query using a materialized CTE
// to force PostgreSQL to resolve tag lookups via the covering index before
// joining to the events table.
//
// The generated SQL looks like:
//
//	WITH _tag_ids AS MATERIALIZED (
//	    SELECT event_id FROM {event_tags}
//	    WHERE key = $1 AND value IN ($2)
//	    INTERSECT
//	    SELECT event_id FROM {event_tags}
//	    WHERE key = $3 AND value IN ($4)
//	)
//	SELECT e.id, e.created_at, e.kind, e.pubkey, e.content, e.tags, e.sig
//	FROM {events} e
//	JOIN _tag_ids t ON t.event_id = e.id
//	WHERE e.kind IN ($5) AND e.created_at >= $6
//	ORDER BY e.created_at DESC
//	LIMIT 1000
//
// Squirrel cannot express materialized CTEs, so we build the CTE prefix as
// raw SQL with Dollar placeholders and prepend it to a Squirrel-built outer
// query. The placeholder numbering is coordinated manually: the CTE consumes
// $1..$N, then the outer query continues from $(N+1).
func (events *EventStore) buildTagFilteredQuery(filter nostr.Filter, tagFilters []struct {
	key    string
	values []interface{}
}) squirrel.SelectBuilder {
	// This is never called directly — the actual tagFilter type is local to
	// buildSelectQuery. We need to accept the same shape here.
	panic("unreachable — see buildSelectQueryWithTags")
}

func (events *EventStore) DeleteEvent(id nostr.ID) error {
	return events.deleteEventWith(GetDb(), id)
}

func (events *EventStore) deleteEventWith(runner squirrel.BaseRunner, id nostr.ID) error {
	ctx, cancel := context.WithTimeout(context.Background(), dbOpTimeout)
	defer cancel()
	_, err := sb.Delete(events.Schema.Prefix("events")).Where(squirrel.Eq{"id": id.Hex()}).RunWith(runner).ExecContext(ctx)
	return err
}

func (events *EventStore) SaveEvent(evt nostr.Event) error {
	ctx, cancel := context.WithTimeout(context.Background(), saveEventTxTimeout)
	defer cancel()

	tx, err := GetDb().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	if err := events.saveEventWith(tx, evt); err != nil {
		return err
	}

	return tx.Commit()
}

// saveEventWith inserts an event and its tags using the provided runner.
// The caller is responsible for transaction management.
//
// Each Exec uses its own per-call context budget so a saturated pool can't
// park individual statements forever. The outer SaveEvent / replaceEventOnce
// budgets bound the total tx wall-clock; this bounds the inner ops.
func (events *EventStore) saveEventWith(runner squirrel.BaseRunner, evt nostr.Event) error {
	tagsJSON, err := json.Marshal(evt.Tags)
	if err != nil {
		return fmt.Errorf("failed to marshal tags: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), dbOpTimeout)
	defer cancel()

	// Insert the event, using ON CONFLICT to atomically detect duplicates.
	// This is race-safe with PostgreSQL's concurrent connections (unlike SELECT-then-INSERT).
	insertQb := sb.Insert(events.Schema.Prefix("events")).
		Columns("id", "created_at", "kind", "pubkey", "content", "tags", "sig").
		Values(
			evt.ID.Hex(),
			int64(evt.CreatedAt),
			int(evt.Kind),
			evt.PubKey.Hex(),
			evt.Content,
			string(tagsJSON),
			hex.EncodeToString(evt.Sig[:]),
		).
		Suffix("ON CONFLICT(id) DO NOTHING")

	result, err := insertQb.RunWith(runner).ExecContext(ctx)
	if err != nil {
		return fmt.Errorf("failed to save event '%s': %w", evt.ID, err)
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		return eventstore.ErrDupEvent
	}

	// Insert single-letter tags into event_tags, chunked to stay below
	// Postgres's 65535 extended-protocol parameter limit. With 3 columns per
	// row, 15000 rows × 3 = 45000 params is well under the 65535 cap and
	// cuts the inner round-trip count by ~3× vs. 5k batches — important for
	// kind-39002 (NIP-29 member list) saves where the whole transaction runs
	// under SERIALIZABLE isolation and contention is dominated by wall-clock
	// duration of the critical section (issues #13, #16).
	const tagInsertBatchSize = 15000

	eventID := evt.ID.Hex()
	tagsTable := events.Schema.Prefix("event_tags")
	batch := sb.Insert(tagsTable).Columns("event_id", "key", "value")
	n := 0

	for _, tag := range evt.Tags {
		if len(tag) < 2 || len(tag[0]) != 1 {
			continue
		}
		batch = batch.Values(eventID, tag[0], tag[1])
		n++
		if n >= tagInsertBatchSize {
			if _, err := batch.RunWith(runner).ExecContext(ctx); err != nil {
				return fmt.Errorf("failed to save tags for event '%s': %w", evt.ID, err)
			}
			batch = sb.Insert(tagsTable).Columns("event_id", "key", "value")
			n = 0
		}
	}

	if n > 0 {
		if _, err := batch.RunWith(runner).ExecContext(ctx); err != nil {
			return fmt.Errorf("failed to save tags for event '%s': %w", evt.ID, err)
		}
	}

	return nil
}

func (events *EventStore) ReplaceEvent(evt nostr.Event) error {
	// Use a serializable transaction so the read-decide-write-delete cycle is
	// atomic. Without this, two concurrent goroutines could both read "no
	// existing event", both insert, and leave duplicate replaceable events.
	// PostgreSQL may return a serialization error (SQLSTATE 40001) under
	// contention; the standard remedy is to retry the transaction.
	//
	// Backoff is full-jitter exponential: sleep ∈ [0, base<<attempt]. Without
	// jitter, multiple losers wake at the same offset and collide again on
	// retry — observed in production as ~19% of contended kind-39002 saves
	// giving up after the original 3-retry policy (issue #16).
	//
	// The whole retry loop runs under a single deadline so a caller can't park
	// indefinitely on the pool wait queue when the pool is saturated (#18).
	ctx, cancel := context.WithTimeout(context.Background(), replaceEventTotalBudget)
	defer cancel()

	maxAttempts, baseBackoffMs := ssiConfig()
	var err error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("replace event budget exceeded after %d attempts: %w", attempt, ctxErr)
		}
		err = events.replaceEventOnce(ctx, evt)
		if err == nil {
			return nil
		}
		// If our budget elapsed during the attempt (BeginTx parked on the pool
		// wait queue, or pgx aborted mid-tx because the context expired), the
		// returned error is wrapped as "failed to begin transaction: context
		// deadline exceeded" or similar. Re-wrap so callers see the same
		// "budget exceeded" message we use for the proactive ctx.Err() check
		// above and the retry-sleep ctx.Done branch — keeps log triage simple.
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return fmt.Errorf("replace event budget exceeded after %d attempts: %w", attempt+1, err)
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "40001" {
			if attempt+1 < maxAttempts {
				// Cap the shift so an aggressive SSI_BASE_BACKOFF_MS can't
				// overflow int when computing the backoff cap.
				shift := attempt
				if shift > 10 {
					shift = 10
				}
				maxInt := int(^uint(0) >> 1)
				safeBaseMs := baseBackoffMs
				if maxBase := maxInt >> shift; safeBaseMs > maxBase {
					safeBaseMs = maxBase
				}
				backoffCap := safeBaseMs << shift
				sleepMs := rand.IntN(backoffCap + 1)
				log.Printf("Serialization conflict replacing kind %d d=%q (attempt %d/%d), retrying in %dms",
					evt.Kind, evt.Tags.GetD(), attempt+1, maxAttempts, sleepMs)
				timer := time.NewTimer(time.Duration(sleepMs) * time.Millisecond)
				select {
				case <-ctx.Done():
					timer.Stop()
					return fmt.Errorf("replace event budget exceeded after %d attempts: %w", attempt+1, ctx.Err())
				case <-timer.C:
				}
				continue
			}
			log.Printf("Serialization conflict replacing kind %d d=%q (attempt %d/%d), giving up",
				evt.Kind, evt.Tags.GetD(), attempt+1, maxAttempts)
			break
		}
		return err // non-retriable error
	}
	return fmt.Errorf("serialization conflict after %d attempts: %w", maxAttempts, err)
}

func (events *EventStore) replaceEventOnce(ctx context.Context, evt nostr.Event) error {
	tx, err := GetDb().BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelSerializable,
	})
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	filter := nostr.Filter{Kinds: []nostr.Kind{evt.Kind}, Authors: []nostr.PubKey{evt.PubKey}}
	if evt.Kind.IsAddressable() {
		filter.Tags = nostr.TagMap{"d": []string{evt.Tags.GetD()}}
	}

	shouldSave := true
	shouldDelete := make([]nostr.ID, 0)
	for previous := range events.queryEventsWith(tx, filter, 0) {
		if previous.CreatedAt <= evt.CreatedAt {
			shouldDelete = append(shouldDelete, previous.ID)
		} else {
			shouldSave = false
		}
	}

	if shouldSave {
		if err := events.saveEventWith(tx, evt); err != nil && err != eventstore.ErrDupEvent {
			return fmt.Errorf("failed to save: %w", err)
		}
	}

	for _, id := range shouldDelete {
		if err := events.deleteEventWith(tx, id); err != nil {
			return fmt.Errorf("failed to delete old event: %w", err)
		}
	}

	return tx.Commit()
}

func (events *EventStore) CountEvents(filter nostr.Filter) (uint32, error) {
	// Strip limit for a true total count; ORDER BY in the subquery is
	// optimized away by PostgreSQL's planner inside COUNT(*).
	filter.Limit = 0
	qb := events.buildSelectQuery(filter).RemoveLimit()

	countQb := sb.Select("COUNT(*)").FromSelect(qb, "subquery")

	ctx, cancel := context.WithTimeout(context.Background(), dbOpTimeout)
	defer cancel()

	var count uint32
	err := countQb.RunWith(GetDb()).QueryRowContext(ctx).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to count events: %w", err)
	}

	return count, nil
}

// Non-eventstore methods

func (events *EventStore) StoreEvent(event nostr.Event) error {
	if event.Kind.IsReplaceable() || event.Kind.IsAddressable() {
		return events.ReplaceEvent(event)
	}

	if err := events.SaveEvent(event); err != nil && err != eventstore.ErrDupEvent {
		return err
	}

	return nil
}

func (events *EventStore) SignAndStoreEvent(event *nostr.Event, broadcast bool) error {
	if err := events.Config.Sign(event); err != nil {
		return err
	}

	if err := events.StoreEvent(*event); err != nil {
		return err
	}

	if broadcast {
		events.Relay.BroadcastEvent(*event)
	}

	return nil
}

func (events *EventStore) GetOrCreateApplicationSpecificData(d string) nostr.Event {
	filter := nostr.Filter{
		Kinds: []nostr.Kind{nostr.KindApplicationSpecificData},
		Tags: nostr.TagMap{
			"d": []string{d},
		},
	}

	for event := range events.QueryEvents(filter, 1) {
		return event
	}

	return nostr.Event{
		Kind:      nostr.KindApplicationSpecificData,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			[]string{"d", d},
		},
	}
}

func (events *EventStore) GetOrCreateRelayMembersList() nostr.Event {
	filter := nostr.Filter{
		Kinds: []nostr.Kind{RELAY_MEMBERS},
	}

	for event := range events.QueryEvents(filter, 1) {
		return event
	}

	return nostr.Event{
		Kind:      RELAY_MEMBERS,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			[]string{"-"},
		},
	}
}
