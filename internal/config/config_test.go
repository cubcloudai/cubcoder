package config

import "testing"

func TestLoadMaxIters(t *testing.T) {
	// Point at a path with no config file so loadFile() can't pull in a real
	// ~/.cubcoder/config.json and skew the precedence checks.
	t.Setenv("CUBCODER_CONFIG", "/nonexistent/cubcoder-config.json")
	t.Setenv("CUBCODER_MAX_ITERS", "")

	if got := Load("", "", "", "", 0, 0, 0, 0, false).MaxIters; got != DefaultMaxIters {
		t.Errorf("default MaxIters = %d, want %d", got, DefaultMaxIters)
	}
	if got := Load("", "", "", "", 0, 0, 42, 0, false).MaxIters; got != 42 {
		t.Errorf("explicit MaxIters = %d, want 42", got)
	}

	t.Setenv("CUBCODER_MAX_ITERS", "77")
	if got := Load("", "", "", "", 0, 0, 0, 0, false).MaxIters; got != 77 {
		t.Errorf("env MaxIters = %d, want 77", got)
	}
	if got := Load("", "", "", "", 0, 0, 9, 0, false).MaxIters; got != 9 {
		t.Errorf("explicit arg should beat env: got %d, want 9", got)
	}
}
