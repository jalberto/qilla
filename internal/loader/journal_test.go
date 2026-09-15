package loader

import (
	"fmt"
	"strings"
	"testing"
)

func TestCompactJournalKeepsOnlyWhatRoutinesRead(t *testing.T) {
	var big strings.Builder
	big.WriteString("---\ntype: daily\n---\n\n# 2026-09-12\n\n## 📥 Inbox\n- a jot\n\n## ✅ Today\n- ship the brief\n\n## 📖 Day\n")
	big.WriteString(strings.Repeat("narrative prose nobody needs twice\n", 200))
	big.WriteString("\n## 📓 Log\n")
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&big, "- %02d:00 · log line %d\n", i%24, i)
	}
	big.WriteString("\n### 🛍️ Market · 2026-09-12\n")
	big.WriteString(strings.Repeat("routine output\n", 100))

	out := CompactJournal(big.String())
	if strings.Contains(out, "narrative prose") || strings.Contains(out, "routine output") {
		t.Fatalf("📖 Day and routine sections must be dropped:\n%s", out)
	}
	if !strings.Contains(out, "- a jot") || !strings.Contains(out, "- ship the brief") {
		t.Fatalf("inbox and today must survive:\n%s", out)
	}
	if strings.Contains(out, "log line 34") || !strings.Contains(out, "log line 59") {
		t.Fatalf("only the last %d log lines survive:\n%s", JournalLogTail, out)
	}
	if len(out) >= len(big.String())/4 {
		t.Fatalf("compaction must be a big win: %d → %d", len(big.String()), len(out))
	}
}
