package zooid

import (
	"fmt"
	"testing"
	"time"
)

func TestParseRetentionDuration(t *testing.T) {
	tests := []struct {
		input    string
		expected time.Duration
		wantErr  bool
	}{
		{"", 0, false},
		{"30s", 30 * time.Second, false},
		{"5m", 5 * time.Minute, false},
		{"24h", 24 * time.Hour, false},
		{"7d", 7 * 24 * time.Hour, false},
		{"1d", 24 * time.Hour, false},
		{"  12h  ", 12 * time.Hour, false},
		// Edge: large but valid
		{"365d", 365 * 24 * time.Hour, false},
		// Invalid inputs
		{"abc", 0, true},
		{"10x", 0, true},
		{"h", 0, true},
		{"0d", 0, true},
		{"-5h", 0, true},
		{"10", 0, true},
		// Overflow: exceeds maxRetentionDays
		{"999999999d", 0, true},
		// Overflow: large values that would overflow int64 nanoseconds
		{"9999999999999999h", 0, true},
		{"99999999999999999s", 0, true},
	}

	for i, tt := range tests {
		name := tt.input
		if name == "" {
			name = fmt.Sprintf("#%d_empty", i)
		}
		t.Run(name, func(t *testing.T) {
			got, err := ParseRetentionDuration(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseRetentionDuration(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if got != tt.expected {
				t.Errorf("ParseRetentionDuration(%q) = %v, want %v", tt.input, got, tt.expected)
			}
		})
	}
}

func TestGetRetention(t *testing.T) {
	config := &Config{}

	// No retention configured — returns 0 (unlimited)
	if got := config.GetRetention("any-group"); got != 0 {
		t.Errorf("expected 0 for unconfigured retention, got %v", got)
	}

	// Set default only
	config.Groups.Retention.Default = "24h"
	if got := config.GetRetention("any-group"); got != 24*time.Hour {
		t.Errorf("expected 24h default, got %v", got)
	}

	// Set per-group override
	config.Groups.Retention.Groups = map[string]string{
		"short-lived": "1h",
	}

	// Per-group override takes precedence
	if got := config.GetRetention("short-lived"); got != 1*time.Hour {
		t.Errorf("expected 1h for short-lived group, got %v", got)
	}

	// Other groups fall back to default
	if got := config.GetRetention("other-group"); got != 24*time.Hour {
		t.Errorf("expected 24h default for other-group, got %v", got)
	}

	// Per-group with empty value falls back to default
	config.Groups.Retention.Groups["empty"] = ""
	if got := config.GetRetention("empty"); got != 24*time.Hour {
		t.Errorf("expected 24h default for empty override, got %v", got)
	}
}

func TestGetRetention_NoDefault(t *testing.T) {
	config := &Config{}
	config.Groups.Retention.Groups = map[string]string{
		"only-this": "2h",
	}
	if got := config.GetRetention("only-this"); got != 2*time.Hour {
		t.Errorf("expected 2h for only-this, got %v", got)
	}
	if got := config.GetRetention("other"); got != 0 {
		t.Errorf("expected 0 for group with no retention and no default, got %v", got)
	}
}

func TestHasRetention(t *testing.T) {
	config := &Config{}
	if config.HasRetention() {
		t.Error("expected HasRetention() == false for empty config")
	}

	config.Groups.Retention.Default = "7d"
	if !config.HasRetention() {
		t.Error("expected HasRetention() == true with default set")
	}

	config2 := &Config{}
	config2.Groups.Retention.Groups = map[string]string{"g": "1h"}
	if !config2.HasRetention() {
		t.Error("expected HasRetention() == true with per-group set")
	}

	// All per-group values empty — no effective retention
	config3 := &Config{}
	config3.Groups.Retention.Groups = map[string]string{"g": ""}
	if config3.HasRetention() {
		t.Error("expected HasRetention() == false when all per-group values are empty")
	}
}

func TestValidateRetention(t *testing.T) {
	// Valid config
	config := &Config{}
	config.Groups.Retention.Default = "7d"
	config.Groups.Retention.Groups = map[string]string{"g1": "1h", "g2": "30m"}
	if err := config.validateRetention(); err != nil {
		t.Errorf("expected valid config, got error: %v", err)
	}

	// Invalid default
	bad := &Config{}
	bad.Groups.Retention.Default = "bad"
	if err := bad.validateRetention(); err == nil {
		t.Error("expected error for invalid default retention")
	}

	// Invalid per-group
	bad2 := &Config{}
	bad2.Groups.Retention.Groups = map[string]string{"g1": "7dd"}
	if err := bad2.validateRetention(); err == nil {
		t.Error("expected error for invalid per-group retention")
	}

	// Empty config is valid
	empty := &Config{}
	if err := empty.validateRetention(); err != nil {
		t.Errorf("expected empty config to be valid, got error: %v", err)
	}
}
