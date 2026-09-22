// Command qilla is an opinionated runtime that turns Claude Code plus an
// Obsidian vault into an always-available chief of staff on a Linux box.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/install"
)

const version = "0.0.0-dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "init", "version", "help", "-h", "--help", "guard", "hook", "statusline":
	default:
		firstRun()
	}
	switch os.Args[1] {
	case "version":
		fmt.Println("qilla", version)
	case "ask":
		if err := cmdAsk(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "qilla:", err)
			os.Exit(1)
		}
	case "model":
		if err := cmdModel(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "qilla:", err)
			os.Exit(1)
		}
	case "chat", "attach":
		if err := cmdChat(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "qilla:", err)
			os.Exit(1)
		}
	case "secret":
		if err := cmdSecret(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "qilla:", err)
			os.Exit(1)
		}
	case "guard":
		cmdGuard(os.Args[2:])
	case "hook":
		cmdHook(os.Args[2:])
	case "statusline":
		cmdStatusline(os.Args[2:])
	case "plugin":
		if err := cmdPlugin(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "qilla:", err)
			os.Exit(1)
		}
	case "artifact":
		if err := cmdArtifact(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "qilla:", err)
			os.Exit(1)
		}
	case "browser":
		if err := cmdBrowser(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "qilla:", err)
			os.Exit(1)
		}
	case "task":
		if err := cmdTask(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "qilla:", err)
			os.Exit(1)
		}
	case "lock":
		if err := cmdLock(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "qilla:", err)
			os.Exit(1)
		}
	case "unlock":
		if err := cmdUnlock(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "qilla:", err)
			os.Exit(1)
		}
	case "queue":
		if err := cmdQueue(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "qilla:", err)
			os.Exit(1)
		}
	case "remind":
		if err := cmdRemind(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "qilla:", err)
			os.Exit(1)
		}
	case "catchup":
		if err := cmdCatchup(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "qilla:", err)
			os.Exit(1)
		}
	case "note":
		if err := cmdNote(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "qilla:", err)
			os.Exit(1)
		}
	case "log":
		if err := cmdLog(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "qilla:", err)
			os.Exit(1)
		}
	case "config":
		if err := cmdConfig(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "qilla:", err)
			os.Exit(1)
		}
	case "runs":
		if err := cmdRuns(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "qilla:", err)
			os.Exit(1)
		}
	case "sessions":
		if err := cmdSessions(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "qilla:", err)
			os.Exit(1)
		}
	case "mem":
		if err := cmdMem(os.Args[2:]); err != nil {
			if !errors.Is(err, errNotSeen) {
				fmt.Fprintln(os.Stderr, "qilla:", err)
			}
			os.Exit(1)
		}
	case "new":
		if err := cmdNew(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "qilla:", err)
			os.Exit(1)
		}
	case "passwd":
		if err := cmdPasswd(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "qilla:", err)
			os.Exit(1)
		}
	case "init":
		if err := cmdInit(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "qilla:", err)
			os.Exit(1)
		}
	case "doctor":
		if err := cmdDoctor(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "qilla:", err)
			os.Exit(1)
		}
	case "reconcile":
		if err := cmdReconcile(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "qilla:", err)
			os.Exit(1)
		}
	case "serve":
		if err := cmdServe(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "qilla:", err)
			os.Exit(1)
		}
	case "status":
		if err := cmdStatus(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "qilla:", err)
			os.Exit(1)
		}
	case "cost":
		if err := cmdCost(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "qilla:", err)
			os.Exit(1)
		}
	case "prices":
		if err := cmdPrices(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "qilla:", err)
			os.Exit(1)
		}
	case "gather":
		if err := cmdGather(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "qilla:", err)
			os.Exit(1)
		}
	case "routine":
		if err := cmdRoutine(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "qilla:", err)
			os.Exit(1)
		}
	case "run":
		if err := cmdRun(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "qilla:", err)
			os.Exit(1)
		}
	case "gmail":
		if err := cmdGmail(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "qilla:", err)
			os.Exit(exitCode(err))
		}
	case "decide":
		if err := cmdDecide(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "qilla:", err)
			if errors.Is(err, errUsage) {
				os.Exit(2) // bad arguments; a backend failure exits 0
			}
			os.Exit(1)
		}
	case "fetch":
		if err := cmdFetch(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "qilla:", err)
			var blocked errBlocked
			switch {
			case errors.As(err, &blocked):
				os.Exit(3) // nothing usable on any rung
			case errors.Is(err, errUsage):
				os.Exit(2)
			}
			os.Exit(1)
		}
	case "enqueue":
		if err := cmdEnqueue(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "qilla:", err)
			os.Exit(1)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: qilla <command>

  init | doctor [--json] [--health [--count]] | doctor fix | passwd | new routine | routine check <name>|--all [--json] | secret | model | chat [agent] | ask "<text>" | enqueue | run | gather <routine> [--dry] | runs last <routine> | runs log [--routine r] [--days n] | serve | reconcile | status | task | lock <vault path> | unlock <vault path> | queue add|list|count|drop | remind add "<what>" --at "<YYYY-MM-DD [HH:MM]>" | remind list|due [--notify]|count|reset | catchup [--all] | note <path> [--section "<heading>"] [--tail N] | log "<text>" [--date YYYY-MM-DD] | config get <dotted.key> | config keys | sessions [--rotated] | sessions handoff [--agent a] [--set "<text>"] | mem | gmail auth|stage|staged|apply|untrash | browser | fetch <url> [--max-rung N] [--json] | artifact | plugin [install|usage [--unused] [--json]] | decide predict --task T | decide status | statusline | guard | hook | cost | prices sync | version`)
}

// firstRun makes sure the pieces exist before any command: a missing config is
// written from the template (with a notice), missing vault/runtime defaults are
// regenerated, edited files are never touched.
func firstRun() {
	path := config.DefaultPath()
	if _, err := os.Stat(path); err != nil {
		fmt.Fprintf(os.Stderr, "qilla: no config at %s — writing the default (edit it, then `qilla init` for the systemd units)\n", path)
		if _, err := install.WriteFile(path, install.Template, false); err != nil {
			fmt.Fprintln(os.Stderr, "qilla:", err)
			os.Exit(1)
		}
	}
	cfg, err := config.Load(path)
	if err != nil {
		return // the command itself reports the config error
	}
	for _, w := range install.Ensure(cfg) {
		fmt.Fprintln(os.Stderr, "qilla: wrote default", w)
	}
}
