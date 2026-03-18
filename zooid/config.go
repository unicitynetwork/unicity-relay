package zooid

import (
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"fiatjaf.com/nostr"
	"github.com/BurntSushi/toml"
)


type Role struct {
	Pubkeys   []string `toml:"pubkeys"`
	CanInvite bool     `toml:"can_invite"`
	CanManage bool     `toml:"can_manage"`
}

type Config struct {
	Host   string `toml:"host"`
	Schema string `toml:"schema"`
	Secret string `toml:"secret"`
	Info   struct {
		Name        string `toml:"name"`
		Icon        string `toml:"icon"`
		Pubkey      string `toml:"pubkey"`
		Description string `toml:"description"`
	} `toml:"info"`

	Policy struct {
		Open            bool `toml:"open"`             // Allow all authenticated users (no membership required)
		PublicJoin      bool `toml:"public_join"`
		StripSignatures bool `toml:"strip_signatures"`
	} `toml:"policy"`

	Groups struct {
		Enabled                 bool `toml:"enabled"`
		AutoJoin                bool `toml:"auto_join"`
		AdminCreateOnly         bool `toml:"admin_create_only"`          // Only admins can create groups
		PrivateAdminOnly        bool `toml:"private_admin_only"`         // Only admins can create private groups
		PrivateRelayAdminAccess bool `toml:"private_relay_admin_access"` // Relay admins can see and moderate private groups
		Retention               struct {
			Default string            `toml:"default"` // Default retention duration (e.g. "7d", "24h"); empty = unlimited
			Groups  map[string]string `toml:"groups"`  // Per-group retention overrides keyed by group ID
		} `toml:"retention"`
	} `toml:"groups"`

	Management struct {
		Enabled bool     `toml:"enabled"`
		Methods []string `toml:"methods"`
	} `toml:"management"`

	Blossom struct {
		Enabled bool `toml:"enabled"`
	} `toml:"blossom"`

	Roles map[string]Role `toml:"roles"`

	// Private/parsed values
	path   string
	secret nostr.SecretKey
}

func LoadConfig(filename string) (*Config, error) {
	path := filepath.Join(Env("CONFIG"), filename)

	var config Config
	if _, err := toml.DecodeFile(path, &config); err != nil {
		return nil, fmt.Errorf("Failed to parse config file %s: %w", path, err)
	}

	if config.Host == "" {
		return nil, fmt.Errorf("host is required")
	}

	if config.Schema == "" {
		return nil, fmt.Errorf("schema is required")
	}

	// Validate retention config early so operators get immediate feedback
	if err := config.validateRetention(); err != nil {
		return nil, fmt.Errorf("invalid retention config in %s: %w", path, err)
	}

	secret, err := nostr.SecretKeyFromHex(config.Secret)
	if err != nil {
		return nil, err
	}

	// Save the path for later
	config.path = path

	// Make the secret... secret
	config.Secret = ""
	config.secret = secret

	return &config, nil
}

func (config *Config) Save() error {
	// Restore the secret key to the public field for saving
	config.Secret = config.secret.Hex()

	file, err := os.Create(config.path)
	if err != nil {
		return fmt.Errorf("Failed to open config file %s: %w", config.path, err)
	}
	defer file.Close()

	encoder := toml.NewEncoder(file)
	if err := encoder.Encode(config); err != nil {
		return fmt.Errorf("Failed to encode config file %s: %w", config.path, err)
	}

	// Clear the secret again
	config.Secret = ""

	return nil
}

func (config *Config) SetName(name string) error {
	config.Info.Name = name

	return config.Save()
}

func (config *Config) SetDescription(description string) error {
	config.Info.Description = description

	return config.Save()
}

func (config *Config) SetIcon(icon string) error {
	config.Info.Icon = icon

	return config.Save()
}

func (config *Config) Sign(event *nostr.Event) error {
	return event.Sign(config.secret)
}

func (config *Config) GetSelf() nostr.PubKey {
	return config.secret.Public()
}

func (config *Config) IsSelf(pubkey nostr.PubKey) bool {
	return pubkey == config.GetSelf()
}

func (config *Config) GetOwner() nostr.PubKey {
	return nostr.MustPubKeyFromHex(config.Info.Pubkey)
}

func (config *Config) IsOwner(pubkey nostr.PubKey) bool {
	return pubkey == config.GetOwner()
}

func (config *Config) GetAssignedRoles(pubkey nostr.PubKey) []Role {
	roles := make([]Role, 0)
	for _, role := range config.Roles {
		if slices.Contains(role.Pubkeys, pubkey.Hex()) {
			roles = append(roles, role)
		}
	}

	return roles
}

func (config *Config) GetAllRoles(pubkey nostr.PubKey) []Role {
	roles := make([]Role, 0)
	for name, role := range config.Roles {
		if name == "member" {
			roles = append(roles, role)
		} else if slices.Contains(role.Pubkeys, pubkey.Hex()) {
			roles = append(roles, role)
		}
	}

	return roles
}

func (config *Config) CanInvite(pubkey nostr.PubKey) bool {
	if config.IsOwner(pubkey) || config.IsSelf(pubkey) {
		return true
	}

	for _, role := range config.GetAllRoles(pubkey) {
		if role.CanInvite {
			return true
		}
	}

	return false
}

func (config *Config) CanManage(pubkey nostr.PubKey) bool {
	if config.IsOwner(pubkey) || config.IsSelf(pubkey) {
		return true
	}

	for _, role := range config.GetAllRoles(pubkey) {
		if role.CanManage {
			return true
		}
	}

	return false
}

// ParseRetentionDuration parses a retention duration string like "30s", "5m", "24h", "7d".
// Returns 0 for empty strings (meaning unlimited). Supports s(econds), m(inutes), h(ours), d(ays).
func ParseRetentionDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}

	if len(s) < 2 {
		return 0, fmt.Errorf("invalid retention duration: %q", s)
	}

	unit := s[len(s)-1]
	valueStr := s[:len(s)-1]

	value, err := strconv.ParseInt(valueStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid retention duration: %q", s)
	}

	if value <= 0 {
		return 0, fmt.Errorf("retention duration must be positive: %q", s)
	}

	var multiplier time.Duration
	switch unit {
	case 's':
		multiplier = time.Second
	case 'm':
		multiplier = time.Minute
	case 'h':
		multiplier = time.Hour
	case 'd':
		multiplier = 24 * time.Hour
	default:
		return 0, fmt.Errorf("invalid retention duration unit %q in %q (use s, m, h, or d)", string(unit), s)
	}

	// Guard against int64 overflow: max time.Duration is ~292 years in nanoseconds.
	maxValue := math.MaxInt64 / int64(multiplier)
	if value > maxValue {
		return 0, fmt.Errorf("retention duration too large: %q", s)
	}

	return time.Duration(value) * multiplier, nil
}

// validateRetention checks all retention duration strings at config load time.
func (config *Config) validateRetention() error {
	if config.Groups.Retention.Default != "" {
		if _, err := ParseRetentionDuration(config.Groups.Retention.Default); err != nil {
			return fmt.Errorf("default: %w", err)
		}
	}
	for groupID, s := range config.Groups.Retention.Groups {
		if _, err := ParseRetentionDuration(s); err != nil {
			return fmt.Errorf("group %q: %w", groupID, err)
		}
	}
	return nil
}

// HasRetention returns true if any effective retention policy is configured
// (a non-empty default or at least one per-group override that parses to a positive duration).
func (config *Config) HasRetention() bool {
	if d, err := ParseRetentionDuration(config.Groups.Retention.Default); err == nil && d > 0 {
		return true
	}
	for _, s := range config.Groups.Retention.Groups {
		if d, err := ParseRetentionDuration(s); err == nil && d > 0 {
			return true
		}
	}
	return false
}

// GetRetention returns the retention duration for a group. Per-group overrides
// take precedence over the default. Returns 0 (unlimited) if no retention is configured.
// Since values are validated at config load time, parse errors here are unexpected
// and logged as warnings.
func (config *Config) GetRetention(groupID string) time.Duration {
	if config.Groups.Retention.Groups != nil {
		if s, ok := config.Groups.Retention.Groups[groupID]; ok {
			d, err := ParseRetentionDuration(s)
			if err != nil {
				log.Printf("retention: unexpected invalid duration for group %q: %v", groupID, err)
			} else if d > 0 {
				return d
			}
		}
	}

	d, err := ParseRetentionDuration(config.Groups.Retention.Default)
	if err != nil {
		log.Printf("retention: unexpected invalid default duration: %v", err)
		return 0
	}
	return d
}
