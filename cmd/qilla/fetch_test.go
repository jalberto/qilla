package main

import (
	"errors"
	"testing"
)

func TestCmdFetchUsage(t *testing.T) {
	for _, args := range [][]string{{}, {"--max-rung"}, {"--max-rung", "9", "u"}, {"--nope", "u"}} {
		if err := cmdFetch(args); !errors.Is(err, errUsage) {
			t.Fatalf("cmdFetch(%v) = %v, want a usage error", args, err)
		}
	}
}

// With nothing on PATH every rung is skipped: no content, so exit 3's error.
func TestCmdFetchExitThree(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	err := cmdFetch([]string{"https://example.invalid", "--max-rung", "2", "--json"})
	var blocked errBlocked
	if !errors.As(err, &blocked) {
		t.Fatalf("err = %v, want errBlocked", err)
	}
	if blocked.kind != "empty" {
		t.Fatalf("kind = %q, want empty", blocked.kind)
	}
}
