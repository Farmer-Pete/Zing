#!/bin/bash
# Forwards an AskUserQuestion (or compatible) tool input to the Zing Command
# Center so the drawer can render the question + options. Sends the raw
# questions[0] object when present so the server preserves header / options /
# multiSelect; falls back to wrapping a plain .tool_input.question string.
input=$(cat)
session_id=$(echo "$input" | jq -r '.session_id // empty' 2>/dev/null)
[ -z "$session_id" ] && exit 0

question_obj=$(echo "$input" | jq -c '.tool_input.questions[0] // empty' 2>/dev/null)
if [ -z "$question_obj" ] || [ "$question_obj" = "null" ]; then
    plain=$(echo "$input" | jq -r '.tool_input.question // empty' 2>/dev/null)
    [ -z "$plain" ] && exit 0
    question_obj=$(jq -n --arg q "$plain" '{question: $q}')
fi

payload=$(jq -n --arg sid "$session_id" --argjson q "$question_obj" \
    '{session_id: $sid, question: $q}')
curl -s -X POST http://127.0.0.1:9876/command-center/session-question \
  -H "Content-Type: application/json" \
  -d "$payload" \
  -m 2 >/dev/null 2>&1
exit 0
