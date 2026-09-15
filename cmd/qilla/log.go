package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/vault"
)

const logUsage = `usage: qilla log "<text>" [--date YYYY-MM-DD]

Append "- HH:MM · <text>" under the ## 📓 Log heading of the daily note, without
reading or rewriting the rest of it. The note is created from the daily template
when missing, the section is appended when missing. Silent on success.`

// cmdLog: qilla log "<text>" [--date d].
func cmdLog(args []string) error {
	fs := flag.NewFlagSet("log", flag.ContinueOnError)
	date := fs.String("date", "", "daily note to write to (default: today)")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, logUsage) }
	var text string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		text, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if text == "" {
		text = strings.Join(fs.Args(), " ")
	}
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("%s", logUsage)
	}
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return err
	}
	loc := time.Local
	if cfg.Timezone != "" {
		if l, err := time.LoadLocation(cfg.Timezone); err == nil {
			loc = l
		}
	}
	now := time.Now().In(loc)
	day := now.Format("2006-01-02")
	if *date != "" {
		d, err := time.ParseInLocation("2006-01-02", *date, loc)
		if err != nil {
			return fmt.Errorf("log: --date must be YYYY-MM-DD")
		}
		day = d.Format("2006-01-02")
	}
	return appendLog(cfg, day, now.Format("15:04"), strings.TrimSpace(text))
}

// appendLog does the file work: ensure the note, insert the line.
func appendLog(cfg *config.Config, day, hhmm, text string) error {
	path := cfg.VaultPath(filepath.Join(cfg.JournalDir, day+".md"))
	b, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("log: %w", err)
		}
		seed, serr := os.ReadFile(cfg.VaultPath(cfg.DailyTemplate))
		if serr != nil {
			seed = []byte("# " + day + "\n\n" + vault.LogHeading + "\n")
		}
		b = []byte(strings.ReplaceAll(string(seed), "{{date}}", day))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
	}
	line := fmt.Sprintf("- %s · %s", hhmm, text)
	out := vault.AppendToSection(string(b), vault.Norm(vault.LogHeading), vault.LogHeading, line)
	return os.WriteFile(path, []byte(out), 0o644)
}
