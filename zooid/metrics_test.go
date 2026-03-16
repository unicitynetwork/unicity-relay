package zooid

import (
	"sync"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/khatru"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func createMetricsTestInstance() *Instance {
	config := &Config{
		Host:   "test.com",
		secret: nostr.Generate(),
	}
	config.Groups.Enabled = true
	schema := &Schema{Name: "test_" + RandomString(8)}
	relay := khatru.NewRelay()
	events := &EventStore{
		Relay:  relay,
		Config: config,
		Schema: schema,
	}
	events.Init()

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

func TestMetrics_CacheGauges(t *testing.T) {
	inst := createMetricsTestInstance()

	// Populate group metadata cache: 3 groups, 2 private, 1 hidden, 1 closed
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
	inst.Groups.membershipCache.Store("group-a", msA)
	inst.Groups.membershipCache.Store("group-b", msB)

	// Populate relay members
	inst.Management.relayMembers.Store(pk1, struct{}{})
	inst.Management.relayMembers.Store(pk2, struct{}{})
	inst.Management.relayMembers.Store(pk3, struct{}{})

	// Populate banned pubkeys
	inst.Management.bannedPubkeys.Store(nostr.Generate().Public(), "spam")

	// Populate banned events
	fakeID := nostr.Generate().Public() // just need a value for the sync.Map
	inst.Management.bannedEvents.Store(fakeID, "abuse")

	// Wire up instance maps so GetAllInstances finds it
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

	collectMetrics()

	// Group counts
	if v := testutil.ToFloat64(groupsTotal.WithLabelValues(metricsInstance)); v != 3 {
		t.Errorf("groupsTotal = %v, want 3", v)
	}
	if v := testutil.ToFloat64(groupsPrivate.WithLabelValues(metricsInstance)); v != 2 {
		t.Errorf("groupsPrivate = %v, want 2", v)
	}
	if v := testutil.ToFloat64(groupsHidden.WithLabelValues(metricsInstance)); v != 1 {
		t.Errorf("groupsHidden = %v, want 1", v)
	}
	if v := testutil.ToFloat64(groupsClosed.WithLabelValues(metricsInstance)); v != 1 {
		t.Errorf("groupsClosed = %v, want 1", v)
	}

	// Per-group members
	if v := testutil.ToFloat64(groupMembers.WithLabelValues(metricsInstance, "group-a")); v != 2 {
		t.Errorf("groupMembers[group-a] = %v, want 2", v)
	}
	if v := testutil.ToFloat64(groupMembers.WithLabelValues(metricsInstance, "group-b")); v != 2 {
		t.Errorf("groupMembers[group-b] = %v, want 2", v)
	}

	// Total group members
	if v := testutil.ToFloat64(groupMembersTotal.WithLabelValues(metricsInstance)); v != 4 {
		t.Errorf("groupMembersTotal = %v, want 4", v)
	}

	// Relay members
	if v := testutil.ToFloat64(relayMembersTotal.WithLabelValues(metricsInstance)); v != 3 {
		t.Errorf("relayMembersTotal = %v, want 3", v)
	}

	// Banned pubkeys
	if v := testutil.ToFloat64(bannedPubkeysTotal.WithLabelValues(metricsInstance)); v != 1 {
		t.Errorf("bannedPubkeysTotal = %v, want 1", v)
	}

	// Banned events
	if v := testutil.ToFloat64(bannedEventsTotal.WithLabelValues(metricsInstance)); v != 1 {
		t.Errorf("bannedEventsTotal = %v, want 1", v)
	}
}

func TestMetrics_DBGauges(t *testing.T) {
	inst := createMetricsTestInstance()

	// Store some events: 2 chat messages (kind 9), 1 regular event
	for _, evt := range []nostr.Event{
		{Kind: 9, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"h", "g1"}}, Content: "hello"},
		{Kind: 9, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"h", "g1"}}, Content: "world"},
		{Kind: 1, CreatedAt: nostr.Now(), Content: "note"},
	} {
		inst.Events.SignAndStoreEvent(&evt, false)
	}

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

	collectMetrics()

	// eventsTotal includes the events we stored plus any internal events from Init
	total := testutil.ToFloat64(eventsTotal.WithLabelValues(metricsInstance))
	if total < 3 {
		t.Errorf("eventsTotal = %v, want >= 3", total)
	}

	// messagesTotal should be exactly 2 (the kind-9 events)
	if v := testutil.ToFloat64(messagesTotal.WithLabelValues(metricsInstance)); v != 2 {
		t.Errorf("messagesTotal = %v, want 2", v)
	}
}

func TestMetrics_GroupMembersCap(t *testing.T) {
	inst := createMetricsTestInstance()

	// Create 1005 groups with 1 member each
	pk := nostr.Generate().Public()
	for i := 0; i < 1005; i++ {
		h := RandomString(10)
		inst.Groups.metadataCache.Store(h, &groupMetaCache{found: true})
		ms := &memberSet{members: map[nostr.PubKey]struct{}{pk: {}}}
		inst.Groups.membershipCache.Store(h, ms)
	}

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

	collectMetrics()

	// groupsTracked should be capped at 1000
	tracked := testutil.ToFloat64(groupsTracked.WithLabelValues(metricsInstance))
	if tracked != 1000 {
		t.Errorf("groupsTracked = %v, want 1000", tracked)
	}

	// groupMembersTotal should count all 1005 groups
	total := testutil.ToFloat64(groupMembersTotal.WithLabelValues(metricsInstance))
	if total != 1005 {
		t.Errorf("groupMembersTotal = %v, want 1005", total)
	}
}

func TestMetrics_StaleGroupsCleared(t *testing.T) {
	inst := createMetricsTestInstance()

	// First collection: group exists
	inst.Groups.membershipCache.Store("old-group", &memberSet{
		members: map[nostr.PubKey]struct{}{nostr.Generate().Public(): {}},
	})

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

	collectMetrics()

	if v := testutil.ToFloat64(groupMembers.WithLabelValues(metricsInstance, "old-group")); v != 1 {
		t.Errorf("groupMembers[old-group] = %v, want 1", v)
	}

	// Delete the group from cache, re-collect
	inst.Groups.membershipCache.Delete("old-group")
	collectMetrics()

	// After reset, the stale gauge should be 0
	if v := testutil.ToFloat64(groupMembers.WithLabelValues(metricsInstance, "old-group")); v != 0 {
		t.Errorf("groupMembers[old-group] after delete = %v, want 0", v)
	}
}

func TestMetrics_QueryDurationHistogram(t *testing.T) {
	inst := createMetricsTestInstance()

	// Count observations before
	before := testutil.CollectAndCount(QueryDuration)

	// Run a query to trigger the histogram
	for range inst.Events.QueryEvents(nostr.Filter{Kinds: []nostr.Kind{1}}, 10) {
	}

	// The histogram should still be collectible (no panics, no errors)
	after := testutil.CollectAndCount(QueryDuration)
	if after < before {
		t.Errorf("histogram metric count decreased: before=%d after=%d", before, after)
	}
}

func TestMetrics_ConcurrentCollect(t *testing.T) {
	inst := createMetricsTestInstance()

	inst.Groups.metadataCache.Store("g1", &groupMetaCache{found: true, private: true})
	inst.Groups.membershipCache.Store("g1", &memberSet{
		members: map[nostr.PubKey]struct{}{nostr.Generate().Public(): {}},
	})
	inst.Management.relayMembers.Store(nostr.Generate().Public(), struct{}{})

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

	// Run multiple collections concurrently — should not panic or race
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			collectMetrics()
		}()
	}
	wg.Wait()
}
