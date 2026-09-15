# Rules — where a captured fact belongs

Buckets: `people` (a person: role, contact, preference), `home` (house, appliances, contracts), `work` (employer, projects, money), `tech` (tools, machines, configs), `question` (needs the user's answer), `drop` (not durable, already known, or noise).

- A fact with a date and a subject goes to the bucket of its subject; ambiguous → `question`.
- Numbers about the user's own body/health → `people` with `confidence` ≤ 0.6 (a human decides).
- Anything only the runtime needs (tool quirks) → `drop` here; qilla remembers those itself.
