package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
		return fmt.Errorf("usage: qilla decide predict --task T [--task T2] [--state DIR] | qilla decide status [--state DIR] | qilla decide ask …")
	}
	switch args[0] {
	case "predict":
		return decidePredict(args[1:])
	case "status":
		return decideStatus(args[1:])
	case "ask":
		return decideAsk(args[1:])
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

// --- decide ask ------------------------------------------------------------

const askUsage = `usage: qilla decide ask --choice a,b,c | --score | --noul [--question Q]
                       (--text T | --stdin) [--floor F] [--route local|jev] [--public]
                       [--caller C] [--state DIR]
       qilla decide ask --show-trace [--tail N] [--state DIR]`

// errUsage marks a bad invocation: main exits 2, not 1. A backend failure is
// never one of these — it is an `unknown` answer on stdout with exit 0.
var errUsage = errors.New("usage")

type askArgs struct {
	kind      string
	options   []string
	question  string
	text      string
	hasText   bool
	stdin     bool
	floor     float64
	route     string
	public    bool
	caller    string
	state     string
	showTrace bool
	tail      int
}

// parseAskArgs reads the flags; every error it returns wraps errUsage.
func parseAskArgs(args []string) (askArgs, error) {
	a := askArgs{kind: "choice", route: "local", tail: 20}
	bad := func(format string, v ...any) (askArgs, error) {
		return askArgs{}, fmt.Errorf("%w: %s", errUsage, fmt.Sprintf(format, v...))
	}
	var scoreKind, noulKind bool
	var choice string
	next := func(i *int, flag string) (string, bool) {
		if *i+1 >= len(args) {
			return "", false
		}
		*i++
		return args[*i], true
	}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--choice":
			v, ok := next(&i, "--choice")
			if !ok {
				return bad("--choice needs a value")
			}
			choice = v
		case "--score":
			scoreKind = true
		case "--noul":
			noulKind = true
		case "--question":
			v, ok := next(&i, "--question")
			if !ok {
				return bad("--question needs a value")
			}
			a.question = v
		case "--text":
			v, ok := next(&i, "--text")
			if !ok {
				return bad("--text needs a value")
			}
			a.text, a.hasText = v, true
		case "--stdin":
			a.stdin = true
		case "--floor":
			v, ok := next(&i, "--floor")
			if !ok {
				return bad("--floor needs a value")
			}
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return bad("--floor must be a number, got %q", v)
			}
			a.floor = f
		case "--route":
			v, ok := next(&i, "--route")
			if !ok {
				return bad("--route needs a value")
			}
			a.route = v
		case "--public":
			a.public = true
		case "--caller":
			v, ok := next(&i, "--caller")
			if !ok {
				return bad("--caller needs a value")
			}
			a.caller = v
		case "--state", "-state":
			v, ok := next(&i, "--state")
			if !ok {
				return bad("--state needs a value")
			}
			a.state = v
		case "--show-trace":
			a.showTrace = true
		case "--tail":
			v, ok := next(&i, "--tail")
			if !ok {
				return bad("--tail needs a value")
			}
			n, err := strconv.Atoi(v)
			if err != nil {
				return bad("--tail must be an integer, got %q", v)
			}
			a.tail = n
		default:
			return bad("unexpected argument %q\n%s", args[i], askUsage)
		}
	}
	if a.showTrace {
		return a, nil
	}
	switch {
	case scoreKind && noulKind:
		return bad("pass one of --score or --noul")
	case scoreKind:
		a.kind = "score"
	case noulKind:
		a.kind = "noul"
	}
	if a.kind == "choice" {
		for _, c := range strings.Split(choice, ",") {
			if c = strings.TrimSpace(c); c != "" {
				a.options = append(a.options, c)
			}
		}
		if len(a.options) == 0 {
			return bad("--choice is required (or use --score / --noul)")
		}
	} else if choice != "" {
		return bad("--choice goes with a choice decision, not --score / --noul")
	}
	if a.stdin == a.hasText {
		return bad("pass exactly one of --text or --stdin")
	}
	if a.route == "jev" && !a.public {
		return bad("jev route requires --public (non-sensitive input)")
	}
	return a, nil
}

// decideAsk is `qilla decide ask`: JSON on stdout, exit 0 unless the arguments
// are wrong. A backend that is down or angry is an `unknown` answer, not an
// error — the caller decides what to do with an undecided.
func decideAsk(args []string) error {
	a, err := parseAskArgs(args)
	if err != nil {
		return err
	}
	dir, err := decidersDir(a.state)
	if err != nil {
		return err
	}
	if a.showTrace {
		lines, err := decide.TraceTail(dir, a.tail)
		if err != nil {
			return err
		}
		for _, ln := range lines {
			fmt.Println(ln)
		}
		return nil
	}
	req := decide.AskRequest{
		Kind:     a.kind,
		Options:  a.options,
		Question: a.question,
		Floor:    a.floor,
		Route:    a.route,
		Public:   a.public,
		Caller:   a.caller,
	}
	if err := applyDecidersConfig(&req); err != nil {
		return err
	}
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	enc := json.NewEncoder(out)

	if !a.stdin {
		req.Text = a.text
		res, err := decide.AskAndTrace(context.Background(), dir, req)
		if err != nil && res.Kind == "" {
			return fmt.Errorf("%w: %v", errUsage, err)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "qilla decide ask: trace:", err)
		}
		return enc.Encode(res)
	}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var row struct {
			ID   any    `json:"id"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			fmt.Fprintf(os.Stderr, "skipping unparseable line: %.80s\n", line)
			continue
		}
		req.Text = row.Text
		res, err := decide.AskAndTrace(context.Background(), dir, req)
		if err != nil && res.Kind == "" {
			return fmt.Errorf("%w: %v", errUsage, err)
		}
		raw, merr := json.Marshal(res)
		if merr != nil {
			return merr
		}
		idRaw, merr := json.Marshal(row.ID)
		if merr != nil {
			return merr
		}
		// {"id": …, <the result's own fields in order>}
		fmt.Fprintf(out, "{\"id\":%s,%s\n", idRaw, string(raw[1:]))
	}
	return sc.Err()
}

// applyDecidersConfig fills the backend wiring from [deciders]; a missing
// config file leaves the package defaults in place.
func applyDecidersConfig(req *decide.AskRequest) error {
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return nil //nolint:nilerr // no usable config = package defaults
	}
	req.URL = cfg.Deciders.LemonadeURL
	req.Model = cfg.Deciders.AskModel
	req.PolicyVersion = cfg.Deciders.PolicyVersion
	if req.Floor == 0 {
		req.Floor = cfg.Deciders.ConfFloor
	}
	req.JevEnabled = cfg.Deciders.JevEnabled
	req.JevURL = cfg.Deciders.JevURL
	req.JevModel = cfg.Deciders.JevModel
	req.JevDailyMax = cfg.Deciders.JevDailyMax
	if req.Route == "jev" && cfg.Deciders.JevEnabled {
		if b, err := readSecret("jev_key"); err == nil {
			req.JevKey = strings.TrimSpace(string(b))
		}
	}
	return nil
}
