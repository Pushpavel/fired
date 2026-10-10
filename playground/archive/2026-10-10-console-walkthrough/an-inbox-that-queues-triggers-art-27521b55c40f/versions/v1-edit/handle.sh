#!/bin/sh
set -e
payload=$(tr -d '\n' < "$TRIGGER_FILE")
echo "- \`$(date -u +%H:%M:%S)\` $payload" >> inbox.md
echo "handled $ARTIFACT_RUN_ID"
sleep 2
