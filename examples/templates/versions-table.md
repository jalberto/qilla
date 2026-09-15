---
updated: {{ date }}
---
# Versions

| Tool | Installed | Latest | |
| - | - | - | - |
{% for t in gathered.tools %}| {{ t.name }} | {{ t.installed }} | {{ t.latest }} | {% if t.installed != t.latest %}⬆{% endif %} |
{% endfor %}
