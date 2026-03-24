package zooid

import (
	"context"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/khatru"
)

func createTestInstance() *Instance {
	ownerSecret := nostr.Generate()
	ownerPubkey := ownerSecret.Public()

	config := &Config{
		Host:   "test.com",
		secret: ownerSecret,
		Info: struct {
			Name        string `toml:"name"`
			Icon        string `toml:"icon"`
			Pubkey      string `toml:"pubkey"`
			Description string `toml:"description"`
		}{
			Name:   "Test Relay",
			Pubkey: ownerPubkey.Hex(),
		},
		Roles: map[string]Role{
			"admin": {
				Pubkeys:   []string{ownerPubkey.Hex()},
				CanManage: true,
				CanInvite: true,
			},
		},
	}
	config.Groups.Enabled = true
	config.Groups.AutoJoin = true

	schema := &Schema{Name: "test_" + RandomString(8)}

	relay := &khatru.Relay{}

	events := &EventStore{
		Relay:  relay,
		Config: config,
		Schema: schema,
	}

	management := &ManagementStore{
		Config: config,
		Events: events,
	}

	groups := &GroupStore{
		Config:     config,
		Events:     events,
		Management: management,
	}

	instance := &Instance{
		Relay:      relay,
		Config:     config,
		Events:     events,
		Management: management,
		Groups:     groups,
	}

	instance.Events.Init()
	management.WarmCaches()
	groups.WarmCaches()

	return instance
}

func TestInstance_AllowRecipientEvent(t *testing.T) {
	instance := createTestInstance()

	userSecret := nostr.Generate()
	userPubkey := userSecret.Public()

	// Add user as member
	instance.Management.AddMember(userPubkey)

	tests := []struct {
		name  string
		event nostr.Event
		want  bool
	}{
		{
			name: "zap event with valid recipient",
			event: nostr.Event{
				Kind: nostr.KindZap,
				Tags: nostr.Tags{{"p", userPubkey.Hex()}},
			},
			want: true,
		},
		{
			name: "gift wrap event with valid recipient",
			event: nostr.Event{
				Kind: nostr.KindGiftWrap,
				Tags: nostr.Tags{{"p", userPubkey.Hex()}},
			},
			want: true,
		},
		{
			name: "zap event with invalid recipient",
			event: nostr.Event{
				Kind: nostr.KindZap,
				Tags: nostr.Tags{{"p", nostr.Generate().Public().Hex()}},
			},
			want: false,
		},
		{
			name: "text note event",
			event: nostr.Event{
				Kind: nostr.KindTextNote,
				Tags: nostr.Tags{{"p", userPubkey.Hex()}},
			},
			want: false,
		},
		{
			name: "zap event without p tag",
			event: nostr.Event{
				Kind: nostr.KindZap,
				Tags: nostr.Tags{},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := instance.AllowRecipientEvent(tt.event)
			if result != tt.want {
				t.Errorf("AllowRecipientEvent() = %v, want %v", result, tt.want)
			}
		})
	}
}

func TestInstance_GenerateInviteEvent(t *testing.T) {
	instance := createTestInstance()

	userPubkey := nostr.Generate().Public()

	// Generate invite event
	inviteEvent := instance.GenerateInviteEvent(userPubkey)

	// Test event properties
	if inviteEvent.Kind != RELAY_INVITE {
		t.Errorf("GenerateInviteEvent() kind = %v, want %v", inviteEvent.Kind, RELAY_INVITE)
	}

	if inviteEvent.PubKey != instance.Config.GetSelf() {
		t.Error("GenerateInviteEvent() should be signed by instance")
	}

	// Test tags
	claimTag := inviteEvent.Tags.Find("claim")
	if claimTag == nil {
		t.Error("GenerateInviteEvent() should have claim tag")
	}

	pTag := inviteEvent.Tags.Find("p")
	if pTag == nil || pTag[1] != userPubkey.Hex() {
		t.Error("GenerateInviteEvent() should have correct p tag")
	}
}

func TestInstance_IsInternalEvent(t *testing.T) {
	instance := createTestInstance()

	tests := []struct {
		name  string
		event nostr.Event
		want  bool
	}{
		{
			name: "internal zooid event",
			event: nostr.Event{
				Kind: nostr.KindApplicationSpecificData,
				Tags: nostr.Tags{{"d", "zooid/banned_pubkeys"}},
			},
			want: true,
		},
		{
			name: "internal zooid event with different data",
			event: nostr.Event{
				Kind: nostr.KindApplicationSpecificData,
				Tags: nostr.Tags{{"d", "zooid/some_data"}},
			},
			want: true,
		},
		{
			name: "non-internal event",
			event: nostr.Event{
				Kind: nostr.KindApplicationSpecificData,
				Tags: nostr.Tags{{"d", "external/data"}},
			},
			want: false,
		},
		{
			name: "wrong kind",
			event: nostr.Event{
				Kind: nostr.KindTextNote,
				Tags: nostr.Tags{{"d", "zooid/data"}},
			},
			want: false,
		},
		{
			name: "no d tag",
			event: nostr.Event{
				Kind: nostr.KindApplicationSpecificData,
				Tags: nostr.Tags{{"t", "tag"}},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := instance.IsInternalEvent(tt.event)
			if result != tt.want {
				t.Errorf("IsInternalEvent() = %v, want %v", result, tt.want)
			}
		})
	}
}

// === OnEventSaved tests ===

// TestOnEventSaved_CreateGroup verifies the full group creation flow:
// kind 9007 saved → OnEventSaved produces metadata, members list, admins list.
func TestOnEventSaved_CreateGroup(t *testing.T) {
	instance := createTestInstance()
	creatorSecret := nostr.Generate()
	creator := creatorSecret.Public()

	createEvent := nostr.Event{
		Kind:      nostr.KindSimpleGroupCreateGroup,
		CreatedAt: nostr.Now(),
		PubKey:    creator,
		Tags:      nostr.Tags{{"h", "general"}},
		Content:   `{"name":"General"}`,
	}
	createEvent.Sign(creatorSecret)
	instance.Events.SaveEvent(createEvent)

	instance.OnEventSaved(context.Background(), createEvent)

	// Metadata (kind 39000) must exist and be queryable
	meta, found := instance.Groups.GetMetadata("general")
	if !found {
		t.Fatal("GetMetadata returned found=false after group creation")
	}
	if meta.Content != `{"name":"General"}` {
		t.Errorf("metadata content = %q, want %q", meta.Content, `{"name":"General"}`)
	}

	// Creator must be a member
	if !instance.Groups.IsMember("general", creator) {
		t.Error("creator should be a member after group creation")
	}

	// Members list (kind 39002) must exist in DB
	membersFilter := nostr.Filter{
		Kinds: []nostr.Kind{nostr.KindSimpleGroupMembers},
		Tags:  nostr.TagMap{"d": []string{"general"}},
	}
	memberEvents := 0
	for range instance.Events.QueryEvents(membersFilter, 0) {
		memberEvents++
	}
	if memberEvents == 0 {
		t.Error("members list event (kind 39002) not found after group creation")
	}

	// Admins list (kind 39001) must exist in DB
	adminsFilter := nostr.Filter{
		Kinds: []nostr.Kind{nostr.KindSimpleGroupAdmins},
		Tags:  nostr.TagMap{"d": []string{"general"}},
	}
	adminEvents := 0
	for range instance.Events.QueryEvents(adminsFilter, 0) {
		adminEvents++
	}
	if adminEvents == 0 {
		t.Error("admins list event (kind 39001) not found after group creation")
	}
}

// TestOnEventSaved_CreateGroup_MetadataSurvivesRestart verifies that after
// a simulated restart (fresh GroupStore + WarmCaches), the group's metadata
// is still found. This is the exact scenario that caused the "group not found" bug.
func TestOnEventSaved_CreateGroup_MetadataSurvivesRestart(t *testing.T) {
	instance := createTestInstance()
	creatorSecret := nostr.Generate()

	createEvent := nostr.Event{
		Kind:      nostr.KindSimpleGroupCreateGroup,
		CreatedAt: nostr.Now(),
		PubKey:    creatorSecret.Public(),
		Tags:      nostr.Tags{{"h", "persist"}},
		Content:   `{"name":"Persistent Group"}`,
	}
	createEvent.Sign(creatorSecret)
	instance.Events.SaveEvent(createEvent)

	instance.OnEventSaved(context.Background(), createEvent)

	// Simulate restart: fresh GroupStore reading from the same DB
	groups2 := &GroupStore{
		Config:     instance.Config,
		Events:     instance.Events,
		Management: instance.Management,
	}
	groups2.WarmCaches()

	meta, found := groups2.GetMetadata("persist")
	if !found {
		t.Fatal("metadata not found after simulated restart — the bug that broke 'general'")
	}
	if meta.Content != `{"name":"Persistent Group"}` {
		t.Errorf("metadata content = %q, want %q", meta.Content, `{"name":"Persistent Group"}`)
	}

	if !groups2.IsMember("persist", creatorSecret.Public()) {
		t.Error("creator not a member after simulated restart")
	}
}

// TestOnEventSaved_CreateGroup_MemberCount verifies that the metadata event
// produced by group creation includes an accurate member_count tag.
func TestOnEventSaved_CreateGroup_MemberCount(t *testing.T) {
	instance := createTestInstance()
	creatorSecret := nostr.Generate()

	createEvent := nostr.Event{
		Kind:      nostr.KindSimpleGroupCreateGroup,
		CreatedAt: nostr.Now(),
		PubKey:    creatorSecret.Public(),
		Tags:      nostr.Tags{{"h", "counted"}},
		Content:   `{"name":"Counted"}`,
	}
	createEvent.Sign(creatorSecret)
	instance.Events.SaveEvent(createEvent)

	instance.OnEventSaved(context.Background(), createEvent)

	meta, found := instance.Groups.GetMetadata("counted")
	if !found {
		t.Fatal("metadata not found after group creation")
	}

	memberCount := findTagValue(meta.Tags, "member_count")
	if memberCount != "1" {
		t.Errorf("member_count = %q after creation, want %q", memberCount, "1")
	}
}

// TestOnEventSaved_JoinRequest updates membership and member_count.
func TestOnEventSaved_JoinRequest(t *testing.T) {
	instance := createTestInstance()
	creatorSecret := nostr.Generate()

	// Create group first
	createEvent := nostr.Event{
		Kind:      nostr.KindSimpleGroupCreateGroup,
		CreatedAt: nostr.Now(),
		PubKey:    creatorSecret.Public(),
		Tags:      nostr.Tags{{"h", "jointest"}},
		Content:   `{"name":"Join Test"}`,
	}
	createEvent.Sign(creatorSecret)
	instance.Events.SaveEvent(createEvent)
	instance.OnEventSaved(context.Background(), createEvent)

	// New user joins (auto_join enabled)
	joinerSecret := nostr.Generate()
	joiner := joinerSecret.Public()
	joinEvent := nostr.Event{
		Kind:      nostr.KindSimpleGroupJoinRequest,
		CreatedAt: nostr.Now(),
		PubKey:    joiner,
		Tags:      nostr.Tags{{"h", "jointest"}},
	}
	joinEvent.Sign(joinerSecret)
	instance.Events.SaveEvent(joinEvent)
	instance.OnEventSaved(context.Background(), joinEvent)

	if !instance.Groups.IsMember("jointest", joiner) {
		t.Error("joiner should be a member after auto-join")
	}

	meta, _ := instance.Groups.GetMetadata("jointest")
	memberCount := findTagValue(meta.Tags, "member_count")
	if memberCount != "2" {
		t.Errorf("member_count = %q after join, want %q", memberCount, "2")
	}
}

// TestOnEventSaved_EditMetadata updates the metadata and preserves member_count.
func TestOnEventSaved_EditMetadata(t *testing.T) {
	instance := createTestInstance()
	creatorSecret := nostr.Generate()

	createEvent := nostr.Event{
		Kind:      nostr.KindSimpleGroupCreateGroup,
		CreatedAt: nostr.Now(),
		PubKey:    creatorSecret.Public(),
		Tags:      nostr.Tags{{"h", "edittest"}},
		Content:   `{"name":"Before Edit"}`,
	}
	createEvent.Sign(creatorSecret)
	instance.Events.SaveEvent(createEvent)
	instance.OnEventSaved(context.Background(), createEvent)

	// Edit metadata
	editEvent := nostr.Event{
		Kind:      nostr.KindSimpleGroupEditMetadata,
		CreatedAt: nostr.Now(),
		PubKey:    creatorSecret.Public(),
		Tags:      nostr.Tags{{"h", "edittest"}},
		Content:   `{"name":"After Edit"}`,
	}
	editEvent.Sign(creatorSecret)
	instance.Events.SaveEvent(editEvent)
	instance.OnEventSaved(context.Background(), editEvent)

	meta, found := instance.Groups.GetMetadata("edittest")
	if !found {
		t.Fatal("metadata not found after edit")
	}
	if meta.Content != `{"name":"After Edit"}` {
		t.Errorf("metadata content = %q, want %q", meta.Content, `{"name":"After Edit"}`)
	}

	// member_count should still be present
	memberCount := findTagValue(meta.Tags, "member_count")
	if memberCount != "1" {
		t.Errorf("member_count = %q after edit, want %q", memberCount, "1")
	}
}

// TestWarmCaches_SelfHeals_MissingMetadata verifies that WarmCaches regenerates
// the kind 39000 metadata event when only the kind 9007 creation event exists.
// This is the recovery path for the bug that broke the "general" group.
func TestWarmCaches_SelfHeals_MissingMetadata(t *testing.T) {
	groups, _ := createTestGroupStore()

	// Simulate the broken state: store a creation event but NO metadata event.
	creatorSecret := nostr.Generate()
	creator := creatorSecret.Public()
	createEvent := nostr.Event{
		Kind:      nostr.KindSimpleGroupCreateGroup,
		CreatedAt: nostr.Now(),
		PubKey:    creator,
		Tags:      nostr.Tags{{"h", "broken"}},
		Content:   `{"name":"Broken Group"}`,
	}
	createEvent.Sign(creatorSecret)
	groups.Events.SaveEvent(createEvent)

	// Verify the broken state: no metadata in DB
	metaFilter := nostr.Filter{
		Kinds: []nostr.Kind{nostr.KindSimpleGroupMetadata},
		Tags:  nostr.TagMap{"d": []string{"broken"}},
	}
	count := 0
	for range groups.Events.QueryEvents(metaFilter, 0) {
		count++
	}
	if count != 0 {
		t.Fatal("test setup: metadata event should not exist yet")
	}

	// WarmCaches should detect the missing metadata and regenerate it
	groups.WarmCaches()

	meta, found := groups.GetMetadata("broken")
	if !found {
		t.Fatal("WarmCaches did not self-heal: metadata still missing for group with creation event")
	}
	if meta.Content != `{"name":"Broken Group"}` {
		t.Errorf("self-healed metadata content = %q, want %q", meta.Content, `{"name":"Broken Group"}`)
	}

	// Verify metadata was persisted to DB (survives another restart)
	groups2 := &GroupStore{
		Config:     groups.Config,
		Events:     groups.Events,
		Management: groups.Management,
	}
	groups2.WarmCaches()

	_, found = groups2.GetMetadata("broken")
	if !found {
		t.Fatal("self-healed metadata did not persist to DB")
	}
}

// TestOnEventSaved_LeaveRequest removes member and updates count.
func TestOnEventSaved_LeaveRequest(t *testing.T) {
	instance := createTestInstance()
	creatorSecret := nostr.Generate()
	creator := creatorSecret.Public()

	// Create group
	createEvent := nostr.Event{
		Kind:      nostr.KindSimpleGroupCreateGroup,
		CreatedAt: nostr.Now(),
		PubKey:    creator,
		Tags:      nostr.Tags{{"h", "leavetest"}},
		Content:   `{"name":"Leave Test"}`,
	}
	createEvent.Sign(creatorSecret)
	instance.Events.SaveEvent(createEvent)
	instance.OnEventSaved(context.Background(), createEvent)

	// Add a second member via join
	joinerSecret := nostr.Generate()
	joiner := joinerSecret.Public()
	joinEvent := nostr.Event{
		Kind:      nostr.KindSimpleGroupJoinRequest,
		CreatedAt: nostr.Now(),
		PubKey:    joiner,
		Tags:      nostr.Tags{{"h", "leavetest"}},
	}
	joinEvent.Sign(joinerSecret)
	instance.Events.SaveEvent(joinEvent)
	instance.OnEventSaved(context.Background(), joinEvent)

	// Joiner leaves
	leaveEvent := nostr.Event{
		Kind:      nostr.KindSimpleGroupLeaveRequest,
		CreatedAt: nostr.Now(),
		PubKey:    joiner,
		Tags:      nostr.Tags{{"h", "leavetest"}},
	}
	leaveEvent.Sign(joinerSecret)
	instance.Events.SaveEvent(leaveEvent)
	instance.OnEventSaved(context.Background(), leaveEvent)

	if instance.Groups.IsMember("leavetest", joiner) {
		t.Error("joiner should not be a member after leaving")
	}

	meta, _ := instance.Groups.GetMetadata("leavetest")
	memberCount := findTagValue(meta.Tags, "member_count")
	if memberCount != "1" {
		t.Errorf("member_count = %q after leave, want %q", memberCount, "1")
	}
}
