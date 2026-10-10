#!/bin/sh
set -e
# Under the fake driver, guest paths live under the sandbox's root folder.
f="$TRIGGER_FILE"; [ -f "$f" ] || f="$PWD/..$TRIGGER_FILE"
payload=$(tr -d '\n' < "$f")
echo "- \`$(date -u +%H:%M:%S)\` $payload" >> inbox.md
echo "handled $ARTIFACT_RUN_ID"
sleep 2
