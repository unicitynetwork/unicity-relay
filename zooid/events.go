package zooid

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"iter"
	"log"
	"sort"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore"
	"fiatjaf.com/nostr/khatru"
	"github.com/Masterminds/squirrel"
)

// Global Squirrel builder with Dollar placeholder format for PostgreSQL
var sb = squirrel.StatementBuilder.PlaceholderFormat(squirrel.Dollar)

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

		rows, err := events.buildSelectQuery(filter).RunWith(runner).Query()
		if err != nil {
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
				return
			}
		}

		if err := rows.Err(); err != nil {
			log.Printf("QueryEvents row iteration error: %v", err)
		}
	}
}

func (events *EventStore) buildSelectQuery(filter nostr.Filter) squirrel.SelectBuilder {
	qb := sb.Select("id", "created_at", "kind", "pubkey", "content", "tags", "sig").
		From(events.Schema.Prefix("events")).
		OrderBy("created_at DESC")

	// Handle search with tsvector FTS
	if filter.Search != "" {
		qb = qb.Where("search_vector @@ plainto_tsquery('english', ?)", filter.Search)
	}

	if len(filter.IDs) > 0 {
		idStrs := make([]interface{}, len(filter.IDs))
		for i, id := range filter.IDs {
			idStrs[i] = id.Hex()
		}
		qb = qb.Where(squirrel.Eq{"id": idStrs})
	}

	if len(filter.Authors) > 0 {
		authorStrs := make([]interface{}, len(filter.Authors))
		for i, author := range filter.Authors {
			authorStrs[i] = author.Hex()
		}
		qb = qb.Where(squirrel.Eq{"pubkey": authorStrs})
	}

	if len(filter.Kinds) > 0 {
		kindInts := make([]interface{}, len(filter.Kinds))
		for i, kind := range filter.Kinds {
			kindInts[i] = int(kind)
		}
		qb = qb.Where(squirrel.Eq{"kind": kindInts})
	}

	if filter.Since != 0 {
		qb = qb.Where(squirrel.GtOrEq{"created_at": filter.Since})
	}

	if filter.Until != 0 {
		qb = qb.Where(squirrel.LtOrEq{"created_at": filter.Until})
	}

	// Tag filters use IN-subqueries against event_tags. The covering index
	// (key, value, event_id) enables index-only scans on these subqueries,
	// making the planner prefer a hash semi-join over a full events scan.
	//
	// Collect and sort tag filters for deterministic SQL output (Go map
	// iteration order is random).
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

	for _, tf := range tagFilters {
		subQuery := squirrel.Select("event_id").
			From(events.Schema.Prefix("event_tags")).
			Where(squirrel.Eq{"key": tf.key}).
			Where(squirrel.Eq{"value": tf.values})

		subQuerySql, subQueryArgs, _ := subQuery.ToSql()
		qb = qb.Where("id IN ("+subQuerySql+")", subQueryArgs...)
	}

	if filter.Limit > 0 {
		qb = qb.Limit(uint64(filter.Limit))
	}

	return qb
}

func (events *EventStore) DeleteEvent(id nostr.ID) error {
	return events.deleteEventWith(GetDb(), id)
}

func (events *EventStore) deleteEventWith(runner squirrel.BaseRunner, id nostr.ID) error {
	_, err := sb.Delete(events.Schema.Prefix("events")).Where(squirrel.Eq{"id": id.Hex()}).RunWith(runner).Exec()
	return err
}

func (events *EventStore) SaveEvent(evt nostr.Event) error {
	tx, err := GetDb().Begin()
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
func (events *EventStore) saveEventWith(runner squirrel.BaseRunner, evt nostr.Event) error {
	tagsJSON, err := json.Marshal(evt.Tags)
	if err != nil {
		return fmt.Errorf("failed to marshal tags: %w", err)
	}

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

	result, err := insertQb.RunWith(runner).Exec()
	if err != nil {
		return fmt.Errorf("failed to save event '%s': %w", evt.ID, err)
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		return eventstore.ErrDupEvent
	}

	// Insert single-letter tags into event_tags table (batched)
	tagQb := sb.Insert(events.Schema.Prefix("event_tags")).
		Columns("event_id", "key", "value")

	hasTags := false
	for _, tag := range evt.Tags {
		if len(tag) >= 2 && len(tag[0]) == 1 {
			tagQb = tagQb.Values(evt.ID.Hex(), tag[0], tag[1])
			hasTags = true
		}
	}

	if hasTags {
		if _, err := tagQb.RunWith(runner).Exec(); err != nil {
			return fmt.Errorf("failed to save tags for event '%s': %w", evt.ID, err)
		}
	}

	return nil
}

func (events *EventStore) ReplaceEvent(evt nostr.Event) error {
	// Use a serializable transaction so the read-decide-write-delete cycle is
	// atomic. Without this, two concurrent goroutines could both read "no
	// existing event", both insert, and leave duplicate replaceable events.
	// PostgreSQL may return a serialization error under contention; in that
	// case the client can simply retry the publish.
	tx, err := GetDb().BeginTx(context.Background(), &sql.TxOptions{
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

	var count uint32
	err := countQb.RunWith(GetDb()).QueryRow().Scan(&count)
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
