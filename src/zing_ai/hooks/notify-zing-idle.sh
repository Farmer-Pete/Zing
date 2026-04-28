#!/bin/bash
# Forwards a Claude Code Notification hook event to the Zing Command Center
# so the session can light up its notification dot / drawer / browser toast.
# Claude Code fires this hook when it shows a system notification (e.g. the
# prompt has been idle for ~60s waiting for user input, or a permission
# request is pending). The hook input includes session_id and message.
input=$(cat)
session_id=$(echo "$input" | jq -r '.session_id // empty' 2>/dev/null)
[ -z "$session_id" ] && exit 0

message=$(echo "$input" | jq -r '.message // empty' 2>/dev/null)
[ -z "$message" ] && message="Claude is waiting for your input"

payload=$(jq -n --arg sid "$session_id" --arg body "$message" \
    '{session_id: $sid, title: "Claude is waiting", body: $body}')
curl -s -X POST http://127.0.0.1:9876/command-center/session-idle \
  -H "Content-Type: application/json" \
  -d "$payload" \
  -m 2 >/dev/null 2>&1
exit 0
