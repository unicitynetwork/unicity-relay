package zooid

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/khatru"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
)

func createMetricsTestInstance(t *testing.T) *Instance {
	t.Helper()
	config := &Config{
		Host:   "test.com",
		Schema: "test_metrics_" + RandomString(8),
		secret: nostr.Generate(),
	}
	config.Groups.Enabled = true
	schema := &Schema{Name: config.Schema}
	relay := khatru.NewRelay()
	events := &EventStore{
		Relay:   relay,
		Config:  config,
		Schema:  schema,
		rootCtx: context.Background(),
	}
	if err := events.Init(); err != nil {
		t.Fatalf("events.Init failed: %v", err)
	}

	mgmt := &ManagementStore{
		Config: config,
		Events: events,
	}

	groups := &GroupStore{
		Config:     config,
		Events:     events,
		Management: mgmt,
	}

	return &Instance{
		Relay:      relay,
		Config:     config,
		Events:     events,
		Management: mgmt,
		Groups:     groups,
	}
}

func withTestInstance(t *testing.T, inst *Instance, fn func()) {
	t.Helper()
	instancesMux.Lock()
	oldByName := instancesByName
	oldByHost := instancesByHost
	instancesByName = map[string]*Instance{"test": inst}
	instancesByHost = map[string]*Instance{"test.com": inst}
	instancesMux.Unlock()
	defer func() {
		instancesMux.Lock()
		instancesByName = oldByName
		instancesByHost = oldByHost
		instancesMux.Unlock()
	}()
	fn()
}

func TestMetrics_CacheGauges(t *testing.T) {
	inst := createMetricsTestInstance(t)
	label := inst.Config.Schema

	// Populate group metadata cache: 3 groups — 2 private, 1 hidden, 1 closed
	// group-a: private+hidden, group-b: private+closed, group-c: public
	inst.Groups.metadataCache.Store("group-a", &groupMetaCache{
		found:   true,
		private: true,
		hidden:  true,
		closed:  false,
	})
	inst.Groups.metadataCache.Store("group-b", &groupMetaCache{
		found:   true,
		private: true,
		hidden:  false,
		closed:  true,
	})
	inst.Groups.metadataCache.Store("group-c", &groupMetaCache{
		found:   true,
		private: false,
		hidden:  false,
		closed:  false,
	})

	// Populate membership cache
	pk1 := nostr.Generate().Public()
	pk2 := nostr.Generate().Public()
	pk3 := nostr.Generate().Public()

	msA := &memberSet{members: map[nostr.PubKey]struct{}{pk1: {}, pk2: {}}}
	msB := &memberSet{members: map[nostr.PubKey]struct{}{pk2: {}, pk3: {}}}
	msC := &memberSet{members: map[nostr.PubKey]struct{}{pk1: {}}}
	inst.Groups.membershipCache.Store("group-a", msA)
	inst.Groups.membershipCache.Store("group-b", msB)
	inst.Groups.membershipCache.Store("group-c", msC)

	// Populate relay members
	inst.Management.relayMembers.Store(pk1, struct{}{})
	inst.Management.relayMembers.Store(pk2, struct{}{})
	inst.Management.relayMembers.Store(pk3, struct{}{})

	// Populate banned pubkeys
	inst.Management.bannedPubkeys.Store(nostr.Generate().Public(), "spam")

	// Populate banned events (key must be nostr.ID, not nostr.PubKey)
	fakeEvt := nostr.Event{Kind: 1, CreatedAt: nostr.Now(), Content: "x"}
	fakeEvt.Sign(nostr.Generate())
	inst.Management.bannedEvents.Store(fakeEvt.ID, "abuse")

	withTestInstance(t, inst, func() {
		collectMetrics(context.Background())
	})

	// Group counts
	if v := testutil.ToFloat64(groupsTotal.WithLabelValues(label)); v != 3 {
		t.Errorf("groupsTotal = %v, want 3", v)
	}
	if v := testutil.ToFloat64(groupsPrivate.WithLabelValues(label)); v != 2 {
		t.Errorf("groupsPrivate = %v, want 2", v)
	}
	if v := testutil.ToFloat64(groupsHidden.WithLabelValues(label)); v != 1 {
		t.Errorf("groupsHidden = %v, want 1", v)
	}
	if v := testutil.ToFloat64(groupsClosed.WithLabelValues(label)); v != 1 {
		t.Errorf("groupsClosed = %v, want 1", v)
	}

	// Per-group members: only group-c should be reported (group-a is private+hidden, group-b is private)
	if v := testutil.ToFloat64(groupMembers.WithLabelValues(label, "group-c")); v != 1 {
		t.Errorf("groupMembers[group-c] = %v, want 1", v)
	}

	// Private/hidden groups should NOT have per-group metrics
	// (WithLabelValues creates the series with 0, so we check it stays 0)
	if v := testutil.ToFloat64(groupMembers.WithLabelValues(label, "group-a")); v != 0 {
		t.Errorf("groupMembers[group-a] = %v, want 0 (private+hidden)", v)
	}
	if v := testutil.ToFloat64(groupMembers.WithLabelValues(label, "group-b")); v != 0 {
		t.Errorf("groupMembers[group-b] = %v, want 0 (private)", v)
	}

	// Total group members counts distinct pubkeys across all groups (including private)
	// pk1 in group-a+c, pk2 in group-a+b, pk3 in group-b → 3 distinct
	if v := testutil.ToFloat64(groupMembersTotal.WithLabelValues(label)); v != 3 {
		t.Errorf("groupMembersTotal = %v, want 3", v)
	}

	// Relay members
	if v := testutil.ToFloat64(relayMembersTotal.WithLabelValues(label)); v != 3 {
		t.Errorf("relayMembersTotal = %v, want 3", v)
	}

	// Banned pubkeys
	if v := testutil.ToFloat64(bannedPubkeysTotal.WithLabelValues(label)); v != 1 {
		t.Errorf("bannedPubkeysTotal = %v, want 1", v)
	}

	// Banned events
	if v := testutil.ToFloat64(bannedEventsTotal.WithLabelValues(label)); v != 1 {
		t.Errorf("bannedEventsTotal = %v, want 1", v)
	}
}

func TestMetrics_DBGauges(t *testing.T) {
	inst := createMetricsTestInstance(t)
	label := inst.Config.Schema

	// Register groups in metadata cache: g1 public, g2 public, g3 private
	inst.Groups.metadataCache.Store("g1", &groupMetaCache{found: true})
	inst.Groups.metadataCache.Store("g2", &groupMetaCache{found: true})
	inst.Groups.metadataCache.Store("g3", &groupMetaCache{found: true, private: true})

	// Store events: 2 chat messages in g1, 1 in g2, 1 in g3 (private), 1 non-chat
	for _, evt := range []nostr.Event{
		{Kind: 9, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"h", "g1"}}, Content: "hello"},
		{Kind: 9, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"h", "g1"}}, Content: "world"},
		{Kind: 9, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"h", "g2"}}, Content: "hi"},
		{Kind: 9, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"h", "g3"}}, Content: "secret"},
		{Kind: 1, CreatedAt: nostr.Now(), Content: "note"},
	} {
		if err := inst.Events.SignAndStoreEvent(&evt, false); err != nil {
			t.Fatalf("SignAndStoreEvent failed: %v", err)
		}
	}

	// ANALYZE to update reltuples
	eventsTable := inst.Events.Schema.Prefix("events")
	if _, err := GetDb().Exec(fmt.Sprintf("ANALYZE %s", eventsTable)); err != nil {
		t.Fatalf("ANALYZE failed: %v", err)
	}

	withTestInstance(t, inst, func() {
		collectMetrics(context.Background())
		collectGroupMessages(context.Background())
	})

	// eventsTotal uses reltuples estimate — after ANALYZE it should be accurate
	total := testutil.ToFloat64(eventsTotal.WithLabelValues(label))
	if total < 5 {
		t.Errorf("eventsTotal = %v, want >= 5", total)
	}

	// messagesTotal should be 4 (all kind-9 events including private group)
	if v := testutil.ToFloat64(messagesTotal.WithLabelValues(label)); v != 4 {
		t.Errorf("messagesTotal = %v, want 4", v)
	}

	// Per-group messages: g1=2, g2=1, g3 not reported (private)
	if v := testutil.ToFloat64(groupMessages.WithLabelValues(label, "g1")); v != 2 {
		t.Errorf("groupMessages[g1] = %v, want 2", v)
	}
	if v := testutil.ToFloat64(groupMessages.WithLabelValues(label, "g2")); v != 1 {
		t.Errorf("groupMessages[g2] = %v, want 1", v)
	}
	if v := testutil.ToFloat64(groupMessages.WithLabelValues(label, "g3")); v != 0 {
		t.Errorf("groupMessages[g3] = %v, want 0 (private)", v)
	}
}

func TestMetrics_GroupMembersCap(t *testing.T) {
	inst := createMetricsTestInstance(t)
	label := inst.Config.Schema

	// Create 1005 public groups with 1 member each
	pk := nostr.Generate().Public()
	for i := 0; i < 1005; i++ {
		h := RandomString(10)
		inst.Groups.metadataCache.Store(h, &groupMetaCache{found: true})
		ms := &memberSet{members: map[nostr.PubKey]struct{}{pk: {}}}
		inst.Groups.membershipCache.Store(h, ms)
	}

	withTestInstance(t, inst, func() {
		collectMetrics(context.Background())
	})

	// groupsTracked should be capped at 1000
	tracked := testutil.ToFloat64(groupsTracked.WithLabelValues(label))
	if tracked != 1000 {
		t.Errorf("groupsTracked = %v, want 1000", tracked)
	}

	// groupMembersTotal counts distinct pubkeys — all 1005 groups share the same pk
	total := testutil.ToFloat64(groupMembersTotal.WithLabelValues(label))
	if total != 1 {
		t.Errorf("groupMembersTotal = %v, want 1", total)
	}
}

func TestMetrics_StaleGroupsCleared(t *testing.T) {
	inst := createMetricsTestInstance(t)
	label := inst.Config.Schema

	// First collection: group exists (public, so it gets per-group metric)
	inst.Groups.metadataCache.Store("old-group", &groupMetaCache{found: true})
	inst.Groups.membershipCache.Store("old-group", &memberSet{
		members: map[nostr.PubKey]struct{}{nostr.Generate().Public(): {}},
	})

	withTestInstance(t, inst, func() {
		collectMetrics(context.Background())
	})

	if v := testutil.ToFloat64(groupMembers.WithLabelValues(label, "old-group")); v != 1 {
		t.Errorf("groupMembers[old-group] = %v, want 1", v)
	}

	// Delete the group from cache, re-collect
	inst.Groups.membershipCache.Delete("old-group")
	inst.Groups.metadataCache.Delete("old-group")
	withTestInstance(t, inst, func() {
		collectMetrics(context.Background())
	})

	// After DeletePartialMatch, the stale gauge should be 0
	if v := testutil.ToFloat64(groupMembers.WithLabelValues(label, "old-group")); v != 0 {
		t.Errorf("groupMembers[old-group] after delete = %v, want 0", v)
	}
}

func TestMetrics_PrivateGroupsNotExposed(t *testing.T) {
	inst := createMetricsTestInstance(t)
	label := inst.Config.Schema

	pk := nostr.Generate().Public()

	inst.Groups.metadataCache.Store("secret-group", &groupMetaCache{found: true, private: true})
	inst.Groups.membershipCache.Store("secret-group", &memberSet{
		members: map[nostr.PubKey]struct{}{pk: {}},
	})
	inst.Groups.metadataCache.Store("hidden-group", &groupMetaCache{found: true, hidden: true})
	inst.Groups.membershipCache.Store("hidden-group", &memberSet{
		members: map[nostr.PubKey]struct{}{pk: {}},
	})
	inst.Groups.metadataCache.Store("public-group", &groupMetaCache{found: true})
	inst.Groups.membershipCache.Store("public-group", &memberSet{
		members: map[nostr.PubKey]struct{}{pk: {}},
	})

	withTestInstance(t, inst, func() {
		collectMetrics(context.Background())
	})

	// Public group should be tracked
	if v := testutil.ToFloat64(groupMembers.WithLabelValues(label, "public-group")); v != 1 {
		t.Errorf("groupMembers[public-group] = %v, want 1", v)
	}

	// Private and hidden groups should NOT have per-group metrics
	if v := testutil.ToFloat64(groupMembers.WithLabelValues(label, "secret-group")); v != 0 {
		t.Errorf("groupMembers[secret-group] = %v, want 0", v)
	}
	if v := testutil.ToFloat64(groupMembers.WithLabelValues(label, "hidden-group")); v != 0 {
		t.Errorf("groupMembers[hidden-group] = %v, want 0", v)
	}

	// But total still includes all distinct members (same pk in all 3 groups)
	if v := testutil.ToFloat64(groupMembersTotal.WithLabelValues(label)); v != 1 {
		t.Errorf("groupMembersTotal = %v, want 1", v)
	}

	// Only 1 tracked (the public group)
	if v := testutil.ToFloat64(groupsTracked.WithLabelValues(label)); v != 1 {
		t.Errorf("groupsTracked = %v, want 1", v)
	}
}

func TestMetrics_QueryDurationHistogram(t *testing.T) {
	inst := createMetricsTestInstance(t)

	// Count observations before
	beforeTotal := testutil.CollectAndCount(QueryDuration)
	beforeDB := testutil.CollectAndCount(QueryDBDuration)
	beforeDrain := testutil.CollectAndCount(QueryDrainDuration)

	// Run a query to trigger the histograms
	for range inst.Events.QueryEvents(nostr.Filter{Kinds: []nostr.Kind{1}}, 10) {
	}

	// All three histograms should still be collectible (no panics, no errors)
	if afterTotal := testutil.CollectAndCount(QueryDuration); afterTotal < beforeTotal {
		t.Errorf("QueryDuration metric count decreased: before=%d after=%d", beforeTotal, afterTotal)
	}
	if afterDB := testutil.CollectAndCount(QueryDBDuration); afterDB < beforeDB {
		t.Errorf("QueryDBDuration metric count decreased: before=%d after=%d", beforeDB, afterDB)
	}
	if afterDrain := testutil.CollectAndCount(QueryDrainDuration); afterDrain < beforeDrain {
		t.Errorf("QueryDrainDuration metric count decreased: before=%d after=%d", beforeDrain, afterDrain)
	}
}

// readHistogram returns the current sample count and sum for a labeled
// histogram child.
func readHistogram(t *testing.T, vec *prometheus.HistogramVec, label string) (count uint64, sum float64) {
	t.Helper()
	m, err := vec.GetMetricWithLabelValues(label)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues: %v", err)
	}
	var pb dto.Metric
	if err := m.(prometheus.Metric).Write(&pb); err != nil {
		t.Fatalf("histogram.Write: %v", err)
	}
	h := pb.GetHistogram()
	return h.GetSampleCount(), h.GetSampleSum()
}

// TestMetrics_QueryDrainAccounting proves that time blocked inside
// `yield(evt)` is recorded against QueryDrainDuration and excluded from
// QueryDBDuration. Without this split, a back-pressured WebSocket peer
// is indistinguishable from slow Postgres in the DB metric.
func TestMetrics_QueryDrainAccounting(t *testing.T) {
	inst := createMetricsTestInstance(t)
	label := inst.Config.Schema

	// Insert a handful of plain text-note events under a fresh author so
	// the query returns a known number of rows.
	const nEvents = 5
	sec := nostr.Generate()
	for i := 0; i < nEvents; i++ {
		evt := nostr.Event{
			Kind:      1,
			CreatedAt: nostr.Now(),
			PubKey:    sec.Public(),
			Tags:      nostr.Tags{},
			Content:   fmt.Sprintf("drain-test-%d", i),
		}
		evt.Sign(sec)
		if err := inst.Events.SaveEvent(evt); err != nil {
			t.Fatalf("SaveEvent[%d]: %v", i, err)
		}
	}

	// Snapshot the histogram counts/sums before the slow-consumer query.
	totalCountBefore, totalSumBefore := readHistogram(t, QueryDuration, label)
	dbCountBefore, dbSumBefore := readHistogram(t, QueryDBDuration, label)
	drainCountBefore, drainSumBefore := readHistogram(t, QueryDrainDuration, label)

	// Run the query with a consumer that sleeps per event. The sleep time
	// should accumulate in QueryDrainDuration and be excluded from
	// QueryDBDuration.
	const sleepPer = 50 * time.Millisecond
	seen := 0
	queryStart := time.Now()
	for range inst.Events.QueryEvents(nostr.Filter{Kinds: []nostr.Kind{1}, Authors: []nostr.PubKey{sec.Public()}}, 100) {
		seen++
		time.Sleep(sleepPer)
	}
	wall := time.Since(queryStart)

	if seen != nEvents {
		t.Fatalf("expected %d events from QueryEvents, got %d", nEvents, seen)
	}

	totalCountAfter, totalSumAfter := readHistogram(t, QueryDuration, label)
	dbCountAfter, dbSumAfter := readHistogram(t, QueryDBDuration, label)
	drainCountAfter, drainSumAfter := readHistogram(t, QueryDrainDuration, label)

	// Exactly one observation per query on each histogram.
	if delta := totalCountAfter - totalCountBefore; delta != 1 {
		t.Errorf("QueryDuration sample-count delta = %d, want 1", delta)
	}
	if delta := dbCountAfter - dbCountBefore; delta != 1 {
		t.Errorf("QueryDBDuration sample-count delta = %d, want 1", delta)
	}
	if delta := drainCountAfter - drainCountBefore; delta != 1 {
		t.Errorf("QueryDrainDuration sample-count delta = %d, want 1", delta)
	}

	totalDelta := totalSumAfter - totalSumBefore
	dbDelta := dbSumAfter - dbSumBefore
	drainDelta := drainSumAfter - drainSumBefore

	// Drain should account for at least 90% of the cumulative sleep —
	// scheduler jitter can shave a little off but shouldn't cut deeper.
	expectedDrain := float64(nEvents) * sleepPer.Seconds()
	if drainDelta < expectedDrain*0.9 {
		t.Errorf("drain delta = %.3fs, want >= %.3fs (~ %d × %s)",
			drainDelta, expectedDrain*0.9, nEvents, sleepPer)
	}

	// DB time must be strictly less than the drain time — the query
	// itself runs against an in-process testcontainer Postgres and
	// returns 5 rows; it should be milliseconds, far below the 250ms of
	// induced sleep.
	if dbDelta >= drainDelta {
		t.Errorf("dbDelta (%.3fs) should be < drainDelta (%.3fs); drain accounting may be broken",
			dbDelta, drainDelta)
	}

	// Total = DB + drain, modulo float arithmetic. Allow 1ms tolerance.
	if got := dbDelta + drainDelta; got < totalDelta-0.001 || got > totalDelta+0.001 {
		t.Errorf("db (%.3fs) + drain (%.3fs) = %.3fs, want ≈ total (%.3fs)",
			dbDelta, drainDelta, got, totalDelta)
	}

	// Sanity: total must not exceed measured wall time of the loop.
	if totalDelta > wall.Seconds()+0.05 {
		t.Errorf("total observation %.3fs exceeds measured wall time %.3fs", totalDelta, wall.Seconds())
	}
}

func TestMetrics_ConcurrentCollect(t *testing.T) {
	inst := createMetricsTestInstance(t)

	inst.Groups.metadataCache.Store("g1", &groupMetaCache{found: true})
	inst.Groups.membershipCache.Store("g1", &memberSet{
		members: map[nostr.PubKey]struct{}{nostr.Generate().Public(): {}},
	})
	inst.Management.relayMembers.Store(nostr.Generate().Public(), struct{}{})

	withTestInstance(t, inst, func() {
		// Run multiple collections concurrently — should not panic or race
		var wg sync.WaitGroup
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				collectMetrics(context.Background())
			}()
		}
		wg.Wait()
	})
}

func TestMetrics_MultipleInstances(t *testing.T) {
	inst1 := createMetricsTestInstance(t)
	inst2 := createMetricsTestInstance(t)

	// Different schemas = different instance labels
	if inst1.Config.Schema == inst2.Config.Schema {
		t.Fatal("test instances should have different schemas")
	}

	inst1.Groups.metadataCache.Store("g1", &groupMetaCache{found: true})
	inst2.Groups.metadataCache.Store("g2", &groupMetaCache{found: true})
	inst2.Groups.metadataCache.Store("g3", &groupMetaCache{found: true})

	instancesMux.Lock()
	oldByName := instancesByName
	oldByHost := instancesByHost
	instancesByName = map[string]*Instance{"inst1": inst1, "inst2": inst2}
	instancesByHost = map[string]*Instance{inst1.Config.Host: inst1, inst2.Config.Host: inst2}
	instancesMux.Unlock()
	defer func() {
		instancesMux.Lock()
		instancesByName = oldByName
		instancesByHost = oldByHost
		instancesMux.Unlock()
	}()

	collectMetrics(context.Background())

	// Each instance should have its own metric
	if v := testutil.ToFloat64(groupsTotal.WithLabelValues(inst1.Config.Schema)); v != 1 {
		t.Errorf("inst1 groupsTotal = %v, want 1", v)
	}
	if v := testutil.ToFloat64(groupsTotal.WithLabelValues(inst2.Config.Schema)); v != 2 {
		t.Errorf("inst2 groupsTotal = %v, want 2", v)
	}
}
