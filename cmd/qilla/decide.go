package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/decide"
)

// cmdDecide: qilla decide predict|status — the deciders' TF-IDF + LR models
// run in-process, no Python, no daemon. Training stays in the qilla-deciders
// repo; this only reads what `decide train` exported.
func cmdDecide(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: qilla decide predict --task T [--task T2] [--state DIR] | qilla decide status [--state DIR]")
	}
	switch args[0] {
	case "predict":
		return decidePredict(args[1:])
	case "status":
		return decideStatus(args[1:])
	default:
		return fmt.Errorf("qilla decide: unknown subcommand %q", args[0])
	}
}

// decidersDir is <state_dir>/deciders unless --state overrides it.
func decidersDir(override string) (string, error) {
	if override != "" {
		return config.Expand(override), nil
	}
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return "", err
	}
	return filepath.Join(cfg.StateDir, "deciders"), nil
}

func parseDecideFlags(args []string) (tasks []string, state string, err error) {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--task", "-task":
			if i+1 >= len(args) {
				return nil, "", fmt.Errorf("--task needs a value")
			}
			i++
			tasks = append(tasks, args[i])
		case "--state", "-state":
			if i+1 >= len(args) {
				return nil, "", fmt.Errorf("--state needs a value")
			}
			i++
			state = args[i]
		default:
			return nil, "", fmt.Errorf("unexpected argument %q", args[i])
		}
	}
	return tasks, state, nil
}

// decidePredict reads JSONL records on stdin and writes one JSONL result per
// record per task. A task with no exported model is skipped with a note on
// stderr and the exit stays 0 — the caller falls back to the model.
func decidePredict(args []string) error {
	tasks, state, err := parseDecideFlags(args)
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		return fmt.Errorf("qilla decide predict: at least one --task")
	}
	dir, err := decidersDir(state)
	if err != nil {
		return err
	}
	set, err := decide.LoadSet(dir, tasks, os.Stderr)
	if err != nil {
		return err
	}
	var records []decide.Record
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r decide.Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return fmt.Errorf("stdin: %w", err)
		}
		records = append(records, r)
	}
	if err := sc.Err(); err != nil {
		return err
	}
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	enc := json.NewEncoder(out)
	for _, res := range set.Classify(records) {
		if err := enc.Encode(res); err != nil {
			return err
		}
	}
	return nil
}

// decideStatus lists the exported models qilla can run.
func decideStatus(args []string) error {
	tasks, state, err := parseDecideFlags(args)
	if err != nil {
		return err
	}
	dir, err := decidersDir(state)
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		matches, _ := filepath.Glob(filepath.Join(dir, "models", "*.model.json.gz"))
		sort.Strings(matches)
		for _, m := range matches {
			tasks = append(tasks, strings.TrimSuffix(filepath.Base(m), ".model.json.gz"))
		}
	}
	if len(tasks) == 0 {
		fmt.Printf("no exported models in %s/models (run `decide train` then scripts/export_model.py)\n", dir)
		return nil
	}
	for _, task := range tasks {
		path := decide.ModelPath(dir, task)
		fi, statErr := os.Stat(path)
		m, err := decide.LoadModel(path)
		if err != nil {
			fmt.Printf("%-12s missing (%v)\n", task, err)
			continue
		}
		age := ""
		if statErr == nil {
			age = fmt.Sprintf("%.1f d old, %.1f MB", time.Since(fi.ModTime()).Hours()/24, float64(fi.Size())/1e6)
		}
		fmt.Printf("%-12s classes=%s threshold=%.4f features=%d  %s\n",
			task, strings.Join(m.Classes, "/"), m.Threshold,
			len(m.Word.IDF)+len(m.Char.IDF), age)
	}
	return nil
}
