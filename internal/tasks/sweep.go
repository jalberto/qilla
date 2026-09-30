package tasks

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// headingLine matches a level-2 "## Section" heading.
var headingLine = regexp.MustCompile(`^## (.+)$`)

// doneItem matches a top-level done task line: "- [x] " or "- [X] ".
var doneItem = regexp.MustCompile(`^- \[[xX]\] `)

// trailingID matches a line's own trailing block id — the LAST token on the
// line, not a "#^task-…" reference buried in a wikilink mid-line.
var trailingID = regexp.MustCompile(`\s+\^task-\d{8}-\d{3}\s*$`)

// archiveTemplate seeds a brand new archive file the first time Sweep needs
// one.
const archiveTemplate = "---\ntype: note\ntags:\n  - desk\n  - archive\n---\n\n" +
	"# Todo — archive\n\nDone items swept out of [[Desk/Todo]] by `qilla task sweep`.\n"

// isIndentedLine reports whether l is a non-blank continuation line: it
// starts with a space or a tab.
func isIndentedLine(l string) bool {
	return l != "" && (l[0] == ' ' || l[0] == '\t')
}

// markSection appends " · §<section>" to a done item's first line, before
// its trailing block id (if any) so the id stays the last token.
func markSection(line, section string) string {
	if section == "" {
		return line
	}
	mark := " · §" + section
	if loc := trailingID.FindStringIndex(line); loc != nil {
		before := line[:loc[0]]
		id := strings.TrimSpace(line[loc[0]:])
		return before + mark + " " + id
	}
	return strings.TrimRight(line, " \t\r") + mark
}

// splitDone walks lines top to bottom, pulling out every top-level done item
// together with its continuation lines (indented, or blank-then-indented).
// kept is the rest of the file, in order; blocks is one []string per done
// item, its first line stamped with the "## " section it was found under.
func splitDone(lines []string) (kept []string, blocks [][]string) {
	section := ""
	i := 0
	for i < len(lines) {
		line := lines[i]
		if m := headingLine.FindStringSubmatch(line); m != nil {
			section = strings.TrimSpace(m[1])
			kept = append(kept, line)
			i++
			continue
		}
		if doneItem.MatchString(line) {
			start := i
			i++
			for i < len(lines) {
				l := lines[i]
				if l == "" {
					j := i
					for j < len(lines) && lines[j] == "" {
						j++
					}
					if j < len(lines) && isIndentedLine(lines[j]) {
						i = j
						continue
					}
					break
				}
				if isIndentedLine(l) {
					i++
					continue
				}
				break
			}
			blk := append([]string(nil), lines[start:i]...)
			blk[0] = markSection(blk[0], section)
			blocks = append(blocks, blk)
			continue
		}
		kept = append(kept, line)
		i++
	}
	return kept, blocks
}

// appendToArchive appends blocks to the end of an archive file's lines,
// under a "## Swept <today>" heading — reusing that heading if it is already
// the last one in the file.
func appendToArchive(lines []string, today string, blocks [][]string) []string {
	heading := "Swept " + today
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	lastHeading := ""
	for i := len(lines) - 1; i >= 0; i-- {
		if m := headingLine.FindStringSubmatch(lines[i]); m != nil {
			lastHeading = strings.TrimSpace(m[1])
			break
		}
	}
	if lastHeading != heading {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, "## "+heading, "")
	} else {
		lines = append(lines, "")
	}
	for _, b := range blocks {
		lines = append(lines, b...)
	}
	return append(lines, "")
}

// archivePath is the vault-relative archive file next to todoRel:
// <dir of todoRel>/Archive/<base of todoRel>.
func archivePath(todoRel string) string {
	rel := filepath.ToSlash(todoRel)
	return path.Join(path.Dir(rel), "Archive", path.Base(rel))
}

// Sweep moves every done item (and its children) out of the vault-relative
// todo file into its Archive/ sibling, under a "## Swept YYYY-MM-DD" heading.
// Nothing is deleted: the archive is written first, then the todo file, both
// atomically. dryRun writes nothing and only reports what would move. It
// returns the number of items moved, the vault-relative archive path, and
// each moved item's own first line (stamped with its " · §Section"), for
// --dry-run to print.
func Sweep(root, todoRel string, now time.Time, dryRun bool) (int, string, []string, error) {
	todoAbs := filepath.Join(root, filepath.FromSlash(todoRel))
	lines, mode, err := readLines(todoAbs)
	if err != nil {
		return 0, "", nil, fmt.Errorf("no %s", todoRel)
	}

	kept, blocks := splitDone(lines)
	archiveRel := archivePath(todoRel)
	firstLines := make([]string, len(blocks))
	for i, b := range blocks {
		firstLines[i] = b[0]
	}
	if len(blocks) == 0 {
		return 0, archiveRel, nil, nil
	}
	if dryRun {
		return len(blocks), archiveRel, firstLines, nil
	}

	archiveAbs := filepath.Join(root, filepath.FromSlash(archiveRel))
	var archiveLines []string
	archiveMode := os.FileMode(0o644)
	if al, am, err := readLines(archiveAbs); err == nil {
		archiveLines, archiveMode = al, am
	} else if os.IsNotExist(err) {
		archiveLines = strings.Split(archiveTemplate, "\n")
	} else {
		return 0, "", nil, err
	}

	archiveLines = appendToArchive(archiveLines, now.Format("2006-01-02"), blocks)

	if err := os.MkdirAll(filepath.Dir(archiveAbs), 0o755); err != nil {
		return 0, "", nil, err
	}
	if err := writeAtomic(archiveAbs, strings.Join(archiveLines, "\n"), archiveMode); err != nil {
		return 0, "", nil, err
	}
	if err := writeAtomic(todoAbs, strings.Join(kept, "\n"), mode); err != nil {
		return 0, "", nil, err
	}
	return len(blocks), archiveRel, firstLines, nil
}
