package loader

import (
	"fmt"
	"strings"

	md "github.com/jalberto/qilla/internal/vault"
)

// JournalLogTail is how many 📓 Log bullets of a day survive compaction.
const JournalLogTail = 25

// CompactJournal reduces a daily note to what a routine actually reads: the
// 📥 Inbox bullets, the ✅ Today bullets and the tail of the 📓 Log. The 📖 Day
// narrative and the routine sub-sections appended below it are dropped — they
// are the bulk of a mature daily note and the model already wrote them.
func CompactJournal(note string) string {
	_, body := md.SplitFrontmatter(note)
	lines := md.Lines(body)
	var b strings.Builder
	block := func(heading string, bullets []string) {
		if len(bullets) == 0 {
			return
		}
		fmt.Fprintf(&b, "## %s\n%s\n", heading, strings.Join(bullets, "\n"))
	}
	block("📥 Inbox", md.Bullets(lines, "Inbox"))
	block("✅ Today", md.Bullets(lines, "Today"))
	logs := md.Bullets(lines, "Log")
	if n := len(logs); n > JournalLogTail {
		logs = logs[n-JournalLogTail:]
	}
	block(fmt.Sprintf("📓 Log (last %d)", len(logs)), logs)
	return strings.TrimRight(b.String(), "\n")
}
