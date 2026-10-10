#!/bin/sh
set -e
mkdir -p data .state
n=$(cat .state/count 2>/dev/null || echo 0)
n=$((n + 1))
echo "$n" > .state/count
printf '{"runs": %s, "at": "%s", "from_version": %s, "trigger": "%s"}\n' \
  "$n" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$ARTIFACT_VERSION" "$TRIGGER" > data/now.json
echo "run $n of $ARTIFACT_JOB, wrote data/now.json"
