// Package tasks implements one-task-one-id over an Obsidian vault.
//
// The problem it fixes: an action point captured in a meeting note gets copied
// into Desk/Todo.md and onto a person page. Three copies, three lifetimes —
// tick one and the others quietly lie. Obsidian block references give us the
// mechanism for free, so no server is needed: every synced task line ends with
// a block id and the copies carry the SAME id.
//
//   - [ ] 2026-08-26 · Chase the benchmark table from Sam ^task-20260826-001
package tasks

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// IDPattern matches a task block id anywhere in a line.
var idPattern = regexp.MustCompile(`\^task-(\d{8})-(\d{3})`)

// openLine matches an unticked task line.
var openLine = regexp.MustCompile(`^\s*- \[ \] `)

// stamped matches a completion stamp already on the line.
var stamped = regexp.MustCompile(`✅\d{4}-\d{2}-\d{2}`)

// walk yields every markdown file under root, skipping dot-directories
// (.obsidian, .git, .trash) and node_modules.
func walk(root string, fn func(path string) error) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable corners are not a reason to fail the scan
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		return fn(path)
	})
}

func readLines(path string) ([]string, os.FileMode, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, 0, err
	}
	return strings.Split(string(b), "\n"), fi.Mode(), nil
}

// NextID returns the next free id for day (format 20060102): one past the
// highest sequence already written anywhere under root.
//
// Ids are free until written: calling NextID twice without writing the first
// one in between returns the same id both times. That is deliberate — nothing
// reserves an id but the file that carries it.
func NextID(root, day string) (string, error) {
	high := 0
	err := walk(root, func(path string) error {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for _, m := range idPattern.FindAllStringSubmatch(string(b), -1) {
			if m[1] != day {
				continue
			}
			if n, err := strconv.Atoi(m[2]); err == nil && n > high {
				high = n
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("task-%s-%03d", day, high+1), nil
}

// Hit is one line carrying an id.
type Hit struct {
	File string // vault-relative
	Line int    // 1-based
	Text string // trimmed
}

// hasID reports whether line carries ^id with a word boundary after it.
func hasID(line, id string) bool {
	needle := "^" + id
	for i := 0; ; {
		j := strings.Index(line[i:], needle)
		if j < 0 {
			return false
		}
		j += i
		end := j + len(needle)
		if end == len(line) || !isWordByte(line[end]) {
			return true
		}
		i = end
	}
}

func isWordByte(c byte) bool {
	return c == '_' || ('0' <= c && c <= '9') || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}

// Find returns every line carrying id, in path order.
func Find(root, id string) ([]Hit, error) {
	id = strings.TrimPrefix(id, "^")
	var hits []Hit
	err := walk(root, func(path string) error {
		lines, _, err := readLines(path)
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		for i, l := range lines {
			if hasID(l, id) {
				hits = append(hits, Hit{File: rel, Line: i + 1, Text: strings.TrimSpace(l)})
			}
		}
		return nil
	})
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].File != hits[j].File {
			return hits[i].File < hits[j].File
		}
		return hits[i].Line < hits[j].Line
	})
	return hits, err
}

// stampLine ticks the checkbox and stamps the completion date on a line
// carrying id, mirroring the original sed: the box is ticked only if still
// open, and the stamp is added only if none is present.
func stampLine(line, id, date string) string {
	line = openLine.ReplaceAllStringFunc(line, func(m string) string {
		return strings.Replace(m, "- [ ] ", "- [x] ", 1)
	})
	if stamped.MatchString(line) {
		return line
	}
	needle := "^" + id
	j := strings.Index(line, needle)
	if j < 0 {
		return line
	}
	// swallow the whitespace run before the id, as the sed's \s* did
	k := j
	for k > 0 && (line[k-1] == ' ' || line[k-1] == '\t') {
		k--
	}
	return line[:k] + " ✅" + date + " " + needle + line[j+len(needle):]
}

// Done ticks every copy of id and stamps the completion date. It returns the
// vault-relative files it changed. Writes are atomic (temp + rename in the same
// directory) and preserve the file mode.
func Done(root, id string, now time.Time) ([]string, error) {
	id = strings.TrimPrefix(id, "^")
	date := now.Format("2006-01-02")
	var changed []string
	err := walk(root, func(path string) error {
		lines, mode, err := readLines(path)
		if err != nil {
			return nil
		}
		touched := false
		for i, l := range lines {
			if !hasID(l, id) {
				continue
			}
			if out := stampLine(l, id, date); out != l {
				lines[i] = out
				touched = true
			}
		}
		if !touched {
			// Still report the file if it carries the id (already done).
			for _, l := range lines {
				if hasID(l, id) {
					rel, _ := filepath.Rel(root, path)
					changed = append(changed, rel)
					break
				}
			}
			return nil
		}
		if err := writeAtomic(path, strings.Join(lines, "\n"), mode); err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		changed = append(changed, rel)
		return nil
	})
	sort.Strings(changed)
	return changed, err
}

func writeAtomic(path, content string, mode os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".qilla*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode.Perm()); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Open is one id-bearing task still open.
type Open struct {
	ID   string
	Text string
	File string // first file carrying it, vault-relative
}

// OpenTasks returns every id-bearing open task, deduped by id, sorted by id.
func OpenTasks(root string) ([]Open, error) {
	seen := map[string]*Open{}
	var ids []string
	err := walk(root, func(path string) error {
		lines, _, err := readLines(path)
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		for _, l := range lines {
			if !openLine.MatchString(l) {
				continue
			}
			m := idPattern.FindString(l)
			if m == "" {
				continue
			}
			id := strings.TrimPrefix(m, "^")
			if _, ok := seen[id]; ok {
				continue
			}
			text := strings.TrimSpace(openLine.ReplaceAllString(l, ""))
			text = strings.TrimSpace(strings.Replace(text, m, "", 1))
			seen[id] = &Open{ID: id, Text: text, File: rel}
			ids = append(ids, id)
		}
		return nil
	})
	sort.Strings(ids)
	out := make([]Open, 0, len(ids))
	for _, id := range ids {
		out = append(out, *seen[id])
	}
	return out, err
}
