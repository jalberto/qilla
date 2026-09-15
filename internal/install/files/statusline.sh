#!/usr/bin/env bash
# qilla status line — a qilla session must never look like a plain claude session.
# Purple badge + who/mode, then the user's own status line (if a `statusline` command exists on
# PATH, e.g. claudia-statusline) fed the same session JSON, so nothing they rely on is lost.
input=$(cat)
who="${QILLA_ROUTINE:-${QILLA_AGENT:-run}}"
mode="headless"; [[ -n "${QILLA_ATTACH:-}" ]] && mode="attach"
badge=$(printf "\033[45;30m ◆ qilla \033[0m \033[1;35m%s\033[0m·%s" "$who" "$mode")
base="${QILLA_STATUSLINE_BASE:-}"
[[ -z "$base" ]] && command -v statusline >/dev/null 2>&1 && base=statusline
if [[ -n "$base" && -z "${QILLA_STATUSLINE_PLAIN:-}" ]]; then
  rest=$(printf '%s' "$input" | QILLA_RUN=1 sh -c "$base" 2>/dev/null | head -1)
  if [[ -n "$rest" ]]; then
    # a base that already knows qilla (prints the badge itself) is passed through untouched
    [[ "$rest" == *qilla* ]] && { printf "%s\n" "$rest"; exit 0; }
    printf "%s %s\n" "$badge" "$rest"; exit 0
  fi
fi
model=$(printf '%s' "$input" | jq -r '.model.display_name // "?"' 2>/dev/null || echo "?")
cpct=$(printf '%s' "$input" | jq -r '.context_window.used_percentage // empty' 2>/dev/null || true); cpct=${cpct%%.*}
ctx=""; [[ "$cpct" =~ ^[0-9]+$ ]] && { if (( cpct > 80 )); then ctx=" · \033[1;31m◔${cpct}%\033[0m"; else ctx=" · ◔${cpct}%"; fi; }
upct=$(printf '%s' "$input" | jq -r '.rate_limits.seven_day.used_percentage // empty' 2>/dev/null || true); upct=${upct%%.*}
usage=""; [[ "$upct" =~ ^[0-9]+$ ]] && { if (( upct > 80 )); then usage=" · \033[1;31m⚡${upct}%\033[0m"; elif (( upct >= 70 )); then usage=" · \033[1;33m⚡${upct}%\033[0m"; else usage=" · ⚡${upct}%"; fi; }
printf "%s · %s%b%b\n" "$badge" "$model" "$ctx" "$usage"
