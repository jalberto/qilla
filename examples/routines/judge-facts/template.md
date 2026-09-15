
### ⚖️ Facts judged · {{ date }}
{% for i in result.items %}- {{ i.bucket }} ({{ i.confidence }}) · {{ i.why }}{% if i.destination %} → {{ i.destination }}{% endif %}
{% endfor %}
