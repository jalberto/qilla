package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/jalberto/qilla/internal/config"
)

const configUsage = `usage: qilla config <command>   (read-only)

  get <dotted.key>   the TOML of that subtree (qilla config get chat.autocompact_pct)
  keys               every table and leaf key, dotted, one per line`

// cmdConfig: qilla config get|keys — read the config file without loading the
// whole thing into a session's context.
func cmdConfig(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%s", configUsage)
	}
	var raw map[string]any
	path := config.DefaultPath()
	if _, err := toml.DecodeFile(path, &raw); err != nil {
		return fmt.Errorf("config %s: %w", path, err)
	}
	switch args[0] {
	case "keys":
		if len(args) > 1 {
			return fmt.Errorf("usage: qilla config keys")
		}
		keys := dottedKeys(raw, "")
		sort.Strings(keys)
		fmt.Printf("n=%d\n", len(keys))
		for _, k := range keys {
			fmt.Println(k)
		}
		return nil
	case "get":
		if len(args) != 2 || args[1] == "" {
			return fmt.Errorf("usage: qilla config get <dotted.key>")
		}
		v, ok := lookup(raw, strings.Split(args[1], "."))
		if !ok {
			return fmt.Errorf("config: no key %q (try `qilla config keys`)", args[1])
		}
		enc := toml.NewEncoder(os.Stdout)
		if m, isTable := v.(map[string]any); isTable {
			return enc.Encode(m)
		}
		parts := strings.Split(args[1], ".")
		return enc.Encode(map[string]any{parts[len(parts)-1]: v})
	}
	return fmt.Errorf("%s", configUsage)
}

// lookup walks a dotted path through nested TOML tables.
func lookup(m map[string]any, path []string) (any, bool) {
	var cur any = m
	for _, p := range path {
		t, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = t[p]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// dottedKeys lists every key, tables included, as "a.b.c".
func dottedKeys(m map[string]any, prefix string) []string {
	var out []string
	for k, v := range m {
		full := k
		if prefix != "" {
			full = prefix + "." + k
		}
		out = append(out, full)
		if t, ok := v.(map[string]any); ok {
			out = append(out, dottedKeys(t, full)...)
		}
	}
	return out
}
