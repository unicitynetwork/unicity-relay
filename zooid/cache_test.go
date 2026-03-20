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

// === Write-restricted and role cache tests ===

func TestGroupMetadataCache_WriteRestricted(t *testing.T) {
	groups, _ := createTestGroupStore()
	groups.WarmCaches()

	// Create a write-restricted group
	createEvent := nostr.Event{
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"h", "announcements"},
		},
		Content: `{"name":"Announcements","closed":true,"write-restricted":true}`,
	}
	groups.UpdateMetadata(createEvent)

	if !groups.IsWriteRestricted("announcements") {
		t.Error("IsWriteRestricted should return true for write-restricted group")
	}

	// Create a normal group
	normalEvent := nostr.Event{
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"h", "general"},
		},
		Content: `{"name":"General"}`,
	}
	groups.UpdateMetadata(normalEvent)

	if groups.IsWriteRestricted("general") {
		t.Error("IsWriteRestricted should return false for normal group")
	}
}

func TestGroupMetadataCache_WriteRestrictedWarmUp(t *testing.T) {
	groups, _ := createTestGroupStore()

	// Store a write-restricted group
	createEvent := nostr.Event{
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"h", "wrgrp"},
		},
		Content: `{"name":"WR Group","write-restricted":true}`,
	}
	groups.UpdateMetadata(createEvent)

	// Fresh store and warm
	groups2 := &GroupStore{
		Config:     groups.Config,
		Events:     groups.Events,
		Management: groups.Management,
	}
	groups2.WarmCaches()

	if !groups2.IsWriteRestricted("wrgrp") {
		t.Error("IsWriteRestricted should return true after WarmCaches")
	}
}

func TestRoleCache_SetAndCheck(t *testing.T) {
	groups, _ := createTestGroupStore()
	groups.WarmCaches()

	pk := nostr.Generate().Public()

	// No roles initially
	if groups.HasRole("grp", pk, "writer") {
		t.Error("HasRole should return false before setting roles")
	}

	// Set writer role
	groups.SetMemberRoles("grp", pk, []string{"writer"})

	if !groups.HasRole("grp", pk, "writer") {
		t.Error("HasRole should return true after SetMemberRoles")
	}

	// Replace roles (remove writer)
	groups.SetMemberRoles("grp", pk, []string{})

	if groups.HasRole("grp", pk, "writer") {
		t.Error("HasRole should return false after roles replaced with empty set")
	}
}

func TestRoleCache_ClearOnRemove(t *testing.T) {
	groups, _ := createTestGroupStore()
	groups.WarmCaches()

	pk := nostr.Generate().Public()

	groups.SetMemberRoles("grp", pk, []string{"writer"})
	if !groups.HasRole("grp", pk, "writer") {
		t.Fatal("HasRole should return true after SetMemberRoles")
	}

	groups.ClearMemberRoles("grp", pk)

	if groups.HasRole("grp", pk, "writer") {
		t.Error("HasRole should return false after ClearMemberRoles")
	}
}

func TestRoleCache_WarmUpFromPutUserEvents(t *testing.T) {
	groups, _ := createTestGroupStore()

	pk := nostr.Generate().Public()

	// Store a put-user event with writer role (p tag has role at position 2+)
	putEvent := nostr.Event{
		Kind:      nostr.KindSimpleGroupPutUser,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"p", pk.Hex(), "writer"},
			{"h", "rolegrp"},
		},
	}
	groups.Events.SignAndStoreEvent(&putEvent, false)

	// Fresh store and warm
	groups2 := &GroupStore{
		Config:     groups.Config,
		Events:     groups.Events,
		Management: groups.Management,
	}
	groups2.WarmCaches()

	if !groups2.HasRole("rolegrp", pk, "writer") {
		t.Error("HasRole should return true after WarmCaches with put-user role")
	}
	if !groups2.IsMember("rolegrp", pk) {
		t.Error("IsMember should also return true")
	}
}

func TestRoleCache_WarmUpRoleReplacement(t *testing.T) {
	groups, _ := createTestGroupStore()

	pk := nostr.Generate().Public()

	// First put-user with writer role
	putEvent1 := nostr.Event{
		Kind:      nostr.KindSimpleGroupPutUser,
		CreatedAt: nostr.Timestamp(1000),
		Tags: nostr.Tags{
			{"p", pk.Hex(), "writer"},
			{"h", "replacegrp"},
		},
	}
	groups.Events.SignAndStoreEvent(&putEvent1, false)

	// Second put-user without writer role (should replace)
	putEvent2 := nostr.Event{
		Kind:      nostr.KindSimpleGroupPutUser,
		CreatedAt: nostr.Timestamp(2000),
		Tags: nostr.Tags{
			{"p", pk.Hex()},
			{"h", "replacegrp"},
		},
	}
	groups.Events.SignAndStoreEvent(&putEvent2, false)

	// Fresh store and warm
	groups2 := &GroupStore{
		Config:     groups.Config,
		Events:     groups.Events,
		Management: groups.Management,
	}
	groups2.WarmCaches()

	if groups2.HasRole("replacegrp", pk, "writer") {
		t.Error("HasRole should return false after role was replaced by later put-user")
	}
	if !groups2.IsMember("replacegrp", pk) {
		t.Error("IsMember should still return true")
	}
}

func TestCanWrite_NotWriteRestricted(t *testing.T) {
	groups, _ := createTestGroupStore()
	groups.WarmCaches()

	// Create normal group
	createEvent := nostr.Event{
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"h", "normalgrp"}},
		Content:   `{"name":"Normal"}`,
	}
	groups.UpdateMetadata(createEvent)

	pk := nostr.Generate().Public()
	if !groups.CanWrite("normalgrp", pk) {
		t.Error("CanWrite should return true for non-write-restricted groups")
	}
}

func TestCanWrite_WriteRestricted_NoRole(t *testing.T) {
	groups, _ := createTestGroupStore()
	groups.Config.Info.Pubkey = nostr.Generate().Public().Hex()
	groups.WarmCaches()

	// Create write-restricted group
	createEvent := nostr.Event{
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"h", "wrgrp"}},
		Content:   `{"name":"WR","write-restricted":true}`,
	}
	groups.UpdateMetadata(createEvent)

	pk := nostr.Generate().Public()
	groups.AddMember("wrgrp", pk)

	if groups.CanWrite("wrgrp", pk) {
		t.Error("CanWrite should return false for member without writer role")
	}
}

func TestCanWrite_WriteRestricted_WithWriterRole(t *testing.T) {
	groups, _ := createTestGroupStore()
	groups.Config.Info.Pubkey = nostr.Generate().Public().Hex()
	groups.WarmCaches()

	createEvent := nostr.Event{
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"h", "wrgrp"}},
		Content:   `{"name":"WR","write-restricted":true}`,
	}
	groups.UpdateMetadata(createEvent)

	pk := nostr.Generate().Public()
	groups.AddMember("wrgrp", pk)
	groups.SetMemberRoles("wrgrp", pk, []string{"writer"})

	if !groups.CanWrite("wrgrp", pk) {
		t.Error("CanWrite should return true for member with writer role")
	}
}

func TestCanWrite_WriteRestricted_Admin(t *testing.T) {
	groups, _ := createTestGroupStore()
	owner := nostr.Generate().Public()
	groups.Config.Info.Pubkey = owner.Hex()
	groups.WarmCaches()

	createEvent := nostr.Event{
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"h", "wrgrp"}},
		Content:   `{"name":"WR","write-restricted":true}`,
	}
	groups.UpdateMetadata(createEvent)

	if !groups.CanWrite("wrgrp", owner) {
		t.Error("CanWrite should return true for admin/owner")
	}
}

func TestCanWrite_WriteRestricted_Creator(t *testing.T) {
	groups, _ := createTestGroupStore()
	groups.Config.Info.Pubkey = nostr.Generate().Public().Hex()
	groups.WarmCaches()

	creator := nostr.Generate().Public()

	// Set the creator
	groups.creatorCache.Store("wrgrp", creator)

	createEvent := nostr.Event{
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"h", "wrgrp"}},
		Content:   `{"name":"WR","write-restricted":true}`,
	}
	groups.UpdateMetadata(createEvent)

	if !groups.CanWrite("wrgrp", creator) {
		t.Error("CanWrite should return true for group creator")
	}
}

func TestCheckWrite_WriteRestrictedCreation_AdminAllowed(t *testing.T) {
	groups, _ := createTestGroupStore()
	admin := nostr.Generate().Public()
	groups.Config.Info.Pubkey = admin.Hex()
	groups.WarmCaches()

	event := nostr.Event{
		Kind:      nostr.KindSimpleGroupCreateGroup,
		CreatedAt: nostr.Now(),
		PubKey:    admin,
		Tags:      nostr.Tags{{"h", "announcements"}},
		Content:   `{"name":"Announcements","closed":true,"write-restricted":true}`,
	}

	result := groups.CheckWrite(event)
	if result != "" {
		t.Errorf("CheckWrite should allow admin to create write-restricted group, got: %s", result)
	}
}

func TestCheckWrite_WriteRestrictedCreation_NonAdminRejected(t *testing.T) {
	groups, _ := createTestGroupStore()
	groups.Config.Info.Pubkey = nostr.Generate().Public().Hex()
	groups.WarmCaches()

	nonAdmin := nostr.Generate().Public()
	event := nostr.Event{
		Kind:      nostr.KindSimpleGroupCreateGroup,
		CreatedAt: nostr.Now(),
		PubKey:    nonAdmin,
		Tags:      nostr.Tags{{"h", "announcements"}},
		Content:   `{"name":"Announcements","closed":true,"write-restricted":true}`,
	}

	result := groups.CheckWrite(event)
	if result == "" {
		t.Error("CheckWrite should reject non-admin creating write-restricted group")
	}
	if result != "restricted: only admins can create write-restricted groups" {
		t.Errorf("unexpected error message: %s", result)
	}
}

func TestCheckWrite_NormalCreation_NonAdminAllowed(t *testing.T) {
	groups, _ := createTestGroupStore()
	groups.Config.Info.Pubkey = nostr.Generate().Public().Hex()
	groups.WarmCaches()

	nonAdmin := nostr.Generate().Public()
	event := nostr.Event{
		Kind:      nostr.KindSimpleGroupCreateGroup,
		CreatedAt: nostr.Now(),
		PubKey:    nonAdmin,
		Tags:      nostr.Tags{{"h", "general"}},
		Content:   `{"name":"General"}`,
	}

	result := groups.CheckWrite(event)
	if result != "" {
		t.Errorf("CheckWrite should allow non-admin to create normal group, got: %s", result)
	}
}

func TestGroupDeleteClearsRoleCache(t *testing.T) {
	groups, _ := createTestGroupStore()
	groups.WarmCaches()

	pk := nostr.Generate().Public()
	groups.SetMemberRoles("delgrp", pk, []string{"writer"})

	if !groups.HasRole("delgrp", pk, "writer") {
		t.Fatal("HasRole should return true before delete")
	}

	groups.DeleteGroup("delgrp")

	if groups.HasRole("delgrp", pk, "writer") {
		t.Error("HasRole should return false after DeleteGroup")
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

// === Member count tests ===

// findTagValue returns the value of the first tag with the given name, or "" if not found.
func findTagValue(tags nostr.Tags, name string) string {
	for _, tag := range tags {
		if len(tag) >= 2 && tag[0] == name {
			return tag[1]
		}
	}
	return ""
}

func TestGetMemberCount_Empty(t *testing.T) {
	groups, _ := createTestGroupStore()
	groups.WarmCaches()

	count := groups.GetMemberCount("nonexistent")
	if count != 0 {
		t.Errorf("GetMemberCount for nonexistent group = %d, want 0", count)
	}
}

func TestGetMemberCount_AfterAddRemove(t *testing.T) {
	groups, _ := createTestGroupStore()
	groups.WarmCaches()

	pk1 := nostr.Generate().Public()
	pk2 := nostr.Generate().Public()
	pk3 := nostr.Generate().Public()

	groups.AddMember("countgrp", pk1)
	groups.AddMember("countgrp", pk2)
	groups.AddMember("countgrp", pk3)

	count := groups.GetMemberCount("countgrp")
	if count != 3 {
		t.Errorf("GetMemberCount after adding 3 members = %d, want 3", count)
	}

	groups.RemoveMember("countgrp", pk2)

	count = groups.GetMemberCount("countgrp")
	if count != 2 {
		t.Errorf("GetMemberCount after removing 1 member = %d, want 2", count)
	}
}

func TestUpdateMetadata_IncludesMemberCount(t *testing.T) {
	groups, _ := createTestGroupStore()
	groups.WarmCaches()

	pk1 := nostr.Generate().Public()
	pk2 := nostr.Generate().Public()
	groups.AddMember("metagrp", pk1)
	groups.AddMember("metagrp", pk2)

	createEvent := nostr.Event{
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"h", "metagrp"},
		},
		Content: `{"name":"Meta Group"}`,
	}
	groups.UpdateMetadata(createEvent)

	meta, found := groups.GetMetadata("metagrp")
	if !found {
		t.Fatal("GetMetadata should return found=true after UpdateMetadata")
	}

	memberCount := findTagValue(meta.Tags, "member_count")
	if memberCount != "2" {
		t.Errorf("member_count tag = %q, want %q", memberCount, "2")
	}
}

func TestUpdateMetadata_ZeroMemberCount(t *testing.T) {
	groups, _ := createTestGroupStore()
	groups.WarmCaches()

	createEvent := nostr.Event{
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"h", "emptygrp"},
		},
		Content: `{"name":"Empty Group"}`,
	}
	groups.UpdateMetadata(createEvent)

	meta, found := groups.GetMetadata("emptygrp")
	if !found {
		t.Fatal("GetMetadata should return found=true after UpdateMetadata")
	}

	memberCount := findTagValue(meta.Tags, "member_count")
	if memberCount != "0" {
		t.Errorf("member_count tag = %q, want %q", memberCount, "0")
	}
}

func TestRefreshMemberCount_UpdatesExisting(t *testing.T) {
	groups, _ := createTestGroupStore()
	groups.WarmCaches()

	// Create metadata with 0 members initially
	createEvent := nostr.Event{
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"h", "refreshgrp"},
		},
		Content: `{"name":"Refresh Group"}`,
	}
	groups.UpdateMetadata(createEvent)

	meta, found := groups.GetMetadata("refreshgrp")
	if !found {
		t.Fatal("GetMetadata should return found=true after UpdateMetadata")
	}
	if findTagValue(meta.Tags, "member_count") != "0" {
		t.Fatalf("member_count should be 0 initially, got %q", findTagValue(meta.Tags, "member_count"))
	}

	// Add 3 members
	pk1 := nostr.Generate().Public()
	pk2 := nostr.Generate().Public()
	pk3 := nostr.Generate().Public()
	groups.AddMember("refreshgrp", pk1)
	groups.AddMember("refreshgrp", pk2)
	groups.AddMember("refreshgrp", pk3)

	// Refresh the member count
	if err := groups.RefreshMemberCount("refreshgrp"); err != nil {
		t.Fatalf("RefreshMemberCount returned error: %v", err)
	}

	meta, found = groups.GetMetadata("refreshgrp")
	if !found {
		t.Fatal("GetMetadata should return found=true after RefreshMemberCount")
	}

	memberCount := findTagValue(meta.Tags, "member_count")
	if memberCount != "3" {
		t.Errorf("member_count tag after refresh = %q, want %q", memberCount, "3")
	}
}

func TestRefreshMemberCount_NoMetadata(t *testing.T) {
	groups, _ := createTestGroupStore()
	groups.WarmCaches()

	// Should return nil and not panic when no metadata exists
	err := groups.RefreshMemberCount("nometadata")
	if err != nil {
		t.Errorf("RefreshMemberCount for nonexistent group returned error: %v", err)
	}
}

func TestRefreshMemberCount_PreservesOtherTags(t *testing.T) {
	groups, _ := createTestGroupStore()
	groups.WarmCaches()

	// Create metadata for a closed (non-private) group
	createEvent := nostr.Event{
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"h", "preservegrp"},
		},
		Content: `{"name":"Test","closed":true}`,
	}
	groups.UpdateMetadata(createEvent)

	// Add 2 members
	pk1 := nostr.Generate().Public()
	pk2 := nostr.Generate().Public()
	groups.AddMember("preservegrp", pk1)
	groups.AddMember("preservegrp", pk2)

	if err := groups.RefreshMemberCount("preservegrp"); err != nil {
		t.Fatalf("RefreshMemberCount returned error: %v", err)
	}

	meta, found := groups.GetMetadata("preservegrp")
	if !found {
		t.Fatal("GetMetadata should return found=true after RefreshMemberCount")
	}

	// Verify member_count is correct
	memberCount := findTagValue(meta.Tags, "member_count")
	if memberCount != "2" {
		t.Errorf("member_count tag = %q, want %q", memberCount, "2")
	}

	// Verify "closed" tag still exists
	hasClosed := false
	for _, tag := range meta.Tags {
		if len(tag) >= 1 && tag[0] == "closed" {
			hasClosed = true
			break
		}
	}
	if !hasClosed {
		t.Error("closed tag should still exist after RefreshMemberCount")
	}

	// Verify "d" tag still has "preservegrp"
	dValue := findTagValue(meta.Tags, "d")
	if dValue != "preservegrp" {
		t.Errorf("d tag = %q, want %q", dValue, "preservegrp")
	}

	// Verify content is preserved
	if meta.Content != `{"name":"Test","closed":true}` {
		t.Errorf("Content = %q, want %q", meta.Content, `{"name":"Test","closed":true}`)
	}
}

func TestUpdateMetadata_PrivateGroupOmitsMemberCount(t *testing.T) {
	groups, _ := createTestGroupStore()
	groups.WarmCaches()

	pk := nostr.Generate().Public()
	groups.AddMember("privgrp", pk)

	createEvent := nostr.Event{
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"h", "privgrp"},
		},
		Content: `{"name":"Secret","private":true}`,
	}
	groups.UpdateMetadata(createEvent)

	meta, found := groups.GetMetadata("privgrp")
	if !found {
		t.Fatal("GetMetadata should return found=true")
	}

	if findTagValue(meta.Tags, "member_count") != "" {
		t.Error("private group metadata should not contain member_count tag")
	}
}

func TestRefreshMemberCount_PrivateGroupSkipped(t *testing.T) {
	groups, _ := createTestGroupStore()
	groups.WarmCaches()

	createEvent := nostr.Event{
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"h", "privrefresh"},
		},
		Content: `{"name":"Private","private":true}`,
	}
	groups.UpdateMetadata(createEvent)

	groups.AddMember("privrefresh", nostr.Generate().Public())

	// RefreshMemberCount should be a no-op for private groups
	err := groups.RefreshMemberCount("privrefresh")
	if err != nil {
		t.Fatalf("RefreshMemberCount returned error: %v", err)
	}

	meta, _ := groups.GetMetadata("privrefresh")
	if findTagValue(meta.Tags, "member_count") != "" {
		t.Error("private group should not get member_count after RefreshMemberCount")
	}
}

func TestUpdateMetadata_StripsClientMemberCount(t *testing.T) {
	groups, _ := createTestGroupStore()
	groups.WarmCaches()

	groups.AddMember("stripgrp", nostr.Generate().Public())

	// Client includes a bogus member_count tag
	editEvent := nostr.Event{
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"h", "stripgrp"},
			{"member_count", "99999"},
		},
		Content: `{"name":"Strip Test"}`,
	}
	groups.UpdateMetadata(editEvent)

	meta, found := groups.GetMetadata("stripgrp")
	if !found {
		t.Fatal("GetMetadata should return found=true")
	}

	memberCount := findTagValue(meta.Tags, "member_count")
	if memberCount != "1" {
		t.Errorf("member_count = %q, want %q (client-supplied value should be stripped)", memberCount, "1")
	}
}

func TestRefreshMemberCount_ShortCircuitsWhenUnchanged(t *testing.T) {
	groups, _ := createTestGroupStore()
	groups.WarmCaches()

	createEvent := nostr.Event{
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"h", "nochurn"},
		},
		Content: `{"name":"No Churn"}`,
	}
	groups.UpdateMetadata(createEvent)

	// First refresh: sets member_count to "0"
	groups.RefreshMemberCount("nochurn")

	meta1, _ := groups.GetMetadata("nochurn")
	id1 := meta1.ID

	// Second refresh with same count: should short-circuit (no new event)
	groups.RefreshMemberCount("nochurn")

	meta2, _ := groups.GetMetadata("nochurn")
	if meta2.ID != id1 {
		t.Error("RefreshMemberCount should not generate a new event when count is unchanged")
	}
}

func TestMemberCount_WarmCachesPreservesTag(t *testing.T) {
	groups, _ := createTestGroupStore()
	groups.WarmCaches()

	// Create metadata and add 5 members
	createEvent := nostr.Event{
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"h", "warmgrp"},
		},
		Content: `{"name":"Warm Group"}`,
	}
	groups.UpdateMetadata(createEvent)

	for i := 0; i < 5; i++ {
		groups.AddMember("warmgrp", nostr.Generate().Public())
	}
	if err := groups.RefreshMemberCount("warmgrp"); err != nil {
		t.Fatalf("RefreshMemberCount returned error: %v", err)
	}

	// Create a fresh GroupStore pointing at the same events
	groups2 := &GroupStore{
		Config:     groups.Config,
		Events:     groups.Events,
		Management: groups.Management,
	}
	groups2.WarmCaches()

	meta, found := groups2.GetMetadata("warmgrp")
	if !found {
		t.Fatal("GetMetadata should return found=true after WarmCaches on fresh store")
	}

	memberCount := findTagValue(meta.Tags, "member_count")
	if memberCount != "5" {
		t.Errorf("member_count tag after WarmCaches = %q, want %q", memberCount, "5")
	}
}
