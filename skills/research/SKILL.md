---
name: research
description: Deep research on any question → a sourced note. Recall first (vault + working memory), then fan out to parallel researcher sub-agents, then one synthesis with a verdict. Use for "research X", "is X worth it", "what's out there on X", "should I buy X", "is X outdated". Presets in the vault's Qilla/Research/ shape the verdict vocabulary and destination.
---

# qilla:research — recall, fan out, synthesize

Generic flow; the vault decides what a verdict means (presets) and where notes go.

## 0. Frame
- One question, written as a sentence. If the ask mixes several, split and pick the one asked.
- Preset: read `Qilla/Research/<preset>.md` if the ask names one (`idea`, `shopping`, …) or the routine's prompt does; otherwise `Qilla/Research/default.md`. A preset gives: verdict vocabulary, required columns, destination folder, note template name.

## 1. Recall before the internet
- Vault: `qmd query "<question>" --files -n 8` (or the configured `recall_cmd`), read the top hits that are actually about it.
- Working memory: `qilla mem search --project shared "<topic>"`.
- If recall already answers it, say so and stop. Otherwise list the 3–5 angles a good answer needs.

## 2. Fan out
- One **researcher** sub-agent per angle, in parallel, each with ONE focused question and the retrieval date. Read-only. They return findings + sources; they never write.
- Fetch pages with `qilla fetch <url>`; it climbs the ladder and classifies the page itself. Only if it exits 3 report the kind (blocked/login) and try hister/karakeep mirrors.
- Keep it to what the question needs (2–5 agents). Cost is the constraint: the `research` tier, not the chief's model.

## 3. Synthesize
- Weigh findings against the preset's criteria. Disagreements between sources are stated, not resolved by preference.
- Verdict from the preset's vocabulary, one line, with the single strongest reason.
- Sources: url · what it gave · date. Nothing unsourced is stated as fact.

## 4. Write
- Return JSON for the routine's knap template (`{"question","verdict","reason","findings":[{"claim","source","date"}],"open":[…],"summary"}`) when running as a routine; interactively, write the note with the preset's template into the preset's destination and link it from where the question came from.
- `remember`: one heuristic if something durable about *how to research this domain* was learned (which sources work, what to ignore).

## Never
- Fabricate or "recall" a source you did not open.
- Skip step 1: most questions are half-answered already.
- Let a researcher write to the vault.
