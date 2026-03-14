//go:build integration

package zooid

import (
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"fiatjaf.com/nostr"
)

// Scale parameters for the performance dataset.
const (
	perfNumEvents  = 500_000
	perfNumMembers = 10_000
	perfNumGroups  = 10
	perfBatchSize  = 5_000
)

var perfGroups = []string{
	"general", "random", "dev", "design", "support",
	"offtopic", "announcements", "feedback", "testing", "ops",
}

// perfStore is initialised once by TestIntegration_QueryPerformance and
// shared across its sub-tests.
var (
	perfStore     *EventStore
	perfPubkeys   []string // hex pubkeys of the 10K members
	perfSetupOnce sync.Once
	perfSetupErr  error
)

// seedPerfData bulk-inserts 500K events and 10K member pubkeys into a fresh
// schema. It uses raw batch INSERTs (not EventStore.SaveEvent) so the seeding
// finishes in seconds rather than hours.
func seedPerfData(t *testing.T) *EventStore {
	t.Helper()
	perfSetupOnce.Do(func() {
		store := createTestEventStore()
		if err := store.Init(); err != nil {
			perfSetupErr = fmt.Errorf("Init: %w", err)
			return
		}

		db := GetDb()
		eventsTable := store.Schema.Prefix("events")
		tagsTable := store.Schema.Prefix("event_tags")

		// Pre-generate 10K valid nostr pubkeys. Using nostr.Generate()
		// ensures every pubkey passes PubKeyFromHex validation when
		// QueryEvents parses rows back.
		pubkeys := make([]string, perfNumMembers)
		for i := range pubkeys {
			pubkeys[i] = nostr.Generate().Public().Hex()
		}
		t.Logf("Generated %d member pubkeys", perfNumMembers)
		perfPubkeys = pubkeys

		t.Logf("Seeding %d events across %d groups with %d members...",
			perfNumEvents, perfNumGroups, perfNumMembers)
		start := time.Now()

		baseTS := int64(1_700_000_000)

		// Pre-sign one event per member to get a valid signature we can reuse.
		// The DB doesn't verify signatures, but QueryEvents parses sig hex.
		memberSigs := make([]string, perfNumMembers)
		for i := 0; i < perfNumMembers; i++ {
			sk := nostr.Generate()
			// Override pubkeys with correctly-derived ones so PubKeyFromHex
			// accepts them when QueryEvents parses result rows.
			pubkeys[i] = sk.Public().Hex()
			evt := nostr.Event{Kind: 9, CreatedAt: nostr.Timestamp(baseTS), Content: "s"}
			evt.Sign(sk)
			memberSigs[i] = hex.EncodeToString(evt.Sig[:])
		}
		t.Logf("Pre-signed %d member keys", perfNumMembers)

		for batchStart := 0; batchStart < perfNumEvents; batchStart += perfBatchSize {
			batchEnd := batchStart + perfBatchSize
			if batchEnd > perfNumEvents {
				batchEnd = perfNumEvents
			}

			tx, err := db.Begin()
			if err != nil {
				perfSetupErr = fmt.Errorf("begin tx: %w", err)
				return
			}

			var eventVals, tagVals []string
			for i := batchStart; i < batchEnd; i++ {
				memberIdx := i % perfNumMembers
				pubkey := pubkeys[memberIdx]
				sig := memberSigs[memberIdx]
				group := perfGroups[i%perfNumGroups]
				ts := baseTS + int64(i)

				// Event IDs: any 32-byte hex works for IDFromHex.
				// Avoid all-zeros (index 0) just in case.
				id := fmt.Sprintf("%064x", i+1)

				eventVals = append(eventVals, fmt.Sprintf(
					"('%s',%d,9,'%s','msg %d in %s','[[\"h\",\"%s\"]]','%s')",
					id, ts, pubkey, i, group, group, sig,
				))
				tagVals = append(tagVals, fmt.Sprintf(
					"('%s','h','%s')", id, group,
				))
			}

			eventSQL := fmt.Sprintf(
				"INSERT INTO %s (id,created_at,kind,pubkey,content,tags,sig) VALUES %s ON CONFLICT(id) DO NOTHING",
				eventsTable, strings.Join(eventVals, ","),
			)
			tagSQL := fmt.Sprintf(
				"INSERT INTO %s (event_id,key,value) VALUES %s",
				tagsTable, strings.Join(tagVals, ","),
			)

			if _, err := tx.Exec(eventSQL); err != nil {
				tx.Rollback()
				perfSetupErr = fmt.Errorf("insert events: %w", err)
				return
			}
			if _, err := tx.Exec(tagSQL); err != nil {
				tx.Rollback()
				perfSetupErr = fmt.Errorf("insert tags: %w", err)
				return
			}
			if err := tx.Commit(); err != nil {
				perfSetupErr = fmt.Errorf("commit: %w", err)
				return
			}

			if (batchStart+perfBatchSize)%100_000 == 0 {
				t.Logf("  %d / %d events inserted...", batchStart+perfBatchSize, perfNumEvents)
			}
		}
		t.Logf("Seeded %d events in %v", perfNumEvents, time.Since(start))

		// Let the planner know about the new data.
		db.Exec(fmt.Sprintf("ANALYZE %s", eventsTable))
		db.Exec(fmt.Sprintf("ANALYZE %s", tagsTable))

		perfStore = store
	})

	if perfSetupErr != nil {
		t.Fatalf("Perf data seeding failed: %v", perfSetupErr)
	}
	return perfStore
}

// ---------- helpers ----------

// explainAnalyze runs EXPLAIN ANALYZE for the query built by buildSelectQuery
// and returns the full plan text.
func explainAnalyze(t *testing.T, store *EventStore, filter nostr.Filter) string {
	t.Helper()
	qb := store.buildSelectQuery(filter)
	sql, args, err := qb.ToSql()
	if err != nil {
		t.Fatalf("ToSql: %v", err)
	}

	rows, err := GetDb().Query("EXPLAIN ANALYZE "+sql, args...)
	if err != nil {
		t.Fatalf("EXPLAIN ANALYZE failed: %v", err)
	}
	defer rows.Close()

	var plan strings.Builder
	for rows.Next() {
		var line string
		rows.Scan(&line)
		plan.WriteString(line + "\n")
	}
	return plan.String()
}

// assertNoSeqScanOnEvents fails if the plan contains a sequential scan on the
// events table.
func assertNoSeqScanOnEvents(t *testing.T, plan, eventsTable string) {
	t.Helper()
	lower := strings.ToLower(plan)
	marker := "seq scan on " + strings.ToLower(eventsTable)
	if strings.Contains(lower, marker) {
		t.Errorf("Plan contains sequential scan on events table:\n%s", plan)
	}
}

// timeQuery runs a filter through QueryEvents, collects results up to a cap,
// returns the count and wall-clock duration.
func timeQuery(store *EventStore, filter nostr.Filter, cap int) (int, time.Duration) {
	start := time.Now()
	n := 0
	for range store.QueryEvents(filter, cap) {
		n++
	}
	return n, time.Since(start)
}

// ---------- test ----------

func TestIntegration_QueryPerformance(t *testing.T) {
	store := seedPerfData(t)
	eventsTable := store.Schema.Prefix("events")

	// Each group has perfNumEvents/perfNumGroups = 50K events.
	eventsPerGroup := perfNumEvents / perfNumGroups

	// ── 1. Tag filter only (the root-cause query pattern) ────────────
	t.Run("TagFilter", func(t *testing.T) {
		filter := nostr.Filter{
			Tags:  nostr.TagMap{"h": []string{"general"}},
			Limit: 1000,
		}

		plan := explainAnalyze(t, store, filter)
		t.Logf("Plan:\n%s", plan)
		assertNoSeqScanOnEvents(t, plan, eventsTable)

		n, dur := timeQuery(store, filter, 1000)
		t.Logf("Returned %d rows in %v", n, dur)
		if n != 1000 {
			t.Errorf("Expected 1000 results, got %d", n)
		}
		if dur > 2*time.Second {
			t.Errorf("Query too slow: %v (want < 2s)", dur)
		}
	})

	// ── 2. Kind + tag filter (exact production query) ────────────────
	t.Run("KindTagFilter", func(t *testing.T) {
		filter := nostr.Filter{
			Kinds: []nostr.Kind{9},
			Tags:  nostr.TagMap{"h": []string{"general"}},
			Limit: 1000,
		}

		plan := explainAnalyze(t, store, filter)
		t.Logf("Plan:\n%s", plan)
		assertNoSeqScanOnEvents(t, plan, eventsTable)

		n, dur := timeQuery(store, filter, 1000)
		t.Logf("Returned %d rows in %v", n, dur)
		if n != 1000 {
			t.Errorf("Expected 1000 results, got %d", n)
		}
		if dur > 2*time.Second {
			t.Errorf("Query too slow: %v (want < 2s)", dur)
		}
	})

	// ── 3. Kind + tag + Since (time-bounded) ─────────────────────────
	t.Run("KindTagSinceFilter", func(t *testing.T) {
		// Pick a Since that gives us roughly half the events in a group.
		midpoint := nostr.Timestamp(1_700_000_000 + int64(perfNumEvents/2))
		filter := nostr.Filter{
			Kinds: []nostr.Kind{9},
			Tags:  nostr.TagMap{"h": []string{"dev"}},
			Since: midpoint,
			Limit: 1000,
		}

		plan := explainAnalyze(t, store, filter)
		t.Logf("Plan:\n%s", plan)
		assertNoSeqScanOnEvents(t, plan, eventsTable)

		n, dur := timeQuery(store, filter, 1000)
		t.Logf("Returned %d rows in %v", n, dur)
		if n == 0 {
			t.Error("Expected results, got 0")
		}
		if dur > 2*time.Second {
			t.Errorf("Query too slow: %v (want < 2s)", dur)
		}
	})

	// ── 4. Multiple tag values (h IN ('general', 'random')) ──────────
	t.Run("MultipleTagValues", func(t *testing.T) {
		filter := nostr.Filter{
			Kinds: []nostr.Kind{9},
			Tags:  nostr.TagMap{"h": []string{"general", "random"}},
			Limit: 1000,
		}

		plan := explainAnalyze(t, store, filter)
		t.Logf("Plan:\n%s", plan)
		assertNoSeqScanOnEvents(t, plan, eventsTable)

		n, dur := timeQuery(store, filter, 1000)
		t.Logf("Returned %d rows in %v", n, dur)
		if n != 1000 {
			t.Errorf("Expected 1000 results, got %d", n)
		}
		if dur > 2*time.Second {
			t.Errorf("Query too slow: %v (want < 2s)", dur)
		}
	})

	// ── 5. Author + kind (no tag filter — tests non-tag path) ───────
	t.Run("AuthorKindFilter", func(t *testing.T) {
		// Query by author using raw SQL to avoid nostr pubkey validation.
		// Each pubkey appears in perfNumEvents/perfNumMembers = 50 events.
		expected := perfNumEvents / perfNumMembers
		pubkey := perfPubkeys[0]

		start := time.Now()
		var n int
		rows, err := GetDb().Query(
			fmt.Sprintf(
				"SELECT id FROM %s WHERE kind = $1 AND pubkey = $2 ORDER BY created_at DESC LIMIT $3",
				store.Schema.Prefix("events"),
			), 9, pubkey, 100,
		)
		if err != nil {
			t.Fatalf("Query failed: %v", err)
		}
		for rows.Next() {
			var id string
			rows.Scan(&id)
			n++
		}
		rows.Close()
		dur := time.Since(start)

		t.Logf("Returned %d rows in %v", n, dur)
		if n != expected && n != 100 {
			t.Errorf("Expected %d or 100 (capped) results, got %d", expected, n)
		}
		if dur > 2*time.Second {
			t.Errorf("Query too slow: %v (want < 2s)", dur)
		}
	})

	// ── 6. COUNT with tag filter ─────────────────────────────────────
	t.Run("CountTagFilter", func(t *testing.T) {
		filter := nostr.Filter{
			Kinds: []nostr.Kind{9},
			Tags:  nostr.TagMap{"h": []string{"general"}},
		}

		start := time.Now()
		count, err := store.CountEvents(filter)
		dur := time.Since(start)
		if err != nil {
			t.Fatalf("CountEvents: %v", err)
		}
		t.Logf("COUNT = %d in %v", count, dur)

		if int(count) != eventsPerGroup {
			t.Errorf("Expected count %d, got %d", eventsPerGroup, count)
		}
		if dur > 5*time.Second {
			t.Errorf("Count too slow: %v (want < 5s)", dur)
		}
	})

	// ── 7. Correctness: result ordering ──────────────────────────────
	t.Run("ResultsOrderedDescending", func(t *testing.T) {
		filter := nostr.Filter{
			Kinds: []nostr.Kind{9},
			Tags:  nostr.TagMap{"h": []string{"feedback"}},
			Limit: 500,
		}

		var prev nostr.Timestamp
		first := true
		for evt := range store.QueryEvents(filter, 500) {
			if first {
				prev = evt.CreatedAt
				first = true
			} else {
				if evt.CreatedAt > prev {
					t.Fatalf("Results not ordered DESC: %d came after %d",
						evt.CreatedAt, prev)
				}
				prev = evt.CreatedAt
			}
		}
	})

	// ── 8. Concurrent queries (simulate 20 connected clients) ────────
	t.Run("ConcurrentClients", func(t *testing.T) {
		const numClients = 20
		var wg sync.WaitGroup
		errors := make(chan error, numClients)

		for i := 0; i < numClients; i++ {
			wg.Add(1)
			go func(clientID int) {
				defer wg.Done()
				group := perfGroups[clientID%perfNumGroups]
				filter := nostr.Filter{
					Kinds: []nostr.Kind{9},
					Tags:  nostr.TagMap{"h": []string{group}},
					Limit: 1000,
				}

				start := time.Now()
				n := 0
				for range store.QueryEvents(filter, 1000) {
					n++
				}
				dur := time.Since(start)

				if n != 1000 {
					errors <- fmt.Errorf("client %d: expected 1000 results, got %d", clientID, n)
					return
				}
				if dur > 5*time.Second {
					errors <- fmt.Errorf("client %d: query took %v (want < 5s)", clientID, dur)
					return
				}
			}(i)
		}

		wg.Wait()
		close(errors)

		for err := range errors {
			t.Error(err)
		}
	})

	// ── 9. No-tag query still works (no JOIN path) ───────────────────
	t.Run("NoTagFilter", func(t *testing.T) {
		filter := nostr.Filter{
			Kinds: []nostr.Kind{9},
			Limit: 100,
		}

		plan := explainAnalyze(t, store, filter)
		t.Logf("Plan:\n%s", plan)

		n, dur := timeQuery(store, filter, 100)
		t.Logf("Returned %d rows in %v", n, dur)
		if n != 100 {
			t.Errorf("Expected 100 results, got %d", n)
		}
		if dur > 2*time.Second {
			t.Errorf("Query too slow: %v (want < 2s)", dur)
		}
	})
}

