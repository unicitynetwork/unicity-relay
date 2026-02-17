package zooid

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"iter"
	"log"

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

	events.initFTS()
	return nil
}

func (events *EventStore) initFTS() {
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
			log.Printf("FTS init warning: %v", err)
		}
	}
}

func (events *EventStore) Close() {
	// Never close the database, since it's a shared resource
}

func (events *EventStore) QueryEvents(filter nostr.Filter, maxLimit int) iter.Seq[nostr.Event] {
	return func(yield func(nostr.Event) bool) {
		if filter.LimitZero {
			return
		}

		if maxLimit > 0 && maxLimit < filter.Limit {
			filter.Limit = maxLimit
		}

		rows, err := events.buildSelectQuery(filter).RunWith(GetDb()).Query()
		if err != nil {
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

	for tagKey, tagValues := range filter.Tags {
		if len(tagValues) == 0 {
			continue
		}

		if len(tagKey) != 1 {
			continue
		}

		tagValueInterfaces := make([]interface{}, len(tagValues))
		for i, tagValue := range tagValues {
			tagValueInterfaces[i] = tagValue
		}

		// Use Question format for the sub-query so that when its SQL is embedded
		// into the outer Dollar-format query, the ? placeholders get properly
		// renumbered to $N by the outer builder.
		subQuery := squirrel.Select("event_id").
			From(events.Schema.Prefix("event_tags")).
			Where(squirrel.Eq{"key": tagKey}).
			Where(squirrel.Eq{"value": tagValueInterfaces})

		subQuerySql, subQueryArgs, _ := subQuery.ToSql()
		qb = qb.Where("id IN ("+subQuerySql+")", subQueryArgs...)
	}

	if filter.Limit > 0 {
		qb = qb.Limit(uint64(filter.Limit))
	}

	return qb
}

func (events *EventStore) DeleteEvent(id nostr.ID) error {
	_, err := sb.Delete(events.Schema.Prefix("events")).Where(squirrel.Eq{"id": id.Hex()}).RunWith(GetDb()).Exec()

	return err
}

func (events *EventStore) SaveEvent(evt nostr.Event) error {
	// Serialize tags to JSON
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

	result, err := insertQb.RunWith(GetDb()).Exec()
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
		_, _ = tagQb.RunWith(GetDb()).Exec()
	}

	return nil
}

func (events *EventStore) ReplaceEvent(evt nostr.Event) error {
	filter := nostr.Filter{Kinds: []nostr.Kind{evt.Kind}, Authors: []nostr.PubKey{evt.PubKey}}
	if evt.Kind.IsAddressable() {
		filter.Tags = nostr.TagMap{"d": []string{evt.Tags.GetD()}}
	}

	shouldSave := true
	shouldDelete := make([]nostr.ID, 0)
	for previous := range events.QueryEvents(filter, 1) {
		if previous.CreatedAt <= evt.CreatedAt {
			shouldDelete = append(shouldDelete, previous.ID)
		} else {
			shouldSave = false
		}
	}

	if shouldSave {
		if err := events.SaveEvent(evt); err != nil && err != eventstore.ErrDupEvent {
			return fmt.Errorf("failed to save: %w", err)
		}
	}

	// Wait until the end to delete old events, just in case our new one doesn't save
	for _, id := range shouldDelete {
		events.DeleteEvent(id)
	}

	return nil
}

func (events *EventStore) CountEvents(filter nostr.Filter) (uint32, error) {
	// Build a count query based on the select query but with COUNT(*) instead
	qb := events.buildSelectQuery(filter)

	// Convert the select query to a count query
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
