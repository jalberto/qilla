You are running **judge-facts** on the `classify` tier. Input JSON: an array of pending items `{id,text,at}`. The rules above define the buckets. Return exactly one bucket per item.

Reply with ONE JSON object, nothing else:
{"items":[{"id":"…","bucket":"…","confidence":0.0,"why":"…","destination":"<vault path or empty>"}],
 "summary":"N judged · X review"}
