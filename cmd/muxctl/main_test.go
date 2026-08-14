package main

import "testing"

func TestHelpReturnsSuccess(t *testing.T) {
	code, err := run([]string{"-h"})
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("help exit code = %d, want 0", code)
	}
}
