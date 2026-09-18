package install

import "fmt"

// Routine scaffolds shared by `qilla new routine` and Ensure (which fills in
// whatever a configured routine is missing so a run never fails on a missing file).

// RoutineManifest scaffolds routine.toml: the bundle's contract. A routine is
// shareable code; everything user-specific lives in
// [routines.<name>.settings] and in user_files, and the manifest is what makes
// that checkable: qilla routine check <name>.
func RoutineManifest(name, summary string) string {
	return fmt.Sprintf(`name = %q
summary = %q

# [requires] is hard: anything missing here means the routine cannot run, and
# "qilla routine check" fails (qilla init writes no timer, qilla run refuses).
[requires]
commands = []   # binaries that must be on PATH
secrets  = []   # names stored with "qilla secret set <name>"
settings = []   # keys that must exist in [routines.%s.settings] in qilla.toml

# Every external tool the bundle can do without is an [[optional]] feature:
# missing pieces disable it (the gather must check before using it) and the
# check prints a note instead of failing. Repeat the block per feature.
# [[optional]]
# name     = "karakeep"
# settings = ["karakeep_url"]
# secrets  = ["karakeep"]
# commands = []
# hint     = "Set settings.karakeep_url and store the karakeep secret to save top stories."

# Data the user provides, relative to this bundle. required = true fails the check.
# [[user_files]]
# path     = "preferences.md"
# required = false
# hint     = "Interests + services you use; ranking uses it."

# Evidence the routine has something to work with. run may contain
# {{settings.key}} placeholders; expect is nonempty | ok | json-nonempty.
# [[signals]]
# name   = "mail to triage"
# run    = ["msgvault", "search", "label:Newsletter newer_than:30d", "--limit", "1", "--json"]
# expect = "json-nonempty"
# hint   = "No mail labelled Newsletter in the last 30 days: create the Gmail filters first."
`, name, summary, name)
}

// RoutineGatherSh is the shell gather step (`qilla new routine --sh`):
// any language, prints one JSON object.
func RoutineGatherSh(name string) string {
	return fmt.Sprintf(`#!/bin/sh
# %s · gather step. Runs in the vault with QILLA_VAULT, QILLA_ROUTINE, QILLA_DATE set.
# Print ONE JSON object to stdout: it becomes {{ gathered }} in template.md and the
# input of prompt.md. Its hash is the digest: same output → the model is not called.
set -eu
printf '{"date":"%%s","items":[]}\n' "$QILLA_DATE"
`, name)
}

// RoutineGatherStar is the DEFAULT gather step (`qilla new routine`; --sh
// scaffolds the shell one instead): no interpreter to install, JSON-native,
// read + run only.
func RoutineGatherStar(name string) string {
	return fmt.Sprintf(`# %s · gather step, in Starlark (Python-shaped). qilla runs it in-process.
# Return ONE dict: it becomes {{ gathered }} in template.md and the input of
# prompt.md. Its JSON hash is the digest: same output -> the model is not called.
#
# Helpers (frozen): json time math re sqlite · run read exists glob listdir mtime env now fail print
# re is RE2 (no backreferences/lookaround); re.sub uses Go's $1 replacement syntax.
# Read + run only: no writing, no network, and sqlite.query is read-only.
# Prefer a tool's CLI over its database when it has one.
#
# test with: qilla gather %s

def gather(ctx):
    notes = glob("Desk/Journal/*.md")
    days = re.findall(r"\d{4}-\d{2}-\d{2}", " ".join(notes))

    r = run(["qilla", "task", "aging", "--brief"], timeout = 60)
    if r["rc"] != 0:
        fail("qilla task aging: " + (r["err"] or r["out"]).strip())

    # a local sqlite file, read-only, one SELECT (last resort — CLI first):
    # rows = sqlite.query("~/.local/share/app/app.db", "SELECT id, name FROM t LIMIT ?", [10])

    return {
        "date": ctx.date,
        "routine": ctx.routine,
        "summary": "%%d daily notes" %% len(notes),
        "state": {"days": days, "aging": r["out"].strip()},
    }
`, name, name)
}

// RoutineTemplateMd is the default knap template; script routines render the
// gathered items, ai routines the model's summary/actions.
func RoutineTemplateMd(name string, script bool) string {
	if script {
		return fmt.Sprintf(`## %s · {{ date }}
{%% for i in gathered.items %%}- {{ i }}
{%% endfor %%}{%% if gathered.items | length == 0 %%}- nothing today
{%% endif %%}
`, name)
	}
	return fmt.Sprintf(`## %s · {{ date }}
{{ result.summary }}
{%% for a in result.actions %%}- [ ] {{ a }}
{%% endfor %%}
`, name)
}

// RoutinePromptMd is the default task for ai routines.
func RoutinePromptMd(name string) string {
	return fmt.Sprintf(`You are running the routine **%s**. Input follows the task marker as JSON.

Decide only what needs judgment; do not restate the input.
Reply with ONE JSON object, nothing else:
{"summary": "<two sentences>", "actions": ["<what the owner should do>", …]}
`, name)
}

// DefaultIndex is <vault>/Qilla/Qilla.md — the user-space folder note.
const DefaultIndex = `---
type: index
tags: [qilla]
---
# Qilla

User space of the **qilla** runtime. Everything here is yours: qilla reads it, never ships it.

| Path | Role |
|---|---|
| ` + "`Persona.md`" + ` | who the assistant is — first person, editable |
| ` + "`Rules.md`" + ` | standing rules loaded into every AI run |
| ` + "`Routines/<name>/`" + ` | one bundle per routine: ` + "`gather.sh` or `gather.star`, `template.md`, `prompt.md`" + ` |
| ` + "`Research/`" + ` | research presets (` + "`default.md`" + ` …) |
| ` + "`Subagents/`" + ` | your sub-agent definitions |
| ` + "`Plugin/`" + ` | your Claude Code plugin (skills, hooks, agents) loaded only into qilla sessions |

Status, jobs and costs: ` + "`qilla status`" + ` or the web UI. Health: ` + "`qilla doctor`" + `.
`

// PluginManifest is the minimal .claude-plugin/plugin.json for a user plugin dir.
const PluginManifest = `{
  "name": "vault",
  "description": "User-space skills, hooks and agents loaded only into qilla sessions.",
  "version": "0.1.0"
}
`

// PluginHooks is an empty hooks.json (qilla's own hooks come from its runtime plugin).
const PluginHooks = "{\n  \"hooks\": {}\n}\n"

// NoopHook is the default body for a configured but missing user hook script.
const NoopHook = `#!/bin/sh
# qilla user hook — stdout is appended to the session context (session_start) or ignored (stop).
# Edit freely; qilla only regenerates this file when it is missing.
exit 0
`

// VaultSettings is <vault>/.claude/settings.json when the vault has none:
// it enables the qilla plugin for the vault project only. Permission rails stay yours.
const VaultSettings = `{
  "enabledPlugins": {
    "qilla@qilla": true
  }
}
`
