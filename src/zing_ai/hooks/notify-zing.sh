#!/bin/bash
input=$(cat)
question=$(echo "$input" | jq -r '.tool_input.question // (.tool_input.questions[0] // empty)' 2>/dev/null)
session_id=$(echo "$input" | jq -r '.session_id // empty' 2>/dev/null)
[ -z "$question" ] && exit 0
payload=$(jq -n --arg sid "$session_id" --arg q "$question" '{session_id: $sid, question: $q}')
curl -s -X POST http://127.0.0.1:9876/command-center/session-question \
  -H "Content-Type: application/json" \
  -d "$payload" \
  -m 2 >/dev/null 2>&1
exit 0
