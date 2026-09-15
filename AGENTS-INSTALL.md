# Installing qilla — instructions for a coding agent

You are an AI coding agent (Claude Code or similar) installing qilla on someone's Linux machine. Work through these steps in order. Stop and ask the user whenever a step says so; never guess a vault path, a budget or a credential.

qilla is **Linux + systemd only**. If the machine is macOS or has no systemd user session, stop and tell the user — there is no supported path.

## 0. Preconditions

Run each check and compare with the expected output. Do not install anything the user has not agreed to; report what is missing and ask.

```sh
uname -s                      # expect: Linux
systemctl --user is-system-running   # expect: running or degraded (not "Failed to connect to bus")
systemctl --version | head -1        # expect: systemd >= 250 (needed for systemd-creds)
claude --version              # expect: a version string; if missing, Claude Code is not installed
go version                    # expect: go1.27 or newer — only needed to build/install from source
mise --version                # expect: a version; used to pin the runtime tools
command -v bwrap socat        # expect: both paths; Claude's Bash sandbox needs them
command -v systemd-creds      # expect: a path; needed for `qilla secret`
command -v notify-send        # optional: desktop reminder notifications
command -v git                # optional: only for routines with commit = true
```

Missing pieces, in the user's package manager (ask before running): `bubblewrap`, `socat`, `libnotify` (Fedora: `sudo dnf install bubblewrap socat libnotify`). Claude Code and `mise` are installed per their own docs; `knap` and `qmd` come later through mise + npm, not system-wide.

## 1. Install the binary

The repo supports both. Prefer `go install` when Go is present:

```sh
go install github.com/jalberto/qilla/cmd/qilla@latest
command -v qilla && qilla version        # expect: .../go/bin/qilla and "qilla <version>"
```

From source instead (the repo's `mise.toml` pins go 1.27.1 and node 24):

```sh
git clone https://github.com/jalberto/qilla && cd qilla
mise install && go build ./cmd/qilla     # produces ./qilla; put it somewhere on PATH, e.g. ~/.local/bin
go test ./...                            # expect: ok for every package
```

If `qilla` is not on PATH afterwards, fix PATH before continuing — the generated systemd units call it by absolute path, but the user will not.

## 2. Create the configuration

```sh
qilla init                # writes ~/.config/qilla/qilla.toml, mise.toml, statusline.sh, and the systemd units
```
It never overwrites an existing `qilla.toml` without `--force`. `qilla init --print` dumps the template if you want to read it first. The shipped [`qilla.example.toml`](qilla.example.toml) documents every key — use it as the reference while editing, and copy comments the user will need later.

**Ask the user for these before editing the file.** Do not invent values:

| Key | Ask for |
|---|---|
| `vault` | the absolute path of their markdown/Obsidian vault |
| `timezone` | their IANA timezone (e.g. `Europe/Madrid`) — schedules and windows are local |
| `[budget] warn` / `block` | the daily USD they are willing to spend, and the hard stop |
| `[models]` tiers + `ladder`, `degrade_at` | which model per tier (classify/extract/research/synthesis/judgment/coding) and when to degrade |
| `[agents.<name>]` | which agents exist, their tier/model, budget, `allowed_tools`, `scope` |
| `persona`, `rules`, `facts_dir`, `journal_dir`, `todo`, `questions` | where those live inside their vault (defaults assume `Qilla/…` and `Desk/…`) |
| `daily_template`, `reply_marker` | the daily-note template path and the reply-line marker `qilla catchup` looks for — defaults are in `qilla.example.toml`; confirm they match the user's vault conventions |
| `[web] listen` / password | whether they want the web page at all; set the password with `qilla passwd`, never by hand |
| `[health] watched_services`, `freshness` | which units and artifacts must never go stale |
| Claude auth | confirm they have a Claude subscription and have run `claude` once and completed `/login`. You cannot do this for them |

Then install the runtime tools the unit will see:

```sh
mise install -C ~/.config/qilla
mise exec -C ~/.config/qilla -- npm i -g knap @tobilu/qmd
```

## 3. Generate and enable the units

Re-run `qilla init` after every config change that adds or removes a routine — units are regenerated from the config each time.

```sh
qilla init
systemctl --user daemon-reload
systemctl --user enable --now qilla.socket qilla-reconcile.timer   # init prints the exact full line, including per-routine timers
loginctl enable-linger $USER          # timers keep firing when the user is not logged in
qilla prices sync                     # so runs are priced instead of "unpriced"
```

Enable exactly the timers `qilla init` printed. Do not invent unit names.

## 4. Verify

```sh
qilla doctor                          # expect: every row ok; exit 1 means a hard failure — fix it, do not move on
qilla doctor --health                 # expect: silent, exit 0
systemctl --user list-timers 'qilla-*'   # expect: one line per enabled timer with a NEXT time in the future
qilla status                          # expect: the day's grid; exit 1 just means something needs the user
qilla runs last <routine>             # expect: "no run" until the first run happens
```

If `doctor` complains about mise shims, it prints the exact lines to pin in `~/.config/qilla/mise.toml`. Apply them and re-check rather than working around it.

## 5. Scaffold the first routine

Pick something the user actually wants daily and that needs no model first.

```sh
qilla new routine <name> --kind script --schedule "*-*-* 07:00" --window 07:00-10:00 \
  --output "<vault-relative note>" --append
qilla gather <name>                   # expect: one JSON object — iterate here, it costs nothing
qilla init && systemctl --user enable --now qilla-<name>.timer
qilla doctor                          # template validates, schedule parses, timer active
qilla run <name>                      # one real run; read the note it produced
```

AI kinds (`--kind ai-fresh|ai-resumed --agent <agent>`) additionally need `prompt.md`, a tier and a budget of at least ~6× the expected per-run cost. The `qilla:routine` skill in `skills/routine/SKILL.md` is the full design procedure — load it before designing a routine.

## Do not

- **Never put a secret in `qilla.toml`** (or in a routine's prompt, or in a gather script). Use `qilla secret set <name> < token.txt`; the gather reads the plaintext from `$QILLA_SECRETS_DIR/<name>` and the model never sees it.
- **Never run qilla as root** or install the units system-wide. Everything is `systemctl --user`.
- **Never set `[sandbox] disabled = true`** to make something work, and never remove hardening lines from the generated unit.
- Never widen a routine's `allowed_tools` or `allowed_domains` beyond what its prompt needs.
- Never set the web password by writing a hash by hand — use `qilla passwd` — and never bind the web listener to anything but loopback (publish over Tailscale if remote access is wanted).
- Never overwrite an edited `qilla.toml` with `qilla init --force` without asking.
- Never claim a routine works without `qilla gather`, `qilla doctor` and, for AI kinds, one real `qilla run`.

## Final checklist

- [ ] Linux with a systemd user session, systemd ≥ 250
- [ ] `bwrap`, `socat` present (or the user knowingly accepted no Bash sandbox)
- [ ] `qilla version` runs from PATH
- [ ] Claude Code installed and logged in (`claude`, `/login`)
- [ ] `~/.config/qilla/qilla.toml` filled from the user's answers: vault, timezone, budgets, models/tiers, agents, `daily_template`, `reply_marker`
- [ ] `mise install -C ~/.config/qilla` done; `knap` and `qmd` available to the unit
- [ ] `qilla init` run after the final config edit
- [ ] `systemctl --user daemon-reload` and the printed `enable --now` line executed
- [ ] `loginctl enable-linger $USER`
- [ ] `qilla prices sync` done
- [ ] `qilla doctor` exits 0; `qilla doctor --health` silent
- [ ] `systemctl --user list-timers 'qilla-*'` shows a future NEXT for every enabled timer
- [ ] One routine scaffolded, gathered, run, and its output note read
- [ ] No secret anywhere in the TOML; any credential stored via `qilla secret`
- [ ] Told the user what you changed, what you could not verify, and what they must do themselves
