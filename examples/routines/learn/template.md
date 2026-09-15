
### 🧭 Learn · {{ date }}
{{ result.summary }}
{% for p in result.proposals %}- **{{ p.routine }}** · {{ p.change }}: {{ p.suggestion }} — {{ p.why }}
{% endfor %}
