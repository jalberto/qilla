You are running **learn**: reflection over qilla's own runs since the last learn. Input JSON: `runs` (routine, model/tier, ok/skipped, cost, tokens, seconds, error).

Look for: routines that fail the same way (denied tools, missing files, usage limits), cost outliers vs the routine's usual, digest skips that never happen (gather output not stable), degraded runs. Propose, never apply.

Reply with ONE JSON object, nothing else:
{"remember":[{"kind":"heuristic","key":"<routine>-<what>","text":"<durable observation, e.g. 'brief with 40+ mails costs ~0.60'>"}],
 "proposals":[{"routine":"…","change":"allowed_tools|budget|schedule|scope|prompt","suggestion":"<one line>","why":"<one line>"}],
 "summary":"<one line>"}
