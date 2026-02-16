package zooid

import (
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/khatru"
)

// Helper to create a GroupStore with a shared ManagementStore for cache tests
func createTestGroupStore() (*GroupStore, *ManagementStore) {
	config := &Config{
		Host:   "test.com",
		secret: nostr.Generate(),
	}
	config.Groups.Enabled = true
	schema := &Schema{Name: "test_" + RandomString(8)}
	relay := &khatru.Relay{}
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

	return groups, mgmt
}

// === Relay membership cache ===

func TestRelayMembershipCache_WarmUp(t *testing.T) {
	mgmt := createTestManagementStore()

	pk1 := nostr.Generate().Public()
	pk2 := nostr.Generate().Public()

	mgmt.AddMember(pk1)
	mgmt.AddMember(pk2)

	// Reset the cache by creating a fresh ManagementStore pointing at same events
	mgmt2 := &ManagementStore{
		Config: mgmt.Config,
		Events: mgmt.Events,
	}
	mgmt2.WarmCaches()

	if !mgmt2.IsMember(pk1) {
		t.Error("IsMember should return true for pk1 after WarmCaches")
	}
	if !mgmt2.IsMember(pk2) {
		t.Error("IsMember should return true for pk2 after WarmCaches")
	}

	unknown := nostr.Generate().Public()
	if mgmt2.IsMember(unknown) {
		t.Error("IsMember should return false for unknown pubkey")
	}
}

func TestRelayMembershipCache_AddRemove(t *testing.T) {
	mgmt := createTestManagementStore()
	mgmt.WarmCaches()

	pk := nostr.Generate().Public()

	mgmt.AddMember(pk)
	if !mgmt.IsMember(pk) {
		t.Error("IsMember should return true after AddMember")
	}

	mgmt.RemoveMember(pk)
	if mgmt.IsMember(pk) {
		t.Error("IsMember should return false after RemoveMember")
	}
}

func TestRelayMembershipCache_GetMembers(t *testing.T) {
	mgmt := createTestManagementStore()
	mgmt.WarmCaches()

	pk1 := nostr.Generate().Public()
	pk2 := nostr.Generate().Public()

	mgmt.AddMember(pk1)
	mgmt.AddMember(pk2)

	members := mgmt.GetMembers()

	found1, found2 := false, false
	for _, m := range members {
		if m == pk1 {
			found1 = true
		}
		if m == pk2 {
			found2 = true
		}
	}

	if !found1 {
		t.Error("GetMembers should include pk1")
	}
	if !found2 {
		t.Error("GetMembers should include pk2")
	}
}

// === Banned pubkeys cache ===

func TestBannedPubkeysCache_WarmUp(t *testing.T) {
	mgmt := createTestManagementStore()

	pk := nostr.Generate().Public()
	mgmt.AddBannedPubkey(pk, "spam")

	// Create fresh store and warm
	mgmt2 := &ManagementStore{
		Config: mgmt.Config,
		Events: mgmt.Events,
	}
	mgmt2.WarmCaches()

	if !mgmt2.PubkeyIsBanned(pk) {
		t.Error("PubkeyIsBanned should return true after WarmCaches")
	}

	items := mgmt2.GetBannedPubkeyItems()
	found := false
	for _, item := range items {
		if item.PubKey == pk && item.Reason == "spam" {
			found = true
		}
	}
	if !found {
		t.Error("GetBannedPubkeyItems should include banned pubkey with reason")
	}
}

func TestBannedPubkeysCache_BanAllow(t *testing.T) {
	mgmt := createTestManagementStore()
	mgmt.WarmCaches()

	pk := nostr.Generate().Public()

	mgmt.AddBannedPubkey(pk, "test")
	if !mgmt.PubkeyIsBanned(pk) {
		t.Error("PubkeyIsBanned should return true after AddBannedPubkey")
	}

	mgmt.RemoveBannedPubkey(pk)
	if mgmt.PubkeyIsBanned(pk) {
		t.Error("PubkeyIsBanned should return false after RemoveBannedPubkey")
	}
}

// === Banned events cache ===

func TestBannedEventsCache_WarmUp(t *testing.T) {
	mgmt := createTestManagementStore()

	eventID := nostr.MustIDFromHex("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	mgmt.BanEvent(eventID, "inappropriate")

	// Create fresh store and warm
	mgmt2 := &ManagementStore{
		Config: mgmt.Config,
		Events: mgmt.Events,
	}
	mgmt2.WarmCaches()

	if !mgmt2.EventIsBanned(eventID) {
		t.Error("EventIsBanned should return true after WarmCaches")
	}

	items := mgmt2.GetBannedEventItems()
	found := false
	for _, item := range items {
		if item.ID == eventID && item.Reason == "inappropriate" {
			found = true
		}
	}
	if !found {
		t.Error("GetBannedEventItems should include banned event with reason")
	}
}

func TestBannedEventsCache_BanAllow(t *testing.T) {
	mgmt := createTestManagementStore()
	mgmt.WarmCaches()

	eventID := nostr.MustIDFromHex("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

	mgmt.BanEvent(eventID, "spam")
	if !mgmt.EventIsBanned(eventID) {
		t.Error("EventIsBanned should return true after BanEvent")
	}

	mgmt.AllowEvent(eventID, "unbanned")
	if mgmt.EventIsBanned(eventID) {
		t.Error("EventIsBanned should return false after AllowEvent")
	}
}

// === Group metadata cache ===

func TestGroupMetadataCache_WarmUp(t *testing.T) {
	groups, _ := createTestGroupStore()

	// Create a group by storing metadata
	creator := nostr.Generate().Public()
	metaEvent := nostr.Event{
		Kind:      nostr.KindSimpleGroupCreateGroup,
		CreatedAt: nostr.Now(),
		PubKey:    creator,
		Tags: nostr.Tags{
			{"h", "testgrp"},
		},
		Content: `{"name":"Test Group"}`,
	}
	groups.Events.SignAndStoreEvent(&metaEvent, false)
	groups.UpdateMetadata(metaEvent)

	// Create fresh store and warm
	groups2 := &GroupStore{
		Config:     groups.Config,
		Events:     groups.Events,
		Management: groups.Management,
	}
	groups2.WarmCaches()

	meta, found := groups2.GetMetadata("testgrp")
	if !found {
		t.Fatal("GetMetadata should return found=true after WarmCaches")
	}
	if meta.Content != `{"name":"Test Group"}` {
		t.Errorf("GetMetadata content = %q, want %q", meta.Content, `{"name":"Test Group"}`)
	}
}

func TestGroupMetadataCache_Update(t *testing.T) {
	groups, _ := createTestGroupStore()
	groups.WarmCaches()

	// Create group metadata
	createEvent := nostr.Event{
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"h", "updgrp"},
		},
		Content: `{"name":"Original"}`,
	}
	groups.UpdateMetadata(createEvent)

	meta, found := groups.GetMetadata("updgrp")
	if !found {
		t.Fatal("GetMetadata should return found=true after UpdateMetadata")
	}
	if meta.Content != `{"name":"Original"}` {
		t.Errorf("Content = %q, want %q", meta.Content, `{"name":"Original"}`)
	}

	// Update metadata
	updateEvent := nostr.Event{
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"h", "updgrp"},
		},
		Content: `{"name":"Updated"}`,
	}
	groups.UpdateMetadata(updateEvent)

	meta, found = groups.GetMetadata("updgrp")
	if !found {
		t.Fatal("GetMetadata should still return found after update")
	}
	if meta.Content != `{"name":"Updated"}` {
		t.Errorf("Content = %q, want %q", meta.Content, `{"name":"Updated"}`)
	}
}

func TestGroupMetadataCache_Private(t *testing.T) {
	groups, _ := createTestGroupStore()
	groups.WarmCaches()

	// Create a private group
	createEvent := nostr.Event{
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"h", "privgrp"},
		},
		Content: `{"name":"Private","private":true}`,
	}
	groups.UpdateMetadata(createEvent)

	if !groups.IsPrivateGroup("privgrp") {
		t.Error("IsPrivateGroup should return true for private group")
	}

	// Create a public group
	publicEvent := nostr.Event{
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"h", "pubgrp"},
		},
		Content: `{"name":"Public"}`,
	}
	groups.UpdateMetadata(publicEvent)

	if groups.IsPrivateGroup("pubgrp") {
		t.Error("IsPrivateGroup should return false for public group")
	}
}

func TestGroupMetadataCache_Delete(t *testing.T) {
	groups, _ := createTestGroupStore()
	groups.WarmCaches()

	// Populate cache entries directly (test cache clearing, not DB deletion)
	groups.metadataCache.Store("delgrp", &groupMetaCache{
		event: nostr.Event{Content: `{"name":"To Delete"}`},
		found: true,
	})
	groups.getOrCreateMemberSet("delgrp")
	groups.creatorCache.Store("delgrp", nostr.Generate().Public())

	_, found := groups.GetMetadata("delgrp")
	if !found {
		t.Fatal("Group should exist in cache before deletion")
	}

	// DeleteGroup clears all cache entries for the group
	groups.DeleteGroup("delgrp")

	_, found = groups.GetMetadata("delgrp")
	if found {
		t.Error("GetMetadata should return found=false after DeleteGroup")
	}

	if groups.GetGroupCreator("delgrp") != (nostr.PubKey{}) {
		t.Error("Creator cache should be cleared after DeleteGroup")
	}
}

// === Group membership cache ===

func TestGroupMembershipCache_WarmUp(t *testing.T) {
	groups, _ := createTestGroupStore()

	pk1 := nostr.Generate().Public()
	pk2 := nostr.Generate().Public()

	groups.AddMember("grp1", pk1)
	groups.AddMember("grp1", pk2)

	// Create fresh store and warm
	groups2 := &GroupStore{
		Config:     groups.Config,
		Events:     groups.Events,
		Management: groups.Management,
	}
	groups2.WarmCaches()

	if !groups2.IsMember("grp1", pk1) {
		t.Error("IsMember should return true for pk1 after WarmCaches")
	}
	if !groups2.IsMember("grp1", pk2) {
		t.Error("IsMember should return true for pk2 after WarmCaches")
	}

	unknown := nostr.Generate().Public()
	if groups2.IsMember("grp1", unknown) {
		t.Error("IsMember should return false for unknown pubkey")
	}
}

func TestGroupMembershipCache_AddRemove(t *testing.T) {
	groups, _ := createTestGroupStore()
	groups.WarmCaches()

	pk := nostr.Generate().Public()

	groups.AddMember("grp2", pk)
	if !groups.IsMember("grp2", pk) {
		t.Error("IsMember should return true after AddMember")
	}

	groups.RemoveMember("grp2", pk)
	if groups.IsMember("grp2", pk) {
		t.Error("IsMember should return false after RemoveMember")
	}
}

func TestGroupMembershipCache_GetMembers(t *testing.T) {
	groups, _ := createTestGroupStore()
	groups.WarmCaches()

	pk1 := nostr.Generate().Public()
	pk2 := nostr.Generate().Public()

	groups.AddMember("grp3", pk1)
	groups.AddMember("grp3", pk2)

	members := groups.GetMembers("grp3")

	found1, found2 := false, false
	for _, m := range members {
		if m == pk1 {
			found1 = true
		}
		if m == pk2 {
			found2 = true
		}
	}

	if !found1 {
		t.Error("GetMembers should include pk1")
	}
	if !found2 {
		t.Error("GetMembers should include pk2")
	}
}

func TestGroupMembershipCache_Creator(t *testing.T) {
	groups, _ := createTestGroupStore()

	creator := groups.Config.secret.Public()

	// Simulate group creation by storing the create event
	createEvent := nostr.Event{
		Kind:      nostr.KindSimpleGroupCreateGroup,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"h", "creatgrp"},
		},
		Content: `{"name":"Creator Test"}`,
	}
	groups.Events.SignAndStoreEvent(&createEvent, false)

	// Fresh store and warm
	groups2 := &GroupStore{
		Config:     groups.Config,
		Events:     groups.Events,
		Management: groups.Management,
	}
	groups2.WarmCaches()

	gotCreator := groups2.GetGroupCreator("creatgrp")
	if gotCreator != creator {
		t.Errorf("GetGroupCreator = %v, want %v", gotCreator, creator)
	}

	if !groups2.IsGroupCreator("creatgrp", creator) {
		t.Error("IsGroupCreator should return true for actual creator")
	}

	other := nostr.Generate().Public()
	if groups2.IsGroupCreator("creatgrp", other) {
		t.Error("IsGroupCreator should return false for non-creator")
	}
}

func TestGroupMembershipCache_DeleteClearsAll(t *testing.T) {
	groups, _ := createTestGroupStore()
	groups.WarmCaches()

	pk := nostr.Generate().Public()

	// Populate cache entries directly (test cache clearing, not DB deletion)
	groups.metadataCache.Store("delall", &groupMetaCache{
		event: nostr.Event{Content: `{"name":"Delete All"}`},
		found: true,
	})
	ms := groups.getOrCreateMemberSet("delall")
	ms.mu.Lock()
	ms.members[pk] = struct{}{}
	ms.mu.Unlock()
	groups.creatorCache.Store("delall", pk)

	// Verify pre-conditions
	_, found := groups.GetMetadata("delall")
	if !found {
		t.Fatal("Metadata should exist before delete")
	}
	if !groups.IsMember("delall", pk) {
		t.Fatal("Member should exist before delete")
	}

	groups.DeleteGroup("delall")

	_, found = groups.GetMetadata("delall")
	if found {
		t.Error("Metadata cache should be cleared after DeleteGroup")
	}
	if groups.IsMember("delall", pk) {
		t.Error("Membership cache should be cleared after DeleteGroup")
	}
	if groups.GetGroupCreator("delall") != (nostr.PubKey{}) {
		t.Error("Creator cache should be cleared after DeleteGroup")
	}
}
