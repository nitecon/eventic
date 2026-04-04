#!/usr/bin/env bash
set -euo pipefail

# ---------------------------------------------------------------------------
# setup-eventic-webhooks.sh
#
# Iterates over every repository the authenticated GitHub user has admin
# access to (personal repos + org repos) and ensures an Eventic webhook is
# configured on each one.
#
# Usage:
#   ./setup-eventic-webhooks.sh --url <webhook-url> --secret <webhook-secret> \
#       [--owner <owner> ...] [--dry-run]
#
# Options:
#   --url       The Eventic webhook endpoint URL (required).
#   --secret    The webhook secret used to sign payloads (required).
#   --owner     GitHub user or org to process. Can be specified multiple
#               times. When omitted the script auto-detects the authenticated
#               user and all organisations they belong to.
#   --dry-run   Show what would be created without making changes.
#
# Requirements: gh (GitHub CLI) authenticated with appropriate scopes, jq.
# ---------------------------------------------------------------------------

WEBHOOK_URL=""
CONTENT_TYPE="json"
INSECURE_SSL="0"
EVENTS='["*"]'          # wildcard – all events

SECRET=""
DRY_RUN=false
OWNERS=()

# ---- Parse CLI args -------------------------------------------------------
while [[ $# -gt 0 ]]; do
  case "$1" in
    --url)
      WEBHOOK_URL="$2"
      shift 2
      ;;
    --secret)
      SECRET="$2"
      shift 2
      ;;
    --owner)
      OWNERS+=("$2")
      shift 2
      ;;
    --dry-run)
      DRY_RUN=true
      shift
      ;;
    -h|--help)
      echo "Usage: $0 --url <webhook-url> --secret <webhook-secret> [--owner <owner> ...] [--dry-run]"
      exit 0
      ;;
    *)
      echo "Unknown option: $1"
      echo "Usage: $0 --url <webhook-url> --secret <webhook-secret> [--owner <owner> ...] [--dry-run]"
      exit 1
      ;;
  esac
done

if [[ -z "$WEBHOOK_URL" ]]; then
  echo "ERROR: --url is required."
  echo "Usage: $0 --url <webhook-url> --secret <webhook-secret> [--owner <owner> ...] [--dry-run]"
  exit 1
fi

if [[ -z "$SECRET" ]]; then
  echo "ERROR: --secret is required."
  echo "Usage: $0 --url <webhook-url> --secret <webhook-secret> [--owner <owner> ...] [--dry-run]"
  exit 1
fi

# ---- Auto-detect owners if none supplied -----------------------------------
if [[ ${#OWNERS[@]} -eq 0 ]]; then
  echo "No --owner specified, auto-detecting from GitHub CLI..."

  current_user=$(gh api user --jq '.login')
  OWNERS+=("$current_user")
  echo "  Found user: $current_user"

  while IFS= read -r org; do
    [[ -z "$org" ]] && continue
    OWNERS+=("$org")
    echo "  Found org:  $org"
  done < <(gh api user/orgs --jq '.[].login')

  echo ""
fi

CREATED=0
SKIPPED=0
FAILED=0

for owner in "${OWNERS[@]}"; do
  echo "=========================================="
  echo " Processing owner: $owner"
  echo "=========================================="

  # Fetch all repos for this owner (handles pagination via --limit)
  repos=$(gh repo list "$owner" --limit 500 --json nameWithOwner --jq '.[].nameWithOwner')

  while IFS= read -r repo; do
    [[ -z "$repo" ]] && continue

    # List existing webhooks and check for the target URL
    hooks=$(gh api "repos/${repo}/hooks" 2>/dev/null || echo "ERROR")

    if [[ "$hooks" == "ERROR" ]]; then
      echo "  [SKIP] $repo - unable to list hooks (no admin access?)"
      ((SKIPPED++)) || true
      continue
    fi

    has_eventic=$(echo "$hooks" | jq -r --arg url "$WEBHOOK_URL" '
      [.[] | select(.config.url == $url)] | length
    ')

    if [[ "$has_eventic" -gt 0 ]]; then
      echo "  [OK]   $repo - webhook already exists"
      ((SKIPPED++)) || true
      continue
    fi

    # No matching webhook found – create one
    if [[ "$DRY_RUN" == true ]]; then
      echo "  [DRY]  $repo - would create webhook"
      ((CREATED++)) || true
      continue
    fi

    payload=$(jq -n \
      --arg url "$WEBHOOK_URL" \
      --arg ct "$CONTENT_TYPE" \
      --arg ssl "$INSECURE_SSL" \
      --arg secret "$SECRET" \
      --argjson events "$EVENTS" \
      '{
        name: "web",
        active: true,
        events: $events,
        config: {
          url: $url,
          content_type: $ct,
          insecure_ssl: $ssl,
          secret: $secret
        }
      }')

    result=$(gh api "repos/${repo}/hooks" --method POST --input - <<< "$payload" 2>&1) && rc=$? || rc=$?

    if [[ $rc -eq 0 ]]; then
      hook_id=$(echo "$result" | jq -r '.id')
      echo "  [NEW]  $repo - created webhook (id: ${hook_id})"
      ((CREATED++)) || true
    else
      echo "  [FAIL] $repo - $result"
      ((FAILED++)) || true
    fi

  done <<< "$repos"

  echo ""
done

echo "=========================================="
echo " Summary"
echo "=========================================="
echo "  Created : $CREATED"
echo "  Skipped : $SKIPPED"
echo "  Failed  : $FAILED"
