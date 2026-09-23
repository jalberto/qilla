package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/engine"
)

// cmdEngine: qilla engine status [--json] — what the engine is running, when
// each routine last ran, and whether its vault matches this checkout. On the
// engine (or with [engine].host unset or naming this host) it reads local
// state; on an agent it asks over ssh. `--json` is also what the agent's ssh
// call runs on the engine.
func cmdEngine(args []string) error {
	if len(args) == 0 || args[0] != "status" {
		return fmt.Errorf("usage: qilla engine status [--json]")
	}
	fs := flag.NewFlagSet("engine status", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "dump the status as JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return err
	}
	ctx := context.Background()
	st, err := engine.Get(ctx, cfg)
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(st)
	}
	var here *engine.Vault
	if !engine.IsLocal(cfg, engine.ShortHostname()) {
		v := engine.VaultState(ctx, cfg.Vault)
		here = &v
	}
	printEngineStatus(os.Stdout, st, here)
	return nil
}

// printEngineStatus renders the human view.
func printEngineStatus(w io.Writer, st *engine.Status, here *engine.Vault) {
	fmt.Fprintf(w, "%s · %s\n", st.Host, st.Role)
	if len(st.Running) == 0 {
		fmt.Fprintln(w, "running: none")
	} else {
		for _, r := range st.Running {
			fmt.Fprintf(w, "running: %s since %s\n", r.Routine, r.Since.Local().Format("2006-01-02 15:04"))
		}
	}
	names := make([]string, 0, len(st.Last))
	for n := range st.Last {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) > 0 {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "routine\tlast run\tok\tduration")
		for _, n := range names {
			l := st.Last[n]
			ok := "ok"
			if !l.OK {
				ok = "FAIL"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%.0fs\n", n, l.At.Local().Format("2006-01-02 15:04"), ok, l.DurationS)
		}
		tw.Flush()
	}
	if here == nil { // status read on this host: nothing to compare against
		fmt.Fprintf(w, "vault %s dirty %d · last commit %s\n", st.Vault.Head, st.Vault.Dirty, st.Vault.LastCommitAt)
		return
	}
	line, _ := engine.Parity(st.Host, st.Vault, *here)
	fmt.Fprintln(w, line)
}
