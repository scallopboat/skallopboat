package main

import "testing"

func TestHomeDir(t *testing.T) {
	total := homeDir()

	if total == "" {
		t.Errorf("Sum was incorrect, got: %s, want: %d.", total, 10)
	}
}
