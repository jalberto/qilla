---
name: routine
description: Design and scaffold a qilla routine following the one pattern (TOML block, gather.star, template.md, prompt.md, systemd timer). Use when asked to "add a routine", "automate X every day/week", "make qilla do X on a schedule", or when a task repeats and should run without asking. Defaults to a no-AI script routine; only adds a model call where judgment is needed.
---

# qilla:routine — one pattern for every routine

A routine is a scheduled unit of work qilla guarantees. Every routine has the same files, so status, budgets and doctor stay coherent. **Most routines need no model.**

## 1. Interview (short, in order)
1. **What does it produce?** A note section, a table, a reminder, a decision? Where in the vault (`output`)?
2. **What does it read?** Always ask *does this tool have a CLI?* — **use the CLI, query its database only when there is none** (and then read-only). That becomes the gather step.
3. **Does any step need judgment?** Summarising, prioritising, deciding, drafting text → yes. Transforming, listing, diffing, counting → no.
4. **When?** systemd `OnCalendar` (`*-*-* 08:30`, `Mon *-*-* 09:00`, `*-*-* 08,13,18:00` for the same minute; different minutes → several specs separated by `;`: `*-*-* 08:30; *-*-* 13:00`). `qilla doctor` validates every spec. **Window** it may run in. `must_run` **only for things that must happen every day** — a missed brief matters, a missed scout does not.
5. **What must it remember between runs?** State goes through a `qilla` subcommand (`qilla task`, `qilla remind`, `qilla runs`) or a file the gather reads back — **never a helper script in `~/.local/bin`**. The run's own outcome belongs in working memory: return `{"kind":"state","key":"<routine>/last","text":"<what was judged, what is pending>"}` in `remember` (last-write-wins, always injected first next run) and read it back with `mem.state()` in the gather.

## 2. Decide the kind
| Answer to Q3 | Kind | AI cost |
|---|---|---|
| no judgment | `script` — gather → template.md | none |
| judgment on fresh data | `ai-fresh` — gather → digest → `claude -p` → template | one call, only when input changed |
| judgment that needs the conversation | `ai-resumed` — the chief's session | one call |

Rules:
- **`kind = "script"` first.** A model only where judgment is actually needed, and only for the smallest step that needs it; everything around it stays in the gather. If an `ai-fresh` gather's output rarely changes, the digest skip makes it nearly free.
- For an ai kind, set a **tier, never a model id**: `tier = "classify" | "extract" | "research" | "synthesis" | "judgment" | "coding"` — classify/extract for bucketing and field-pulling, research for reading, synthesis for verdicts, judgment for decisions the owner would otherwise make, coding for patches. Tiers degrade automatically when the weekly subscription window runs low; a pinned model punches through that. **Fable is never a routine default.**
- **`budget` ≥ 6× the expected per-run cost** (a daily routine must survive a bad week and a retry). `allowed_tools` stays minimal — list only what the prompt genuinely needs.
- Anything authenticated happens in the gather: `qilla secret set <name>`, read in the gather from `$QILLA_SECRETS_DIR`/`ctx.secrets_dir`. **The model never sees a secret.**

## 3. Scaffold
```sh
qilla new routine <name> --kind script --schedule "*-*-* 06:30" --window 06:00-09:00 \
  --output "Desk/Journal/{{date}}.md" --append
# ai kinds add: --kind ai-fresh --agent chief
# --sh scaffolds gather.sh (any language) instead of the default gather.star
# --must-run only if a missed day matters
```
A routine is an **isolated bundle**: one directory, `Qilla/Routines/<name>/`, holding its own gather, its own `template.md` and its own `prompt.md`. No shared helper scripts between routines — if two routines need the same thing, each gathers it, or it becomes a `qilla` subcommand. Then edit in the vault:

- **`gather.star`** — the default, see below. `gather.sh` (`--sh`) is the escape hatch: any language, prints ONE JSON object, env `QILLA_VAULT`, `QILLA_ROUTINE`, `QILLA_DATE`, `QILLA_MEM_SEARCH`, `QILLA_SECRETS_DIR`; `set -eu`. **Never a `jq` pipeline** — if you are reaching for `jq`, you want `gather.star`, which speaks JSON natively. If a bundle ships both, `gather.sh` wins and doctor warns.
- `template.md` — [knap](https://github.com/obsidianmd/knap): `{{ gathered.x }}`, `{{ result.x }}`, `{% for %}`, filters `date`, `wikilink`, `table`, `yaml_property`. `knap help filters`.
- `prompt.md` (ai only) — say what to decide, forbid restating input, **require ONE JSON object** with the fields template.md uses.
- In `qilla.toml`: tighten `scope` to the minimum layers, set the `budget` and the `tier`.

### `gather.star` — the built-in Starlark gather (the default)
Python-shaped, hermetic, JSON-native, **nothing to install**: qilla runs it in-process. Define `def gather(ctx):` returning a dict (or set a top-level `result`); qilla JSON-encodes it exactly like gather.sh stdout, so digests keep working. A non-dict result is an error.

`ctx` carries `.routine`, `.date`, `.vault`, `.env` (the QILLA_* vars) and `.secrets_dir` (`""` when none).

The helper set is **frozen** — no imports, no `open` for writing, no network. The posture is **read + run only**: a gather *reads* files and *runs* commands; anything it needs to persist goes through `run(["qilla", …])`.

| Helper | Does |
|---|---|
| `json` | `json.encode/decode/indent` |
| `time` · `math` | starlark-go's stdlib modules (`time.from_timestamp(s).format("2006-01-02")`, `math.round`) |
| `re.match(pat, s)` | bool |
| `re.find(pat, s)` | first whole match, or `None` |
| `re.findall(pat, s)` | all whole matches; with groups, a list of group-lists |
| `re.groups(pat, s)` | submatches of the first match, or `None` |
| `re.sub(pat, repl, s)` | replace — **Go syntax: `$1`, `${name}`**, not `\1` |
| `re.split(pat, s)` | list of pieces |
| `sqlite.query(path, sql, params=[])` | list of dicts keyed by column. **Read-only**: `mode=ro`, one `SELECT`/`WITH`/`PRAGMA table_info`, 10 000 rows max. Path may be `~`-relative. **Last resort — a tool's CLI first** |
| `run(cmd, timeout=30, input=None, env=None)` | `{"rc", "out", "err"}`; `cmd` is a list (no shell), cwd = the vault, QILLA_* inherited. **Never raises on a non-zero rc** — the script decides |
| `read(path)` · `exists(path)` | vault-relative or absolute |
| `glob(pat)` · `listdir(path)` | sorted, vault-relative results |
| `mtime(path)` · `now()` | unix seconds (float) |
| `mem.search(query, n=5)` · `mem.state(routine="")` | read-only working memory. `mem.state()` is this routine's own last outcome (kind `state`, newest per key) — use it to skip what the routine already judged instead of re-diffing the vault |
| `env(name, default="")` | a QILLA_* var or the process env |
| `fail(msg)` · `print(…)` | abort the gather · goes to the run's stderr, kept with the error |

Regexps are **RE2** (Go): no backreferences, no lookaround. Patterns are compiled once and cached.

```python
def gather(ctx):
    # run: a command, no shell, rc is yours to handle
    r = run(["gcalcli", "agenda", "--tsv"], timeout = 60)
    if r["rc"] != 0:
        fail("gcalcli: " + (r["err"] or r["out"]).strip())

    # re: RE2, findall over the output
    days = re.findall(r"\d{4}-\d{2}-\d{2}", r["out"])

    # sqlite: only when the tool has no CLI — read-only, bind your params
    rows = sqlite.query("~/.local/share/app/app.db",
                        "SELECT id, title FROM items WHERE done = ? LIMIT 200", [0])

    return {"summary": "%d events" % len(days),
            "state": {"days": days, "items": rows}}
```

Reference bundle: `Qilla/Routines/retro/gather.star`.

## Shareable by default

A routine bundle is **code someone else can install**. Nothing in `Qilla/Routines/<name>/` may be specific to one user:

- **User-specific criteria live in settings.** Accounts, URLs, thresholds, names, feed lists → `[routines.<name>.settings]` in `qilla.toml`. The gather reads them: `settings["accounts"]` in `gather.star` (also `ctx.settings`), `$QILLA_SETTINGS` (a JSON object) in `gather.sh`; templates see them as `settings`. Hardcoding an address, a domain or a person's name in a gather is a bug.
- **Longer user data is a user_file** in the bundle (`preferences.md`, `accounts.md`): declared in `[[user_files]]`, `required` only when the routine cannot work without it.
- **Every external tool the bundle can live without is an `[[optional]]` feature**, and the code checks it before use — if the setting/secret/command is absent, the feature is skipped, never an error.
- **Declare `[[signals]]`**: a cheap command proving the routine has something to work with (mail under that label, notes in that folder). A green install with no input is the failure people never notice.

That contract is `routine.toml` in the bundle:

```toml
name = "newsletters"
summary = "Triage newsletters from Gmail into a daily feed note"

[requires]                # hard: missing ⇒ the check fails, no timer, no run
commands = ["msgvault", "curl"]
secrets  = []             # names under `qilla secret set`
settings = ["accounts"]   # keys that must exist in [routines.newsletters.settings]

[[optional]]              # soft: missing ⇒ the feature is disabled, the check notes it
name     = "karakeep"
settings = ["karakeep_url"]
secrets  = ["karakeep"]
commands = []
hint     = "Set settings.karakeep_url and `qilla secret set karakeep` to save top stories."

[[user_files]]
path     = "preferences.md"
required = false
hint     = "Interests + services you use; ranking uses it."

[[signals]]
name   = "newsletter label"
run    = ["msgvault", "search", "label:Newsletter newer_than:30d", "--limit", "1", "--json"]
expect = "json-nonempty"  # nonempty | ok | json-nonempty
hint   = "No mail labelled Newsletter in the last 30 days: create the Gmail filters first."
```

`run` may contain `{{settings.key}}` placeholders and gets 20 s. A bundle without `routine.toml` is legacy, not an error — the check just says "no manifest".

**Run `qilla routine check <name>` before `qilla init`**: it prints requirement · status · hint and exits 1 on a hard failure. `qilla init` refuses to write the timer of a failing routine (`--force` overrides, and says so), `qilla run` refuses to run it (`--force`), and `qilla doctor` carries one soft row per failing routine.

## 4. Finish
```sh
qilla routine check <name>  # ALWAYS FIRST: requirements, optional features, signals
qilla init          # writes qilla-<name>.timer/.service (skips routines failing the check)
systemctl --user enable --now qilla-<name>.timer
qilla gather <name> # ALWAYS: the gather step alone, $0 — iterate here until the JSON is right
qilla doctor        # ALWAYS: template validates, schedule parses, timer active
qilla run <name>    # ai kinds only: ONE real run as the paid proof, then read the output note
```
Never claim a routine works without those. Log the new routine where the project keeps its decisions.

## Patterns
| Need | Kind | the gather does | template renders |
|---|---|---|---|
| **Calendar into the daily note** (script-only example) | `script`, `--output "Desk/Journal/{{date}}.md" --append`, no model, no budget | `gather.star`: `run(["gcalcli", "agenda", "--tsv", …])`, parse the columns with `re`, return `{"events": [...]}` | list under `## 📅` |
| **Email triage** (ai-fresh example) | `ai-fresh`, `tier = "classify"`, `budget = { block = 0.30 }`, `allowed_tools` = none | `gather.star`: unread threads as JSON (id, from, subject, snippet) via the mail CLI, token from `ctx.secrets_dir` | buckets + draft offers |
| Version checks | script | `mise ls-remote`, GitHub releases API | table with ⬆ marks |
| Morning brief | ai-fresh | agenda + inbox counts + open questions + yesterday's log | brief section |
| Weekly retro | ai-resumed | this week's journal paths, `qilla runs log --json`, `atuin history list` | retro note |
| Backup / sync | script | `git push`, `rsync` | one status line |

## Never
- A model call to format markdown (knap does it).
- A `jq` pipeline in a gather — that is what `gather.star` is for.
- A tool's database when it has a CLI.
- A helper script outside the bundle, or shared between routines.
- A pinned model id, or Fable, as a routine default.
- A user's account, domain, folder or name hardcoded in a gather instead of `settings`.
- An external tool used without an `[[optional]]` block and a code path that works without it.
- A routine without `window` when it costs money, or with a budget that cannot absorb a handful of runs.
- Overwriting an existing bundle (`qilla new routine` refuses; edit instead).
