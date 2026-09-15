// Package vault parses the markdown shapes qilla reads out of the vault:
// headings, the bullets under a heading, and YAML frontmatter. It exists so
// `qilla note`, `qilla catchup` and the prompt loader agree on what "the 📓 Log
// section" means instead of each re-inventing an awk one-liner.
package vault

import (
	"strings"
	"unicode"
)

// Heading is one markdown heading and where its body lives.
type Heading struct {
	Level int    // 1..6
	Text  string // the raw heading text, emoji included
	Start int    // index of the heading line
	End   int    // index one past the last body line (next heading of any level)
}

// SplitFrontmatter returns the body with a leading `---` YAML block removed,
// plus the block itself (empty when there is none).
func SplitFrontmatter(s string) (front, body string) {
	if !strings.HasPrefix(s, "---\n") {
		return "", s
	}
	rest := s[len("---\n"):]
	i := strings.Index(rest, "\n---")
	if i < 0 {
		return "", s
	}
	end := i + len("\n---")
	// consume the rest of the closing line
	if j := strings.IndexByte(rest[end:], '\n'); j >= 0 {
		end += j + 1
	} else {
		end = len(rest)
	}
	return s[:len("---\n")+end], rest[end:]
}

// Lines splits text into lines without a trailing empty element.
func Lines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// headingLevel is 0 for a normal line.
func headingLevel(line string) (int, string) {
	n := 0
	for n < len(line) && line[n] == '#' {
		n++
	}
	if n == 0 || n > 6 || n >= len(line) || line[n] != ' ' {
		return 0, ""
	}
	return n, strings.TrimSpace(line[n+1:])
}

// Headings lists every heading of the note, each body running to the next
// heading of any level.
func Headings(lines []string) []Heading {
	var hs []Heading
	for i, l := range lines {
		lvl, txt := headingLevel(l)
		if lvl == 0 {
			continue
		}
		if n := len(hs); n > 0 {
			hs[n-1].End = i
		}
		hs = append(hs, Heading{Level: lvl, Text: txt, Start: i, End: len(lines)})
	}
	return hs
}

// Norm folds a heading for matching: emoji and punctuation dropped, lowercase,
// single-spaced. "## 📥 Inbox" and "inbox" match.
func Norm(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if space && b.Len() > 0 {
				b.WriteRune(' ')
			}
			space = false
			b.WriteRune(unicode.ToLower(r))
		default:
			space = true
		}
	}
	return b.String()
}

// Section returns the lines of the H2/H3 whose heading text matches want
// (ignoring emoji and case), heading line included. ok is false when no
// heading matches.
func Section(lines []string, want string) ([]string, bool) {
	w := Norm(want)
	hs := Headings(lines)
	for i, h := range hs {
		if Norm(h.Text) != w {
			continue
		}
		// a section owns the sub-headings nested under it: run to the next
		// heading of the same or a higher level.
		end := len(lines)
		for _, n := range hs[i+1:] {
			if n.Level <= h.Level {
				end = n.Start
				break
			}
		}
		return lines[h.Start:end], true
	}
	return nil, false
}

// Bullets returns the top-level "- " lines directly under the heading matching
// want, stopping at the next heading of any level (so routine sub-sections
// appended under 📓 Log are not swept in).
func Bullets(lines []string, want string) []string {
	w := Norm(want)
	for _, h := range Headings(lines) {
		if Norm(h.Text) != w {
			continue
		}
		var out []string
		for _, l := range lines[h.Start+1 : h.End] {
			if strings.HasPrefix(l, "- ") {
				out = append(out, l)
			}
		}
		return out
	}
	return nil
}

// Truncate cuts s to at most n runes, marking the cut with "…".
func Truncate(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\t", " "))
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return strings.TrimSpace(string(r[:n-1])) + "…"
}
