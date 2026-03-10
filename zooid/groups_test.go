package zooid

import (
	"encoding/json"
	"testing"

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

func TestIsWriteRestrictedGroupContent(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{
			name:    "write-restricted true",
			content: `{"name": "Announcements", "write-restricted": true}`,
			want:    true,
		},
		{
			name:    "write-restricted false",
			content: `{"name": "Test", "write-restricted": false}`,
			want:    false,
		},
		{
			name:    "no write-restricted field",
			content: `{"name": "Test"}`,
			want:    false,
		},
		{
			name:    "write-restricted with closed",
			content: `{"name": "Announcements", "closed": true, "write-restricted": true}`,
			want:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var data map[string]interface{}
			json.Unmarshal([]byte(tt.content), &data)
			wr, ok := data["write-restricted"].(bool)
			result := ok && wr
			if result != tt.want {
				t.Errorf("write-restricted check for %q = %v, want %v", tt.content, result, tt.want)
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
