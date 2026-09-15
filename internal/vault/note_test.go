package vault

import (
	"strings"
	"testing"
)

const note = `---
type: daily
---

# 2026-09-12

## 📥 Inbox
- a jot

## 📓 Log
- 08:30 · brief ran

### 🛍️ Market
- routine output
`

func TestSectionMatchesIgnoringEmojiAndCase(t *testing.T) {
	_, body := SplitFrontmatter(note)
	lines := Lines(body)
	sec, ok := Section(lines, "log")
	if !ok {
		t.Fatal("heading text must match without emoji or case")
	}
	if !strings.Contains(strings.Join(sec, "\n"), "🛍️ Market") {
		t.Fatal("an H2 owns the H3s nested under it")
	}
	if _, ok := Section(lines, "Inbox"); !ok {
		t.Fatal("inbox must match")
	}
	if _, ok := Section(lines, "Nope"); ok {
		t.Fatal("a missing section must not match")
	}
}

func TestBulletsStopAtAnyHeading(t *testing.T) {
	lines := Lines(note)
	if b := Bullets(lines, "Log"); len(b) != 1 || !strings.Contains(b[0], "brief ran") {
		t.Fatalf("routine sub-section bullets must not be swept in: %v", b)
	}
}

func TestFrontmatterSplit(t *testing.T) {
	front, body := SplitFrontmatter(note)
	if !strings.HasPrefix(front, "---\n") || strings.Contains(body, "type: daily") {
		t.Fatalf("frontmatter split: %q / %q", front, body)
	}
	if f, b := SplitFrontmatter("# no frontmatter\n"); f != "" || b != "# no frontmatter\n" {
		t.Fatalf("a note without frontmatter passes through: %q %q", f, b)
	}
}
