#!/bin/sh
# mem-consolidate · weekly. Feeds pending working-memory conflicts + promotion candidates to the model.
set -eu
conf=$(qilla mem conflicts --json 2>/dev/null || echo '[]')
promo=$(qilla mem promote --json 2>/dev/null || echo '[]')
printf '{"conflicts":%s,"promotable":%s}\n' "${conf:-[]}" "${promo:-[]}"
