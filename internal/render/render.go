// Package render turns a run's data into a vault note with knap, Obsidian's
// template language: JSON in, Markdown out, identical every day.
package render

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/worker"
)

// Knap renders with the `knap` CLI. Vault is the root for template and output.
type Knap struct {
	Vault string
	Bin   string // default "knap"
	Now   func() time.Time
}

// Render writes routine output. Template: <vault>/Qilla/Routines/<name>/template.md
// (or r.Template when set). Output: r.Output with {{date}} substituted, vault-relative.
// Routines without Output render nothing.
func (k Knap) Render(ctx context.Context, name string, r config.Routine, data worker.Data) error {
	if r.Output == "" {
		return nil
	}
	if k.Bin == "" {
		k.Bin = "knap"
	}
	if k.Now == nil {
		k.Now = time.Now
	}
	tmpl := r.Template
	if tmpl == "" {
		tmpl = filepath.Join(worker.RoutinesDir, name, "template.md")
	}
	tmplPath := filepath.Join(k.Vault, tmpl)
	if _, err := os.Stat(tmplPath); err != nil {
		return fmt.Errorf("routine %s: template %s: %w", name, tmpl, err)
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, k.Bin, "render", tmplPath, "--data", "-")
	cmd.Stdin = bytes.NewReader(payload)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("knap render %s: %v: %s", tmpl, err, strings.TrimSpace(errb.String()))
	}
	outRel := strings.ReplaceAll(r.Output, "{{date}}", k.Now().Format("2006-01-02"))
	outPath := filepath.Join(k.Vault, outRel)
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	if r.Append {
		f, err := os.OpenFile(outPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		if fi, _ := f.Stat(); fi != nil && fi.Size() > 0 {
			f.WriteString("\n")
		}
		_, err = f.Write(out.Bytes())
		return err
	}
	return os.WriteFile(outPath, out.Bytes(), 0o644)
}

// Validate runs `knap validate` on a template (doctor).
func Validate(bin, tmplPath string) error {
	if bin == "" {
		bin = "knap"
	}
	out, err := exec.Command(bin, "validate", tmplPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", filepath.Base(tmplPath), strings.TrimSpace(string(out)))
	}
	return nil
}
