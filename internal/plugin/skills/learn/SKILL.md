---
name: learn
description: End-of-job reflection for an interactive qilla session — what was learned that should change how the next session behaves. Use when a substantial task just finished, when asked "what did you learn", or when the Stop hook prompts it. Stores runtime heuristics in working memory (qilla mem) and proposes, never applies, changes to routines or rules.
---

# qilla:learn — the checkpoint

Bar: would this change how the next run behaves? If not, say "nothing to learn" and stop.

1. **Runtime heuristics** (tool quirks, what blocked what, cost observations, which sources worked): `qilla mem add --project shared --kind heuristic --key <slug> "<one line>"`. One idea per call.
2. **Proposals** (a routine's allowlist, budget, schedule, scope, prompt; a rule): write them where the vault keeps proposals (the user's convention), never edit `qilla.toml` or `Rules.md` yourself.
3. **Corrections from the user**: restate the rule in one line and store it as a `heuristic` with key `rule-<topic>`; if the user's vault has a place for standing judgments, propose the line there.
4. Reply in one line: `✓ learn: <what changed>` or `✓ nothing to learn`.
