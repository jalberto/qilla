// Package loader builds an AI prompt from vault layers in a fixed order:
// stable layers first and byte-identical across runs (persona, rules), then
// facts, journal, recall, then the routine prompt and the gathered input.
package loader

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Loader knows where the layers live.
type Loader struct {
	Vault         string
	Persona       string        // vault-relative
	Rules         string        // vault-relative
	FactsDir      string        // vault-relative dir of <domain>.md
	JournalDir    string        // vault-relative dir of YYYY-MM-DD.md
	RecallCmd     string        // shell command; {{query}} and {{n}} substituted; prints vault-relative paths, one per line
	RecallTimeout time.Duration // default 45s
	// Memory injects working-memory lines for a query (nil = the memory:<N> token is ignored).
	Memory func(query string, n int) (text string, err error)
	Now    func() time.Time
}

// Prompt is the assembled text plus bookkeeping for the ledger.
type Prompt struct {
	Text        string
	Stable      string // the cache-stable prefix (persona + rules)
	Dynamic     string // everything after the prefix — sent as the user prompt
	Chars       int
	EstTokens   int      // chars/4, good enough for budgeting
	Missing     []string // scope tokens that resolved to nothing
	MemoryChars int      // chars injected from working memory
}

var scopeRe = regexp.MustCompile(`^(persona|rules|facts:[A-Za-z0-9_-]+|journal:[1-9][0-9]*d|journal-full:[1-9][0-9]*d|recall:[1-9][0-9]*|memory:[1-9][0-9]*|file:.+)$`)

// ValidateScope rejects unknown or malformed scope tokens.
func ValidateScope(scope []string) error {
	for _, s := range scope {
		if !scopeRe.MatchString(s) {
			return fmt.Errorf("bad scope token %q (persona | rules | facts:<domain> | journal:<N>d | journal-full:<N>d | recall:<N> | memory:<N> | file:<path>)", s)
		}
	}
	return nil
}

// Build assembles the prompt. Order is fixed regardless of scope order:
// persona, rules, facts, journal, recall, file, routine prompt, gathered.
func (l Loader) Build(scope []string, query, routinePrompt, gathered string) (Prompt, error) {
	if err := ValidateScope(scope); err != nil {
		return Prompt{}, err
	}
	if l.Now == nil {
		l.Now = time.Now
	}
	has := map[string]bool{}
	var facts, files []string
	journalDays, recallN, memoryN := 0, 0, 0
	journalFull := false
	for _, s := range scope {
		switch {
		case s == "persona", s == "rules":
			has[s] = true
		case strings.HasPrefix(s, "facts:"):
			facts = append(facts, strings.TrimPrefix(s, "facts:"))
		case strings.HasPrefix(s, "journal-full:"):
			journalDays, _ = strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(s, "journal-full:"), "d"))
			journalFull = true
		case strings.HasPrefix(s, "journal:"):
			journalDays, _ = strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(s, "journal:"), "d"))
		case strings.HasPrefix(s, "recall:"):
			recallN, _ = strconv.Atoi(strings.TrimPrefix(s, "recall:"))
		case strings.HasPrefix(s, "memory:"):
			memoryN, _ = strconv.Atoi(strings.TrimPrefix(s, "memory:"))
		case strings.HasPrefix(s, "file:"):
			files = append(files, strings.TrimPrefix(s, "file:"))
		}
	}

	var p Prompt
	var stable, dyn bytes.Buffer
	if has["persona"] {
		l.section(&stable, &p, "persona", l.Persona)
	}
	if has["rules"] {
		l.section(&stable, &p, "rules", l.Rules)
	}
	for _, d := range facts {
		l.section(&dyn, &p, "facts:"+d, filepath.Join(l.FactsDir, d+".md"))
	}
	if journalDays > 0 {
		found := false
		day := l.Now()
		raw, kept := 0, 0
		// oldest first so the newest ends closest to the prompt
		for i := journalDays - 1; i >= 0; i-- {
			rel := filepath.Join(l.JournalDir, day.AddDate(0, 0, -i).Format("2006-01-02")+".md")
			if journalFull {
				if l.section(&dyn, &p, "journal", rel) {
					found = true
				}
				continue
			}
			b, err := os.ReadFile(filepath.Join(l.Vault, rel))
			if err != nil {
				p.Missing = append(p.Missing, "journal")
				continue
			}
			found = true
			raw += len(b)
			compact := CompactJournal(string(b))
			kept += len(compact)
			fmt.Fprintf(&dyn, "<!-- journal: %s (compact) -->\n%s\n\n", rel, compact)
		}
		if !journalFull {
			slog.Debug("journal scope compacted", "days", journalDays, "chars_before", raw, "chars_after", kept)
		}
		if !found {
			p.Missing = append(p.Missing, fmt.Sprintf("journal:%dd", journalDays))
		}
	}
	if recallN > 0 {
		paths, err := l.recall(query, recallN)
		if err != nil {
			// recall is an accelerator: a slow or missing tool costs context, never the run
			p.Missing = append(p.Missing, fmt.Sprintf("recall:%d (%s)", recallN, firstLine(err.Error())))
			paths = nil
		} else if len(paths) == 0 {
			p.Missing = append(p.Missing, fmt.Sprintf("recall:%d", recallN))
		}
		for _, rel := range paths {
			l.section(&dyn, &p, "recall", rel)
		}
	}
	for _, f := range files {
		l.section(&dyn, &p, "file:"+f, f)
	}
	if memoryN > 0 && l.Memory != nil {
		txt, err := l.Memory(query, memoryN)
		switch {
		case err != nil:
			// working memory is an accelerator, never a blocker: the run continues without it
			p.Missing = append(p.Missing, fmt.Sprintf("memory:%d (%s)", memoryN, firstLine(err.Error())))
		case strings.TrimSpace(txt) == "":
			p.Missing = append(p.Missing, fmt.Sprintf("memory:%d", memoryN))
		default:
			fmt.Fprintf(&dyn, "<!-- working memory -->\n%s\n", txt)
			p.MemoryChars = len(txt)
		}
	}
	if routinePrompt != "" {
		fmt.Fprintf(&dyn, "<!-- task -->\n%s\n\n", routinePrompt)
	}
	if gathered != "" {
		fmt.Fprintf(&dyn, "<!-- input -->\n%s\n", gathered)
	}
	p.Stable = stable.String()
	p.Dynamic = dyn.String()
	p.Text = p.Stable + p.Dynamic
	p.Chars = len(p.Text)
	p.EstTokens = (p.Chars + 3) / 4
	return p, nil
}

// section appends one vault file as a labelled block. Missing files are
// recorded in p.Missing, never fatal: a routine must not die because a
// facts domain has no note yet.
func (l Loader) section(w *bytes.Buffer, p *Prompt, label, rel string) bool {
	b, err := os.ReadFile(filepath.Join(l.Vault, rel))
	if err != nil {
		p.Missing = append(p.Missing, label)
		return false
	}
	fmt.Fprintf(w, "<!-- %s: %s -->\n%s\n\n", label, rel, strings.TrimRight(string(b), "\n"))
	return true
}

// recall runs RecallCmd and returns vault-relative paths, at most n.
func (l Loader) recall(query string, n int) ([]string, error) {
	if l.RecallCmd == "" {
		return nil, fmt.Errorf("recall: recall_cmd not configured")
	}
	cmd := strings.NewReplacer("{{query}}", shellQuote(query), "{{n}}", strconv.Itoa(n)).Replace(l.RecallCmd)
	to := l.RecallTimeout
	if to == 0 {
		to = 45 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), to)
	defer cancel()
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	c.Dir = l.Vault
	out, err := c.Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		return nil, fmt.Errorf("recall command failed (%s): %v %s — check recall_cmd in qilla.toml and that the tool is on PATH", cmd, err, stderr)
	}
	var paths []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// qmd --files prints "#id,score,qmd://collection/path"; keep only the path
		if i := strings.Index(line, "qmd://"); i >= 0 {
			line = line[i+len("qmd://"):]
			if j := strings.Index(line, "/"); j >= 0 {
				line = line[j+1:]
			}
		}
		paths = append(paths, line)
		if len(paths) == n {
			break
		}
	}
	return paths, nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}
