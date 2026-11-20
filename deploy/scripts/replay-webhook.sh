#!/usr/bin/env bash
# Sends a fixture as a signed GitHub webhook to a running gha-tracer.
#   GITHUB_WEBHOOK_SECRET=... deploy/scripts/replay-webhook.sh workflow_run testdata/workflow_run_completed.json [url]
set -euo pipefail
event=${1:?event type (workflow_run)}
file=${2:?payload file}
url=${3:-http://localhost:8080/webhook}
: "${GITHUB_WEBHOOK_SECRET:?set GITHUB_WEBHOOK_SECRET}"
sig=$(openssl dgst -sha256 -hmac "$GITHUB_WEBHOOK_SECRET" -hex < "$file" | sed 's/^.* //')
curl -sS -o /dev/null -w '%{http_code}\n' -X POST "$url" \
  -H "Content-Type: application/json" \
  -H "X-GitHub-Event: $event" \
  -H "X-GitHub-Delivery: replay-$(date +%s%N)" \
  -H "X-Hub-Signature-256: sha256=$sig" \
  --data-binary @"$file"
