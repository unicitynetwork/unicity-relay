package zooid

import (
	"os"
	"testing"
)

func TestEnvInt_DefaultValue(t *testing.T) {
	result := envInt("NONEXISTENT_KEY_FOR_TEST", 42)
	if result != 42 {
		t.Errorf("envInt() = %d, want 42", result)
	}
}

func TestEnvInt_FromEnv(t *testing.T) {
	os.Setenv("TEST_ENV_INT", "100")
	defer os.Unsetenv("TEST_ENV_INT")

	// Reset env cache so the new value is picked up
	envOnce.Do(func() {})
	env["TEST_ENV_INT"] = "100"

	result := envInt("TEST_ENV_INT", 42)
	if result != 100 {
		t.Errorf("envInt() = %d, want 100", result)
	}
}

func TestEnvInt_InvalidValue(t *testing.T) {
	env["TEST_ENV_INT_BAD"] = "notanumber"
	defer delete(env, "TEST_ENV_INT_BAD")

	result := envInt("TEST_ENV_INT_BAD", 42)
	if result != 42 {
		t.Errorf("envInt() with invalid value = %d, want fallback 42", result)
	}
}
