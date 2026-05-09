package zooid

import (
	"sync/atomic"
	"testing"
	"time"

	"fiatjaf.com/nostr"
)

func TestGetGroupIDFromEvent(t *testing.T) {
	tests := []struct {
		name string
		tags nostr.Tags
		want string
	}{
		{
			name: "with h tag",
			tags: nostr.Tags{{"h", "group123"}},
			want: "group123",
		},
		{
			name: "without h tag",
			tags: nostr.Tags{{"p", "pubkey123"}},
			want: "",
		},
		{
			name: "empty tags",
			tags: nostr.Tags{},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := nostr.Event{Tags: tt.tags}
			result := GetGroupIDFromEvent(event)
			if result != tt.want {
				t.Errorf("GetGroupIDFromEvent() = %v, want %v", result, tt.want)
			}
		})
	}
}

func TestGetInviteCodeFromEvent(t *testing.T) {
	tests := []struct {
		name string
		tags nostr.Tags
		want string
	}{
		{
			name: "with code tag",
			tags: nostr.Tags{{"h", "group123"}, {"code", "abc123"}},
			want: "abc123",
		},
		{
			name: "code tag without value",
			tags: nostr.Tags{{"code"}},
			want: "",
		},
		{
			name: "without code tag",
			tags: nostr.Tags{{"h", "group123"}},
			want: "",
		},
		{
			name: "empty tags",
			tags: nostr.Tags{},
			want: "",
		},
		{
			name: "code tag with empty value",
			tags: nostr.Tags{{"code", ""}},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := nostr.Event{Tags: tt.tags}
			result := GetInviteCodeFromEvent(event)
			if result != tt.want {
				t.Errorf("GetInviteCodeFromEvent() = %v, want %v", result, tt.want)
			}
		})
	}
}

func TestIsWriteRestrictedGroupContentFunc(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{"write-restricted true", `{"name":"Test","write-restricted":true}`, true},
		{"write-restricted false", `{"name":"Test","write-restricted":false}`, false},
		{"no field", `{"name":"Test"}`, false},
		{"empty", "", false},
		{"invalid JSON", "not json", false},
		{"string type", `{"write-restricted":"true"}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isWriteRestrictedGroupContent(tt.content)
			if result != tt.want {
				t.Errorf("isWriteRestrictedGroupContent(%q) = %v, want %v", tt.content, result, tt.want)
			}
		})
	}
}

func TestIsPrivateGroupContent(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{
			name:    "private true",
			content: `{"name": "Test Group", "private": true}`,
			want:    true,
		},
		{
			name:    "private false",
			content: `{"name": "Test Group", "private": false}`,
			want:    false,
		},
		{
			name:    "no private field",
			content: `{"name": "Test Group"}`,
			want:    false,
		},
		{
			name:    "empty content",
			content: "",
			want:    false,
		},
		{
			name:    "invalid JSON",
			content: "not json",
			want:    false,
		},
		{
			name:    "private as string (invalid type)",
			content: `{"name": "Test Group", "private": "true"}`,
			want:    false,
		},
		{
			name:    "empty object",
			content: `{}`,
			want:    false,
		},
		{
			name:    "private with other fields",
			content: `{"name": "Secret Group", "about": "A secret group", "private": true, "closed": true, "hidden": true}`,
			want:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isPrivateGroupContent(tt.content)
			if result != tt.want {
				t.Errorf("isPrivateGroupContent(%q) = %v, want %v", tt.content, result, tt.want)
			}
		})
	}
}

// TestGroupStore_ScheduleRewrite_CoalescesBurst verifies the leading-edge
// debounce: many rapid calls collapse into a single fn invocation that runs
// after DebounceDelay, and a fresh burst arms a new timer.
func TestGroupStore_ScheduleRewrite_CoalescesBurst(t *testing.T) {
	g := &GroupStore{DebounceDelay: 30 * time.Millisecond}

	var calls atomic.Int32
	fn := func() { calls.Add(1) }

	for range 50 {
		g.scheduleRewrite("members:group1", fn)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("calls before delay = %d, want 0", got)
	}

	time.Sleep(80 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("calls after first burst = %d, want 1", got)
	}

	// Fresh burst after the timer fired must arm a new run.
	for range 20 {
		g.scheduleRewrite("members:group1", fn)
	}
	time.Sleep(80 * time.Millisecond)
	if got := calls.Load(); got != 2 {
		t.Errorf("calls after second burst = %d, want 2", got)
	}
}

// TestGroupStore_ScheduleRewrite_PerKeyIsolation verifies different debounce
// keys (e.g. members:g1 vs count:g1, or members:g1 vs members:g2) don't share
// pending slots.
func TestGroupStore_ScheduleRewrite_PerKeyIsolation(t *testing.T) {
	g := &GroupStore{DebounceDelay: 30 * time.Millisecond}

	var calls atomic.Int32
	fn := func() { calls.Add(1) }

	g.scheduleRewrite("members:g1", fn)
	g.scheduleRewrite("members:g2", fn)
	g.scheduleRewrite("count:g1", fn)
	// All three should run independently.
	time.Sleep(80 * time.Millisecond)
	if got := calls.Load(); got != 3 {
		t.Errorf("calls = %d, want 3 (one per distinct key)", got)
	}
}

// TestGroupStore_ScheduleRewrite_RerunIfDirtyDuringRun verifies that a
// Schedule call arriving while fn() is running flags the entry dirty, and
// after fn() completes the runner picks the flag up and re-invokes fn()
// once. This prevents overlapping SERIALIZABLE rewrites (Copilot review on
// PR #17) without dropping updates that race with fn's cache read.
func TestGroupStore_ScheduleRewrite_RerunIfDirtyDuringRun(t *testing.T) {
	g := &GroupStore{DebounceDelay: 10 * time.Millisecond}

	var calls atomic.Int32
	block := make(chan struct{})
	fn := func() {
		if calls.Add(1) == 1 {
			<-block // first invocation blocks until the test releases it
		}
	}

	g.scheduleRewrite("members:g1", fn)

	// Wait for fn() to start.
	deadline := time.Now().Add(200 * time.Millisecond)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if calls.Load() != 1 {
		t.Fatalf("first fn invocation didn't start: calls = %d", calls.Load())
	}

	// Schedule again while fn() is still blocked. With the old delete-before-fn
	// design this would arm a new timer (overlap); with the dirty-flag design
	// it should mark the in-flight entry dirty for a follow-up run.
	g.scheduleRewrite("members:g1", fn)

	// Release fn(). The runner should observe dirty and call fn() once more.
	close(block)

	deadline = time.Now().Add(200 * time.Millisecond)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("dirty re-run didn't fire: calls = %d, want 2", got)
	}

	// And the loop must terminate — no further calls without new schedules.
	time.Sleep(50 * time.Millisecond)
	if got := calls.Load(); got != 2 {
		t.Errorf("runner kept looping: calls = %d, want stable at 2", got)
	}
}

// TestGroupStore_ScheduleMembersList_SyncWhenDelayZero verifies that
// DebounceDelay = 0 (the test default) preserves the synchronous semantics
// existing tests rely on — Schedule* runs the underlying op inline and
// returns its error directly.
func TestGroupStore_ScheduleMembersList_SyncWhenDelayZero(t *testing.T) {
	g := &GroupStore{DebounceDelay: 0}
	// No Events store wired up: UpdateMembersList will reach SignAndStoreEvent
	// and panic. We only assert that scheduling delegates synchronously,
	// proven by the panic propagating to this goroutine.
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected synchronous call to panic from nil Events; got no panic")
		}
	}()
	_ = g.ScheduleMembersListUpdate("g1")
}

// TestGroupStore_WarmCaches_FromMembersSnapshot verifies that the warm-up
// path reads kind-39002 (members snapshot) and kind-39001 (admins
// snapshot) instead of replaying the kind-9000/9001 put/remove log.
// Issue #25.
//
// Two assertions:
//
//  1. Membership and roles loaded from snapshots end up in the caches.
//  2. A standalone kind-9000 put-user event NOT reflected in any 39002
//     is intentionally NOT in the warm cache — the live event handler
//     would pick it up post-startup, but the warm-up only reads
//     snapshots. Documents the lag-window tradeoff explicitly so a
//     future refactor doesn't quietly change it.
func TestGroupStore_WarmCaches_FromMembersSnapshot(t *testing.T) {
	inst := createTestInstance()
	const groupID = "general"

	relaySec := inst.Config.secret
	memberA := nostr.Generate().Public()
	memberB := nostr.Generate().Public()
	writerPK := nostr.Generate().Public()    // member with role on 39002
	adminOnlyPK := nostr.Generate().Public() // present in 39001 but not 39002 — must still be a member
	tailMember := nostr.Generate().Public()  // post-snapshot put

	mkAndSave := func(kind nostr.Kind, tags nostr.Tags) {
		evt := nostr.Event{
			Kind:      kind,
			CreatedAt: nostr.Now(),
			PubKey:    relaySec.Public(),
			Tags:      tags,
		}
		evt.Sign(relaySec)
		if err := inst.Events.SaveEvent(evt); err != nil {
			t.Fatalf("SaveEvent(kind=%d): %v", kind, err)
		}
	}

	// kind-39002 (members) — UpdateMembersList shape: roles ride on
	// the per-member p-tag at positions 2+.
	mkAndSave(nostr.KindSimpleGroupMembers, nostr.Tags{
		{"d", groupID},
		{"p", memberA.Hex()},
		{"p", memberB.Hex()},
		{"p", writerPK.Hex(), "writer"},
	})
	// kind-39001 (admins) — UpdateAdminsList shape: just `{p, pubkey}`,
	// no role positions. WarmCaches uses this only to ensure listed
	// admins are visible as members even if the 39002 snapshot is
	// stale and missed them.
	mkAndSave(nostr.KindSimpleGroupAdmins, nostr.Tags{
		{"d", groupID},
		{"p", adminOnlyPK.Hex()},
	})
	// Tail-of-log put-user that the snapshot doesn't cover yet.
	// Live updates post-startup would pull this in via the event
	// handler; warm-up should NOT see it.
	mkAndSave(nostr.KindSimpleGroupPutUser, nostr.Tags{
		{"h", groupID},
		{"p", tailMember.Hex()},
	})

	// Re-warm caches against the just-saved fixtures.
	inst.Groups.membershipCache.Delete(groupID)
	inst.Groups.roleCache.Delete(groupID)
	inst.Groups.cachesWarmed = false
	inst.Groups.WarmCaches()

	if !inst.Groups.IsMember(groupID, memberA) {
		t.Errorf("memberA missing from cache after WarmCaches; should have been loaded from kind-39002")
	}
	if !inst.Groups.IsMember(groupID, memberB) {
		t.Errorf("memberB missing from cache")
	}
	if !inst.Groups.IsMember(groupID, writerPK) {
		t.Errorf("writerPK missing from cache; should have been loaded from kind-39002")
	}
	if !inst.Groups.HasRole(groupID, writerPK, "writer") {
		t.Errorf("writerPK role missing; roles ride on kind-39002 p-tag positions 2+")
	}
	if !inst.Groups.IsMember(groupID, adminOnlyPK) {
		t.Errorf("adminOnlyPK missing from cache; admins listed in kind-39001 are implicitly members and must be surfaced even if the 39002 snapshot didn't list them")
	}
	if inst.Groups.IsMember(groupID, tailMember) {
		t.Errorf("tailMember unexpectedly in cache; warm-up should read snapshots only, not tail of put/remove log")
	}
}

// TestGroupStore_WarmCaches_StaleAdminsSnapshotDoesNotOverride locks in
// the cross-kind staleness rule: a 39001 (admins) snapshot that's
// strictly older than the 39002 (members) snapshot for the same group
// must NOT add its listed pubkeys to the membership cache. Otherwise
// an admin who was demoted+removed (and so dropped from the newer
// 39002) would get re-added by the older 39001 — exactly the
// false-acceptance class we'd be trying to avoid in the false-rejection
// fix. Issue #25 follow-up review.
func TestGroupStore_WarmCaches_StaleAdminsSnapshotDoesNotOverride(t *testing.T) {
	inst := createTestInstance()
	const groupID = "stalegrp"

	relaySec := inst.Config.secret
	currentMember := nostr.Generate().Public()
	demotedAndRemoved := nostr.Generate().Public()

	mkAndSave := func(kind nostr.Kind, ts nostr.Timestamp, tags nostr.Tags) {
		evt := nostr.Event{
			Kind:      kind,
			CreatedAt: ts,
			PubKey:    relaySec.Public(),
			Tags:      tags,
		}
		evt.Sign(relaySec)
		if err := inst.Events.SaveEvent(evt); err != nil {
			t.Fatalf("SaveEvent(kind=%d): %v", kind, err)
		}
	}

	// Older 39001 with the demoted user still listed.
	mkAndSave(nostr.KindSimpleGroupAdmins, nostr.Timestamp(1000), nostr.Tags{
		{"d", groupID},
		{"p", demotedAndRemoved.Hex()},
	})
	// Newer 39002 reflecting current state — only the remaining member.
	mkAndSave(nostr.KindSimpleGroupMembers, nostr.Timestamp(2000), nostr.Tags{
		{"d", groupID},
		{"p", currentMember.Hex()},
	})

	inst.Groups.membershipCache.Delete(groupID)
	inst.Groups.roleCache.Delete(groupID)
	inst.Groups.cachesWarmed = false
	inst.Groups.WarmCaches()

	if !inst.Groups.IsMember(groupID, currentMember) {
		t.Errorf("currentMember missing from cache after WarmCaches")
	}
	if inst.Groups.IsMember(groupID, demotedAndRemoved) {
		t.Errorf("demotedAndRemoved is in cache; an older 39001 must not re-add a pubkey that the newer 39002 has dropped")
	}
}

// TestGroupStore_WarmCaches_StaysPreWarmWhenSnapshotReadsEmpty pins
// the heuristic that detects a catastrophic warm-up failure (e.g. the
// snapshot QueryEvents calls timing out under DB pressure): if the
// metadata cache shows we have groups but the members/admins reads
// returned nothing at all, leave cachesWarmed=false so IsMember keeps
// using its DB query fallback. Issue #25 follow-up review.
func TestGroupStore_WarmCaches_StaysPreWarmWhenSnapshotReadsEmpty(t *testing.T) {
	inst := createTestInstance()

	// Save a group metadata event but NO members/admins snapshots.
	// This simulates the failure mode where the metadata read
	// succeeded but the snapshot reads came back empty (timeout, db
	// outage, partial read).
	relaySec := inst.Config.secret
	metaEvent := nostr.Event{
		Kind:      nostr.KindSimpleGroupMetadata,
		CreatedAt: nostr.Now(),
		PubKey:    relaySec.Public(),
		Tags: nostr.Tags{
			{"d", "lonelygroup"},
			{"name", "Lonely"},
		},
	}
	metaEvent.Sign(relaySec)
	if err := inst.Events.SaveEvent(metaEvent); err != nil {
		t.Fatalf("SaveEvent metadata: %v", err)
	}

	// Reset everything so WarmCaches runs from scratch against the
	// just-saved fixture.
	inst.Groups.metadataCache.Range(func(k, _ any) bool {
		inst.Groups.metadataCache.Delete(k)
		return true
	})
	inst.Groups.membershipCache.Range(func(k, _ any) bool {
		inst.Groups.membershipCache.Delete(k)
		return true
	})
	inst.Groups.cachesWarmed = false

	inst.Groups.WarmCaches()

	if inst.Groups.cachesWarmed {
		t.Errorf("cachesWarmed unexpectedly true: metadata has groups but no membership snapshots were read; should stay in pre-warm mode so IsMember falls back to DB")
	}
}
