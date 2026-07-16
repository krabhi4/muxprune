package main

import "testing"

func TestEnvIntIn(t *testing.T) {
	t.Setenv("T_OK", "42")
	t.Setenv("T_LOW", "0")
	t.Setenv("T_HIGH", "99999999")
	t.Setenv("T_BAD", "nope")
	if got := envIntIn("T_OK", 7, 1, 100); got != 42 {
		t.Errorf("ok = %d, want 42", got)
	}
	if got := envIntIn("T_LOW", 7, 1, 100); got != 7 {
		t.Errorf("below min = %d, want default 7", got)
	}
	if got := envIntIn("T_HIGH", 7, 1, 100); got != 7 {
		t.Errorf("above max = %d, want default 7", got)
	}
	if got := envIntIn("T_BAD", 7, 1, 100); got != 7 {
		t.Errorf("malformed = %d, want default 7", got)
	}
	if got := envIntIn("T_UNSET", 7, 1, 100); got != 7 {
		t.Errorf("unset = %d, want default 7", got)
	}
}
