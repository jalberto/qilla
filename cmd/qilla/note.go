package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/vault"
)

const noteUsage = `usage: qilla note <vault-relative path> [--section "<heading>"] [--tail N] [--frontmatter]

Print a note, or just the part of it you need — for a note you can name.
For topical retrieval ("where did we discuss X") use qmd query "<what>" --files
(hybrid search over the vault); --section is for a known note + section only.

  --section "<heading>"  the H2/H3 whose text matches, emoji and case ignored
  --tail N               only the last N lines of what would be printed
  --frontmatter          keep the YAML block (skipped by default)`

// cmdNote: qilla note <path> [--section h] [--tail n].
func cmdNote(args []string) error {
	fs := flag.NewFlagSet("note", flag.ContinueOnError)
	section := fs.String("section", "", "print only this H2/H3 section")
	tail := fs.Int("tail", 0, "print only the last N lines")
	front := fs.Bool("frontmatter", false, "keep the YAML frontmatter")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, noteUsage) }
	var rel string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		rel, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if rel == "" && fs.NArg() > 0 {
		rel = fs.Arg(0)
	}
	if rel == "" {
		return fmt.Errorf("%s", noteUsage)
	}
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return err
	}
	if filepath.IsAbs(rel) || strings.Contains(rel, "..") {
		return fmt.Errorf("note: %s must be vault-relative", rel)
	}
	b, err := os.ReadFile(cfg.VaultPath(rel))
	if err != nil {
		return fmt.Errorf("note: %w", err)
	}
	fm, body := vault.SplitFrontmatter(string(b))
	text := body
	if *front {
		text = fm + body
	}
	lines := vault.Lines(text)
	if *section != "" {
		sec, ok := vault.Section(vault.Lines(body), *section)
		if !ok {
			return fmt.Errorf("note: %s has no section %q", rel, *section)
		}
		lines = sec
	}
	if *tail > 0 && len(lines) > *tail {
		lines = lines[len(lines)-*tail:]
	}
	for _, l := range lines {
		fmt.Println(l)
	}
	return nil
}
