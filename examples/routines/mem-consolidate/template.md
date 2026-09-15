## 🧠 Working memory · {{ date }}
{{ result.summary }}
{% if result.promote %}
> [!question] Promote to facts?
{% for p in result.promote %}> - [ ] {{ p.fact }} `mem:{{ p.id }}`
{% endfor %}{% endif %}
{% for j in result.judge %}{% if j.relation == "conflicts_with" %}- ⚠ conflict `{{ j.id }}`: {{ j.reason }}
{% endif %}{% endfor %}
