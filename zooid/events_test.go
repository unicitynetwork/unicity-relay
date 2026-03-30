package zooid

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"fiatjaf.com/nostr"
)

func createTestEventStore() *EventStore {
	schema := &Schema{Name: "test_" + RandomString(8)}
	config := &Config{
		Host:   "test.com",
		secret: nostr.Generate(),
	}
	return &EventStore{
		Config: config,
		Schema: schema,
	}
}

func createTestEvent(kind nostr.Kind, content string) nostr.Event {
	secret := nostr.Generate()
	event := nostr.Event{
		Kind:      kind,
		CreatedAt: nostr.Now(),
		Content:   content,
		Tags:      nostr.Tags{{"t", "test"}, {"p", "testpubkey"}},
	}
	event.Sign(secret)
	return event
}

func TestEventStore_Init(t *testing.T) {
	store := createTestEventStore()

	err := store.Init()
	if err != nil {
		t.Errorf("EventStore.Init() error = %v", err)
	}
}

func TestEventStore_SaveEvent(t *testing.T) {
	store := createTestEventStore()
	store.Init()

	event := createTestEvent(nostr.KindTextNote, "test content")

	err := store.SaveEvent(event)
	if err != nil {
		t.Errorf("EventStore.SaveEvent() error = %v", err)
	}

	// Try to save the same event again - should return duplicate error
	err = store.SaveEvent(event)
	if err == nil {
		t.Error("EventStore.SaveEvent() should return error for duplicate event")
	}
}

func TestEventStore_QueryEvents_Basic(t *testing.T) {
	store := createTestEventStore()
	store.Init()

	event1 := createTestEvent(nostr.KindTextNote, "first event")
	event2 := createTestEvent(nostr.KindTextNote, "second event")

	store.SaveEvent(event1)
	store.SaveEvent(event2)

	// Query all events
	filter := nostr.Filter{}
	events := make([]nostr.Event, 0)
	for evt := range store.QueryEvents(filter, 0) {
		events = append(events, evt)
	}

	if len(events) != 2 {
		t.Errorf("QueryEvents() returned %d events, want 2", len(events))
	}
}

func TestEventStore_QueryEvents_ByKind(t *testing.T) {
	store := createTestEventStore()
	store.Init()

	textEvent := createTestEvent(nostr.KindTextNote, "text note")
	metadataEvent := createTestEvent(nostr.KindProfileMetadata, "metadata")

	store.SaveEvent(textEvent)
	store.SaveEvent(metadataEvent)

	// Query only text notes
	filter := nostr.Filter{Kinds: []nostr.Kind{nostr.KindTextNote}}
	events := make([]nostr.Event, 0)
	for evt := range store.QueryEvents(filter, 0) {
		events = append(events, evt)
	}

	if len(events) != 1 {
		t.Errorf("QueryEvents() by kind returned %d events, want 1", len(events))
	}

	if events[0].Kind != nostr.KindTextNote {
		t.Errorf("QueryEvents() by kind returned wrong kind: got %v, want %v", events[0].Kind, nostr.KindTextNote)
	}
}

func TestEventStore_QueryEvents_ByAuthor(t *testing.T) {
	store := createTestEventStore()
	store.Init()

	secret1 := nostr.Generate()
	secret2 := nostr.Generate()

	event1 := nostr.Event{Kind: nostr.KindTextNote, CreatedAt: nostr.Now(), Content: "from author 1"}
	event1.Sign(secret1)

	event2 := nostr.Event{Kind: nostr.KindTextNote, CreatedAt: nostr.Now(), Content: "from author 2"}
	event2.Sign(secret2)

	store.SaveEvent(event1)
	store.SaveEvent(event2)

	// Query by specific author
	filter := nostr.Filter{Authors: []nostr.PubKey{secret1.Public()}}
	events := make([]nostr.Event, 0)
	for evt := range store.QueryEvents(filter, 0) {
		events = append(events, evt)
	}

	if len(events) != 1 {
		t.Errorf("QueryEvents() by author returned %d events, want 1", len(events))
	}

	if events[0].PubKey != secret1.Public() {
		t.Error("QueryEvents() by author returned wrong author")
	}
}

func TestEventStore_QueryEvents_ByIDs(t *testing.T) {
	store := createTestEventStore()
	store.Init()

	event1 := createTestEvent(nostr.KindTextNote, "first event")
	event2 := createTestEvent(nostr.KindTextNote, "second event")

	store.SaveEvent(event1)
	store.SaveEvent(event2)

	// Query by specific ID
	filter := nostr.Filter{IDs: []nostr.ID{event1.ID}}
	events := make([]nostr.Event, 0)
	for evt := range store.QueryEvents(filter, 0) {
		events = append(events, evt)
	}

	if len(events) != 1 {
		t.Errorf("QueryEvents() by ID returned %d events, want 1", len(events))
	}

	if events[0].ID != event1.ID {
		t.Error("QueryEvents() by ID returned wrong event")
	}
}

func TestEventStore_QueryEvents_ByTags(t *testing.T) {
	store := createTestEventStore()
	store.Init()

	event1 := nostr.Event{
		Kind:      nostr.KindTextNote,
		CreatedAt: nostr.Now(),
		Content:   "tagged event",
		Tags:      nostr.Tags{{"t", "bitcoin"}, {"p", "testuser"}},
	}
	event1.Sign(nostr.Generate())

	event2 := nostr.Event{
		Kind:      nostr.KindTextNote,
		CreatedAt: nostr.Now(),
		Content:   "other event",
		Tags:      nostr.Tags{{"t", "nostr"}},
	}
	event2.Sign(nostr.Generate())

	store.SaveEvent(event1)
	store.SaveEvent(event2)

	// Query by tag
	filter := nostr.Filter{Tags: nostr.TagMap{"t": []string{"bitcoin"}}}
	events := make([]nostr.Event, 0)
	for evt := range store.QueryEvents(filter, 0) {
		events = append(events, evt)
	}

	if len(events) != 1 {
		t.Errorf("QueryEvents() by tags returned %d events, want 1", len(events))
	}

	if events[0].ID != event1.ID {
		t.Error("QueryEvents() by tags returned wrong event")
	}

	// Test that non-single-character tags are ignored
	event3 := nostr.Event{
		Kind:      nostr.KindTextNote,
		CreatedAt: nostr.Now(),
		Content:   "event with multi-char tag",
		Tags:      nostr.Tags{{"title", "special"}, {"t", "ignored"}},
	}
	event3.Sign(nostr.Generate())
	store.SaveEvent(event3)

	// Query by multi-character tag key should ignore the tag filter and return all events
	// (because multi-character tags are skipped in buildSelectQuery)
	filter = nostr.Filter{Tags: nostr.TagMap{"title": []string{"special"}}}
	events = make([]nostr.Event, 0)
	for evt := range store.QueryEvents(filter, 0) {
		events = append(events, evt)
	}

	if len(events) != 3 {
		t.Errorf("QueryEvents() with multi-character tag key should ignore tag filter and return all 3 events, got %d", len(events))
	}
}

func TestEventStore_QueryEvents_TimeRange(t *testing.T) {
	store := createTestEventStore()
	store.Init()

	oldTime := nostr.Timestamp(1000000)
	newTime := nostr.Timestamp(2000000)

	event1 := nostr.Event{Kind: nostr.KindTextNote, CreatedAt: oldTime, Content: "old event"}
	event1.Sign(nostr.Generate())

	event2 := nostr.Event{Kind: nostr.KindTextNote, CreatedAt: newTime, Content: "new event"}
	event2.Sign(nostr.Generate())

	store.SaveEvent(event1)
	store.SaveEvent(event2)

	// Query events since a certain time
	filter := nostr.Filter{Since: oldTime}
	events := make([]nostr.Event, 0)
	for evt := range store.QueryEvents(filter, 0) {
		events = append(events, evt)
	}

	if len(events) != 2 {
		t.Errorf("QueryEvents() with Since returned %d events, want 2", len(events))
	}

	// Query events until a certain time
	filter = nostr.Filter{Until: oldTime}
	events = make([]nostr.Event, 0)
	for evt := range store.QueryEvents(filter, 0) {
		events = append(events, evt)
	}

	if len(events) != 1 {
		t.Errorf("QueryEvents() with Until returned %d events, want 1", len(events))
	}
}

func TestEventStore_QueryEvents_Limit(t *testing.T) {
	store := createTestEventStore()
	store.Init()

	// Save multiple events
	for i := 0; i < 5; i++ {
		event := createTestEvent(nostr.KindTextNote, "event content")
		store.SaveEvent(event)
	}

	// Query with limit
	filter := nostr.Filter{Limit: 3}
	events := make([]nostr.Event, 0)
	for evt := range store.QueryEvents(filter, 0) {
		events = append(events, evt)
	}

	if len(events) != 3 {
		t.Errorf("QueryEvents() with limit returned %d events, want 3", len(events))
	}
}

func TestEventStore_QueryEvents_MaxLimitCapsUnlimitedFilter(t *testing.T) {
	store := createTestEventStore()
	store.Init()

	for i := 0; i < 5; i++ {
		evt := createTestEvent(nostr.KindTextNote, "maxlimit test")
		store.SaveEvent(evt)
	}

	// filter.Limit=0 means "no limit", but maxLimit=2 should cap it
	filter := nostr.Filter{}
	events := make([]nostr.Event, 0)
	for evt := range store.QueryEvents(filter, 2) {
		events = append(events, evt)
	}

	if len(events) != 2 {
		t.Errorf("QueryEvents() with maxLimit=2 and no filter limit returned %d events, want 2", len(events))
	}
}

func TestEventStore_QueryEvents_LimitZero(t *testing.T) {
	store := createTestEventStore()
	store.Init()

	event := createTestEvent(nostr.KindTextNote, "test event")
	store.SaveEvent(event)

	// Query with LimitZero true should return no events
	filter := nostr.Filter{LimitZero: true}
	events := make([]nostr.Event, 0)
	for evt := range store.QueryEvents(filter, 0) {
		events = append(events, evt)
	}

	if len(events) != 0 {
		t.Errorf("QueryEvents() with LimitZero returned %d events, want 0", len(events))
	}
}

func TestEventStore_QueryEvents_Search(t *testing.T) {
	store := createTestEventStore()
	store.Init()

	event1 := createTestEvent(nostr.KindTextNote, "this contains bitcoin")
	event2 := createTestEvent(nostr.KindTextNote, "this contains nostr")

	store.SaveEvent(event1)
	store.SaveEvent(event2)

	// Query by search term
	filter := nostr.Filter{Search: "bitcoin"}
	events := make([]nostr.Event, 0)
	for evt := range store.QueryEvents(filter, 0) {
		events = append(events, evt)
	}

	// Should find at least one event (exact result depends on FTS availability)
	if len(events) == 0 {
		t.Error("QueryEvents() with search should find at least one event")
	}

	// If we found events, make sure they contain the search term
	if len(events) > 0 {
		found := false
		for _, evt := range events {
			if evt.Content == "this contains bitcoin" {
				found = true
				break
			}
		}
		if !found {
			t.Error("QueryEvents() search did not return the expected event")
		}
	}
}

func TestEventStore_DeleteEvent(t *testing.T) {
	store := createTestEventStore()
	store.Init()

	event := createTestEvent(nostr.KindTextNote, "to be deleted")
	store.SaveEvent(event)

	// Verify event exists
	filter := nostr.Filter{IDs: []nostr.ID{event.ID}}
	events := make([]nostr.Event, 0)
	for evt := range store.QueryEvents(filter, 0) {
		events = append(events, evt)
	}

	if len(events) != 1 {
		t.Error("Event should exist before deletion")
	}

	// Delete the event
	err := store.DeleteEvent(event.ID)
	if err != nil {
		t.Errorf("DeleteEvent() error = %v", err)
	}

	// Verify event is deleted
	events = make([]nostr.Event, 0)
	for evt := range store.QueryEvents(filter, 0) {
		events = append(events, evt)
	}

	if len(events) != 0 {
		t.Error("Event should not exist after deletion")
	}
}

func TestEventStore_ReplaceEvent(t *testing.T) {
	store := createTestEventStore()
	store.Init()

	secret := nostr.Generate()

	// Create initial addressable event
	event1 := nostr.Event{
		Kind:      nostr.KindProfileMetadata,
		CreatedAt: nostr.Timestamp(1000000),
		Content:   "initial content",
		Tags:      nostr.Tags{{"d", "profile"}},
	}
	event1.Sign(secret)

	err := store.ReplaceEvent(event1)
	if err != nil {
		t.Errorf("ReplaceEvent() error = %v", err)
	}

	// Create newer event to replace the first
	event2 := nostr.Event{
		Kind:      nostr.KindProfileMetadata,
		CreatedAt: nostr.Timestamp(2000000),
		Content:   "updated content",
		Tags:      nostr.Tags{{"d", "profile"}},
	}
	event2.Sign(secret)

	err = store.ReplaceEvent(event2)
	if err != nil {
		t.Errorf("ReplaceEvent() error = %v", err)
	}

	// Query events - should only have the newer one
	filter := nostr.Filter{Kinds: []nostr.Kind{nostr.KindProfileMetadata}, Authors: []nostr.PubKey{secret.Public()}}
	events := make([]nostr.Event, 0)
	for evt := range store.QueryEvents(filter, 0) {
		events = append(events, evt)
	}

	if len(events) != 1 {
		t.Errorf("ReplaceEvent() should result in 1 event, got %d", len(events))
	}

	if events[0].Content != "updated content" {
		t.Errorf("ReplaceEvent() content = %q, want %q", events[0].Content, "updated content")
	}
}

func TestEventStore_ReplaceEvent_OlderEvent(t *testing.T) {
	store := createTestEventStore()
	store.Init()

	secret := nostr.Generate()

	// Create newer event first
	event1 := nostr.Event{
		Kind:      nostr.KindProfileMetadata,
		CreatedAt: nostr.Timestamp(2000000),
		Content:   "newer content",
		Tags:      nostr.Tags{{"d", "profile"}},
	}
	event1.Sign(secret)

	store.ReplaceEvent(event1)

	// Try to replace with older event - should be ignored
	event2 := nostr.Event{
		Kind:      nostr.KindProfileMetadata,
		CreatedAt: nostr.Timestamp(1000000),
		Content:   "older content",
		Tags:      nostr.Tags{{"d", "profile"}},
	}
	event2.Sign(secret)

	err := store.ReplaceEvent(event2)
	if err != nil {
		t.Errorf("ReplaceEvent() with older event error = %v", err)
	}

	// Verify the newer event is still there
	filter := nostr.Filter{Kinds: []nostr.Kind{nostr.KindProfileMetadata}, Authors: []nostr.PubKey{secret.Public()}}
	events := make([]nostr.Event, 0)
	for evt := range store.QueryEvents(filter, 0) {
		events = append(events, evt)
	}

	if len(events) != 1 {
		t.Errorf("ReplaceEvent() with older event should keep newer event, got %d events", len(events))
	}

	if events[0].Content != "newer content" {
		t.Errorf("ReplaceEvent() with older event kept wrong content = %q, want %q", events[0].Content, "newer content")
	}
}

// TestEventStore_ReplaceEvent_SerializationRetry provokes a real PostgreSQL
// serialization conflict by holding a SERIALIZABLE transaction open while
// ReplaceEvent tries to touch the same rows. The first attempt should fail
// with SQLSTATE 40001 and the retry should succeed.
func TestEventStore_ReplaceEvent_SerializationRetry(t *testing.T) {
	store := createTestEventStore()
	store.Init()

	secret := nostr.Generate()

	// Seed an addressable event so ReplaceEvent has something to read.
	event1 := nostr.Event{
		Kind:      nostr.Kind(30023), // addressable kind
		CreatedAt: nostr.Timestamp(1000000),
		Content:   "original",
		Tags:      nostr.Tags{{"d", "conflict-test"}},
	}
	event1.Sign(secret)
	if err := store.ReplaceEvent(event1); err != nil {
		t.Fatalf("seed ReplaceEvent: %v", err)
	}

	// Open a SERIALIZABLE transaction that reads the same row, creating a
	// rw-dependency that will force the next ReplaceEvent to abort once we commit.
	tx, err := GetDb().BeginTx(context.Background(), &sql.TxOptions{
		Isolation: sql.LevelSerializable,
	})
	if err != nil {
		t.Fatalf("begin blocking tx: %v", err)
	}

	// Read inside the blocking tx to establish the dependency.
	eventsTable := store.Schema.Prefix("events")
	row := tx.QueryRow("SELECT id FROM "+eventsTable+" WHERE kind = $1 LIMIT 1", int(event1.Kind))
	var idStr string
	if err := row.Scan(&idStr); err != nil {
		tx.Rollback()
		t.Fatalf("blocking tx read: %v", err)
	}

	// Write something in the blocking tx to create a real rw-conflict.
	_, err = tx.Exec("UPDATE "+eventsTable+" SET content = 'blocked' WHERE id = $1", idStr)
	if err != nil {
		tx.Rollback()
		t.Fatalf("blocking tx write: %v", err)
	}

	// Launch ReplaceEvent in a goroutine — it will block or conflict.
	done := make(chan error, 1)
	event2 := nostr.Event{
		Kind:      nostr.Kind(30023),
		CreatedAt: nostr.Timestamp(2000000),
		Content:   "replacement",
		Tags:      nostr.Tags{{"d", "conflict-test"}},
	}
	event2.Sign(secret)
	go func() {
		done <- store.ReplaceEvent(event2)
	}()

	// Give the goroutine a moment to start its transaction, then commit ours
	// which forces PostgreSQL to abort the other.
	time.Sleep(50 * time.Millisecond)
	if err := tx.Commit(); err != nil {
		t.Fatalf("blocking tx commit: %v", err)
	}

	// ReplaceEvent should succeed via retry.
	if err := <-done; err != nil {
		t.Fatalf("ReplaceEvent with contention should succeed via retry, got: %v", err)
	}

	// Verify the replacement landed.
	filter := nostr.Filter{Kinds: []nostr.Kind{nostr.Kind(30023)}, Authors: []nostr.PubKey{secret.Public()}}
	var results []nostr.Event
	for evt := range store.QueryEvents(filter, 0) {
		results = append(results, evt)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 event after retry, got %d", len(results))
	}
	if results[0].Content != "replacement" {
		t.Errorf("content = %q, want %q", results[0].Content, "replacement")
	}
}

func TestEventStore_CountEvents(t *testing.T) {
	store := createTestEventStore()
	store.Init()

	// Save events with different kinds
	for i := 0; i < 3; i++ {
		event := createTestEvent(nostr.KindTextNote, "text note")
		store.SaveEvent(event)
	}
	for i := 0; i < 2; i++ {
		event := createTestEvent(nostr.KindProfileMetadata, "profile metadata")
		store.SaveEvent(event)
	}

	// Count all events
	filter := nostr.Filter{}
	count, err := store.CountEvents(filter)
	if err != nil {
		t.Errorf("CountEvents() error = %v", err)
	}

	if count != 5 {
		t.Errorf("CountEvents() = %d, want 5", count)
	}

	// Count by specific kind - should return less than 5
	filter = nostr.Filter{Kinds: []nostr.Kind{nostr.KindTextNote}}
	count, err = store.CountEvents(filter)
	if err != nil {
		t.Errorf("CountEvents() by kind error = %v", err)
	}

	if count != 3 {
		t.Errorf("CountEvents() by kind = %d, want 3", count)
	}

	// Count by another specific kind - should return less than 5
	filter = nostr.Filter{Kinds: []nostr.Kind{nostr.KindProfileMetadata}}
	count, err = store.CountEvents(filter)
	if err != nil {
		t.Errorf("CountEvents() by metadata kind error = %v", err)
	}

	if count != 2 {
		t.Errorf("CountEvents() by metadata kind = %d, want 2", count)
	}
}

func TestEventStore_CountEvents_IgnoresLimit(t *testing.T) {
	store := createTestEventStore()
	store.Init()

	for i := 0; i < 5; i++ {
		evt := createTestEvent(nostr.KindTextNote, "count test")
		store.SaveEvent(evt)
	}

	// CountEvents should return total count even when filter has a Limit
	filter := nostr.Filter{Kinds: []nostr.Kind{nostr.KindTextNote}, Limit: 2}
	count, err := store.CountEvents(filter)
	if err != nil {
		t.Fatalf("CountEvents() error = %v", err)
	}

	if count != 5 {
		t.Errorf("CountEvents() with Limit=2 should return 5 (total), got %d", count)
	}
}

func TestEventStore_SaveEvent_TagsArePersisted(t *testing.T) {
	store := createTestEventStore()
	store.Init()

	evt := nostr.Event{
		Kind:      nostr.KindTextNote,
		CreatedAt: nostr.Now(),
		Content:   "tagged event",
		Tags:      nostr.Tags{{"h", "group1"}, {"p", "user1"}, {"t", "topic"}},
	}
	evt.Sign(nostr.Generate())
	if err := store.SaveEvent(evt); err != nil {
		t.Fatalf("SaveEvent() error = %v", err)
	}

	// Verify each single-letter tag is queryable
	for _, tag := range evt.Tags {
		filter := nostr.Filter{Tags: nostr.TagMap{tag[0]: []string{tag[1]}}}
		var found bool
		for result := range store.QueryEvents(filter, 0) {
			if result.ID == evt.ID {
				found = true
			}
		}
		if !found {
			t.Errorf("Event not found by tag %s=%s", tag[0], tag[1])
		}
	}
}

func TestEventStore_SaveEvent_DuplicateDoesNotCreateOrphanTags(t *testing.T) {
	store := createTestEventStore()
	store.Init()

	evt := nostr.Event{
		Kind:      nostr.KindTextNote,
		CreatedAt: nostr.Now(),
		Content:   "duplicate test",
		Tags:      nostr.Tags{{"t", "dup"}},
	}
	evt.Sign(nostr.Generate())

	// First save should succeed
	err := store.SaveEvent(evt)
	if err != nil {
		t.Fatalf("First SaveEvent() error = %v", err)
	}

	// Second save should return duplicate error
	err = store.SaveEvent(evt)
	if err == nil {
		t.Fatal("Second SaveEvent() should return duplicate error")
	}

	// Query by tag should still return exactly one event
	filter := nostr.Filter{Tags: nostr.TagMap{"t": []string{"dup"}}}
	var count int
	for range store.QueryEvents(filter, 0) {
		count++
	}
	if count != 1 {
		t.Errorf("Expected 1 event for tag query after duplicate save, got %d", count)
	}
}

func TestEventStore_DeleteEvent_CascadesTags(t *testing.T) {
	store := createTestEventStore()
	store.Init()

	evt := nostr.Event{
		Kind:      nostr.KindTextNote,
		CreatedAt: nostr.Now(),
		Content:   "cascade test",
		Tags:      nostr.Tags{{"t", "cascade_test_unique"}},
	}
	evt.Sign(nostr.Generate())
	store.SaveEvent(evt)

	// Verify tag query finds the event
	filter := nostr.Filter{Tags: nostr.TagMap{"t": []string{"cascade_test_unique"}}}
	var count int
	for range store.QueryEvents(filter, 0) {
		count++
	}
	if count != 1 {
		t.Fatalf("Expected 1 event before delete, got %d", count)
	}

	// Delete the event
	if err := store.DeleteEvent(evt.ID); err != nil {
		t.Fatalf("DeleteEvent() error = %v", err)
	}

	// Tag query should return nothing (tags cascaded)
	count = 0
	for range store.QueryEvents(filter, 0) {
		count++
	}
	if count != 0 {
		t.Errorf("Expected 0 events after delete (cascade), got %d", count)
	}
}

func TestEventStore_Close(t *testing.T) {
	store := createTestEventStore()

	// Close should not panic or error
	store.Close()
}

func TestEventStore_GetOrCreateApplicationSpecificData(t *testing.T) {
	store := createTestEventStore()
	store.Init()

	dTag := "test/data"

	// Test creating new data when none exists (unsigned)
	event1 := store.GetOrCreateApplicationSpecificData(dTag)

	if event1.Kind != nostr.KindApplicationSpecificData {
		t.Errorf("GetOrCreateApplicationSpecificData() kind = %v, want %v", event1.Kind, nostr.KindApplicationSpecificData)
	}

	dTagFound := event1.Tags.Find("d")
	if dTagFound == nil || dTagFound[1] != dTag {
		t.Errorf("GetOrCreateApplicationSpecificData() d tag = %v, want %v", dTagFound, dTag)
	}

	// Sign and store the event
	store.SignAndStoreEvent(&event1, false)

	// Test retrieving existing data
	event2 := store.GetOrCreateApplicationSpecificData(dTag)

	if event1.ID != event2.ID {
		t.Error("GetOrCreateApplicationSpecificData() should return same event when called again")
	}

	if event2.PubKey != store.Config.GetSelf() {
		t.Error("Retrieved event should be signed by config secret")
	}

	// Test with different d tag creates new event
	event3 := store.GetOrCreateApplicationSpecificData("other/data")

	if event1.ID == event3.ID {
		t.Error("GetOrCreateApplicationSpecificData() should create different event for different d tag")
	}
}
