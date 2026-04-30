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
