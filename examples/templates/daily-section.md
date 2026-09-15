## 📅 {{ date }}
{% if gathered.events %}
{% for e in gathered.events %}- {{ e.start | date:"HH:mm" }} · **{{ e.title }}**{% if e.location %} · {{ e.location }}{% endif %}
{% endfor %}{% else %}- nothing scheduled
{% endif %}
