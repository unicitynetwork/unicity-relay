package zooid

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
)

func TestLoadConfig_ValidatesPubkeys(t *testing.T) {
	valid := nostr.Generate().Public().Hex()
	npub := "npub1" + valid[:58]

	tests := []struct {
		name       string
		infoPubkey string
		rolePubkey string
		canManage  bool
		wantErr    string
	}{
		{name: "valid pubkeys", infoPubkey: valid, rolePubkey: valid, canManage: true},
		{name: "missing info.pubkey", infoPubkey: "", rolePubkey: valid, canManage: true, wantErr: "info.pubkey"},
		{name: "short info.pubkey", infoPubkey: valid[:10], rolePubkey: valid, canManage: true, wantErr: "info.pubkey"},
		{name: "npub in a role that can manage", infoPubkey: valid, rolePubkey: npub, canManage: true, wantErr: "roles.staff.pubkeys"},
		{name: "npub in a role that cannot manage", infoPubkey: valid, rolePubkey: npub, canManage: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name := "load-" + strings.ToLower(RandomString(8)) + ".toml"
			path := filepath.Join(Env("CONFIG"), name)
			content := fmt.Sprintf(`host = "localhost"
schema = "load"
secret = %q

[info]
pubkey = %q

[roles.staff]
pubkeys = [%q]
can_manage = %t
`, nostr.Generate().Hex(), tt.infoPubkey, tt.rolePubkey, tt.canManage)
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Remove(path) })

			config, err := LoadConfig(name)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("LoadConfig() error = %v, want none", err)
				}
				if config.GetOwner().Hex() != valid {
					t.Errorf("GetOwner() = %s, want %s", config.GetOwner().Hex(), valid)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("LoadConfig() error = %v, want one mentioning %q", err, tt.wantErr)
			}
		})
	}
}
