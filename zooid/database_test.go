package zooid

import (
	"testing"
)

func TestEnvInt_DefaultValue(t *testing.T) {
	result := envInt("NONEXISTENT_KEY_FOR_TEST", 42)
	if result != 42 {
		t.Errorf("envInt() = %d, want 42", result)
	}
}

func TestEnvInt_FromEnv(t *testing.T) {
	// Override env cache for this test and restore it afterward.
	prevVal, hadPrev := env["TEST_ENV_INT"]
	env["TEST_ENV_INT"] = "100"
	defer func() {
		if hadPrev {
			env["TEST_ENV_INT"] = prevVal
		} else {
			delete(env, "TEST_ENV_INT")
		}
	}()

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
