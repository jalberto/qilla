---
name: routine
description: Design and scaffold a qilla routine following the one pattern (TOML block, gather.sh, template.md, prompt.md, systemd timer). Use when asked to "add a routine", "automate X every day/week", "make qilla do X on a schedule", or when a task repeats and should run without asking. Defaults to a no-AI script routine; only adds a model call where judgment is needed.
---

# qilla:routine — one pattern for every routine

A routine is a scheduled unit of work qilla guarantees. Every routine has the same files, so status, budgets and doctor stay coherent. **Most routines need no model.**

## 1. Interview (short, in order)
1. **What does it produce?** A note section, a table, a reminder, a decision? Where in the vault (`output`)?
2. **What does it read?** APIs, files, commands. That becomes `gather.sh`.
3. **Does any step need judgment?** Summarising, prioritising, deciding, drafting text → yes. Transforming, listing, diffing, counting → no.
4. **When?** systemd `OnCalendar` (`*-*-* 08:30`, `Mon *-*-* 09:00`, `*-*-* 08,13,18:00` for the same minute; different minutes → several specs separated by `;`: `*-*-* 08:30; *-*-* 13:00`). `qilla doctor` validates every spec. **Window** it may run in. **must_run** if a missed day matters.

## 2. Decide the kind
| Answer to Q3 | Kind | AI cost |
|---|---|---|
| no judgment | `script` — gather.sh → template.md | none |
| judgment on fresh data | `ai-fresh` — gather → digest → `claude -p` → template | one call, only when input changed |
| judgment that needs the conversation | `ai-resumed` — the chief's session | one call |

Rule: **default `script`**. Pick `ai-*` only for the smallest step that needs judgment; everything around it stays in `gather.sh`. If an `ai-fresh` routine's gather output rarely changes, the digest skip makes it nearly free.

## 3. Scaffold
```sh
qilla new routine <name> --kind script --schedule "*-*-* 06:30" --window 06:00-09:00 --must-run \
  --output "Desk/Journal/{{date}}.md" --append
# ai kinds add: --kind ai-fresh --agent chief
```
Then edit in the vault `Qilla/Routines/<name>/`:
- `gather.sh` — prints ONE JSON object. Env: `QILLA_VAULT`, `QILLA_ROUTINE`, `QILLA_DATE`, `QILLA_MEM_SEARCH`, and `QILLA_SECRETS_DIR` when secrets exist (`token=$(cat "$QILLA_SECRETS_DIR/gcal")`; stored with `qilla secret set gcal`). The model never sees secrets: anything authenticated happens here. Use the tools already on the box (`gcalcli`, `curl`, `jq`, `qmd`, `git`), fail loudly (`set -eu`).
- `template.md` — [knap](https://github.com/obsidianmd/knap): `{{ gathered.x }}`, `{{ result.x }}`, `{% for %}`, filters `date`, `wikilink`, `table`, `yaml_property`. `knap help filters`.
- `prompt.md` (ai only) — say what to decide, forbid restating input, **require ONE JSON object** with the fields template.md uses.
- In `qilla.toml`: tighten `scope` to the minimum layers, set a `budget`, and pick a **tier** (`tier = "classify" | "extract" | "research" | "synthesis" | "judgment"`), not a model: classify/extract for bucketing and field-pulling, research for reading, synthesis for verdicts, judgment only for decisions the owner would otherwise make. Tiers degrade automatically when the weekly subscription window runs low.

## 4. Finish
```sh
qilla init          # writes qilla-<name>.timer/.service
systemctl --user enable --now qilla-<name>.timer
qilla run <name>    # one real run, read the output note
qilla doctor        # template validates, timer active
```
Log the new routine where the project keeps its decisions.

## Patterns
| Need | Kind | gather.sh does | template renders |
|---|---|---|---|
| Calendar into the daily note | script | `gcalcli --tsv agenda … \| jq` | list under `## 📅` |
| Version checks | script | `mise ls-remote`, GitHub releases API | table with ⬆ marks |
| Email triage | ai-fresh | unread threads as JSON (ids, from, subject, snippet) | buckets + draft offers |
| Morning brief | ai-fresh | agenda + inbox counts + open questions + yesterday's log | brief section |
| Weekly retro | ai-resumed | this week's journal paths | retro note |
| Backup / sync | script | `git push`, `rsync` | one status line |

## Never
- A model call to format markdown (knap does it).
- A routine without `window` when it costs money.
- Overwriting an existing bundle (`qilla new routine` refuses; edit instead).
