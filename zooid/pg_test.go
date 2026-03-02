package zooid

import (
	"testing"

	"fiatjaf.com/nostr"
)

// PostgreSQL-specific test cases

func TestFTS_TsvectorSearch(t *testing.T) {
	store := createTestEventStore()
	store.Init()

	// Create events with varied content
	events := []struct {
		content string
	}{
		{"The quick brown fox jumps over the lazy dog"},
		{"Bitcoin is a decentralized digital currency"},
		{"Nostr is a simple protocol for decentralized social networking"},
		{"PostgreSQL supports full-text search with tsvector"},
		{""},
	}

	for _, e := range events {
		evt := createTestEvent(nostr.KindTextNote, e.content)
		store.SaveEvent(evt)
	}

	tests := []struct {
		name     string
		search   string
		minCount int
	}{
		{"find bitcoin", "bitcoin", 1},
		{"find decentralized", "decentralized", 2},
		{"find fox", "fox", 1},
		{"find postgresql", "postgresql", 1},
		{"stemming - search 'jumps' finds 'jump'", "jump", 1},
		{"no results", "xyznonexistent", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filter := nostr.Filter{Search: tt.search}
			var results []nostr.Event
			for evt := range store.QueryEvents(filter, 0) {
				results = append(results, evt)
			}

			if len(results) < tt.minCount {
				t.Errorf("search %q: got %d results, want at least %d", tt.search, len(results), tt.minCount)
			}
		})
	}
}

func TestBIGINT_LargeTimestamps(t *testing.T) {
	store := createTestEventStore()
	store.Init()

	// Test with timestamps that would overflow 32-bit INTEGER
	// Unix timestamp for year 2040: ~2208988800
	largeTimestamp := nostr.Timestamp(2208988800)

	evt := nostr.Event{
		Kind:      nostr.KindTextNote,
		CreatedAt: largeTimestamp,
		Content:   "event from the future",
	}
	evt.Sign(nostr.Generate())

	err := store.SaveEvent(evt)
	if err != nil {
		t.Fatalf("SaveEvent with large timestamp failed: %v", err)
	}

	// Query it back
	filter := nostr.Filter{IDs: []nostr.ID{evt.ID}}
	var found bool
	for result := range store.QueryEvents(filter, 0) {
		if result.CreatedAt != largeTimestamp {
			t.Errorf("created_at = %d, want %d", result.CreatedAt, largeTimestamp)
		}
		found = true
	}

	if !found {
		t.Error("Event with large timestamp not found")
	}

	// Test that Since/Until work with large timestamps
	since := nostr.Timestamp(2208988799)
	filter = nostr.Filter{Since: since}
	var count int
	for range store.QueryEvents(filter, 0) {
		count++
	}
	if count == 0 {
		t.Error("Since filter with large timestamp should find events")
	}
}

func TestSchemaInit_Idempotency(t *testing.T) {
	store := createTestEventStore()

	// Init once
	if err := store.Init(); err != nil {
		t.Fatalf("First Init() failed: %v", err)
	}

	// Init again - should not error (CREATE TABLE IF NOT EXISTS, etc.)
	if err := store.Init(); err != nil {
		t.Fatalf("Second Init() failed: %v (schema init should be idempotent)", err)
	}

	// Init a third time
	if err := store.Init(); err != nil {
		t.Fatalf("Third Init() failed: %v (schema init should be idempotent)", err)
	}

	// Verify the store works after multiple inits
	evt := createTestEvent(nostr.KindTextNote, "after multiple inits")
	if err := store.SaveEvent(evt); err != nil {
		t.Fatalf("SaveEvent after multiple inits failed: %v", err)
	}

	filter := nostr.Filter{IDs: []nostr.ID{evt.ID}}
	var found bool
	for range store.QueryEvents(filter, 0) {
		found = true
	}
	if !found {
		t.Error("Event not found after multiple schema inits")
	}
}

func TestConcurrentSaveAndQuery(t *testing.T) {
	store := createTestEventStore()
	store.Init()

	// Pre-populate with some events
	for i := 0; i < 10; i++ {
		evt := createTestEvent(nostr.KindTextNote, "concurrent test event")
		store.SaveEvent(evt)
	}

	// Run concurrent saves and queries
	done := make(chan bool, 20)

	// 10 concurrent writers
	for i := 0; i < 10; i++ {
		go func() {
			evt := createTestEvent(nostr.KindTextNote, "concurrent write")
			store.SaveEvent(evt)
			done <- true
		}()
	}

	// 10 concurrent readers
	for i := 0; i < 10; i++ {
		go func() {
			filter := nostr.Filter{Kinds: []nostr.Kind{nostr.KindTextNote}}
			for range store.QueryEvents(filter, 0) {
				// drain
			}
			done <- true
		}()
	}

	// Wait for all goroutines
	for i := 0; i < 20; i++ {
		<-done
	}

	// Verify we can still read
	filter := nostr.Filter{}
	var count int
	for range store.QueryEvents(filter, 0) {
		count++
	}

	if count < 10 {
		t.Errorf("Expected at least 10 events after concurrent access, got %d", count)
	}
}

func TestFTS_EmptyContent(t *testing.T) {
	store := createTestEventStore()
	store.Init()

	// Events with empty content should not cause errors
	evt := createTestEvent(nostr.KindTextNote, "")
	if err := store.SaveEvent(evt); err != nil {
		t.Fatalf("SaveEvent with empty content failed: %v", err)
	}

	// Should be queryable
	filter := nostr.Filter{IDs: []nostr.ID{evt.ID}}
	var found bool
	for range store.QueryEvents(filter, 0) {
		found = true
	}
	if !found {
		t.Error("Event with empty content not found")
	}
}

func TestDollarPlaceholders_MultipleTagFilters(t *testing.T) {
	store := createTestEventStore()
	store.Init()

	// Create events with specific tags
	evt1 := nostr.Event{
		Kind:      nostr.KindTextNote,
		CreatedAt: nostr.Now(),
		Content:   "tagged event",
		Tags:      nostr.Tags{{"h", "group1"}, {"p", "user1"}},
	}
	evt1.Sign(nostr.Generate())
	store.SaveEvent(evt1)

	evt2 := nostr.Event{
		Kind:      nostr.KindTextNote,
		CreatedAt: nostr.Now(),
		Content:   "other event",
		Tags:      nostr.Tags{{"h", "group1"}, {"p", "user2"}},
	}
	evt2.Sign(nostr.Generate())
	store.SaveEvent(evt2)

	evt3 := nostr.Event{
		Kind:      nostr.KindTextNote,
		CreatedAt: nostr.Now(),
		Content:   "different group",
		Tags:      nostr.Tags{{"h", "group2"}, {"p", "user1"}},
	}
	evt3.Sign(nostr.Generate())
	store.SaveEvent(evt3)

	// Query with multiple tag filters - this exercises Dollar placeholder numbering
	filter := nostr.Filter{
		Tags: nostr.TagMap{
			"h": []string{"group1"},
			"p": []string{"user1"},
		},
	}

	var results []nostr.Event
	for evt := range store.QueryEvents(filter, 0) {
		results = append(results, evt)
	}

	if len(results) != 1 {
		t.Errorf("Multiple tag filter: got %d results, want 1", len(results))
	}

	if len(results) > 0 && results[0].ID != evt1.ID {
		t.Error("Multiple tag filter returned wrong event")
	}
}

func TestDollarPlaceholders_KindAndTagFilter(t *testing.T) {
	store := createTestEventStore()
	store.Init()

	secret := nostr.Generate()

	// Create events of different kinds with same tag
	evt1 := nostr.Event{
		Kind:      nostr.Kind(9),
		CreatedAt: nostr.Now(),
		Content:   "chat message",
		Tags:      nostr.Tags{{"h", "mygroup"}},
	}
	evt1.Sign(secret)
	store.SaveEvent(evt1)

	evt2 := nostr.Event{
		Kind:      nostr.Kind(39000),
		CreatedAt: nostr.Now(),
		Content:   "metadata",
		Tags:      nostr.Tags{{"d", "mygroup"}},
	}
	evt2.Sign(secret)
	store.SaveEvent(evt2)

	// Query with kind + tag + author - exercises multiple Dollar placeholders
	filter := nostr.Filter{
		Kinds:   []nostr.Kind{nostr.Kind(9)},
		Authors: []nostr.PubKey{secret.Public()},
		Tags:    nostr.TagMap{"h": []string{"mygroup"}},
	}

	var results []nostr.Event
	for evt := range store.QueryEvents(filter, 0) {
		results = append(results, evt)
	}

	if len(results) != 1 {
		t.Errorf("Kind+Tag+Author filter: got %d results, want 1", len(results))
	}
}
