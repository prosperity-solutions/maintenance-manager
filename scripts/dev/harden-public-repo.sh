#!/bin/sh
# Hardens prosperity-solutions/maintenance-manager and makes it public
# (docs/HANDOVER.md §21.3).
#
# The OWNER runs this, in their own terminal, as a repository admin:
#   sh scripts/dev/harden-public-repo.sh
# Agents never run it (§18); the agent guard blocks it. It is pure `gh api`,
# idempotent and safe to re-run, and ends by printing every setting as read back.
set -eu

REPO=prosperity-solutions/maintenance-manager
R=repos/$REPO

step() { printf '\n▶ %s\n' "$1"; }

# Create or update a ruleset by name.
ruleset() { # $1 name, $2 JSON body
  # This repository's own rulesets only, not ones inherited from the org.
  id=$(gh api "$R/rulesets?includes_parents=false" --jq ".[] | select(.name == \"$1\") | .id")
  if [ -n "$id" ]; then
    printf '%s' "$2" | gh api -X PUT "$R/rulesets/$id" --input - --silent
    echo "updated ruleset $1 ($id)"
  else
    printf '%s' "$2" | gh api -X POST "$R/rulesets" --input - --silent
    echo "created ruleset $1"
  fi
}

# --- 1. While still private -------------------------------------------------

step "Workflow token: read-only by default; Actions may not approve PRs"
gh api -X PUT "$R/actions/permissions/workflow" \
  -f default_workflow_permissions=read -F can_approve_pull_request_reviews=false --silent

step "Actions: SHA-pinned only; GitHub-owned plus an explicit list (order matters)"
printf '%s' '{"enabled": true, "allowed_actions": "selected", "sha_pinning_required": true}' |
  gh api -X PUT "$R/actions/permissions" --input - --silent
printf '%s' '{"github_owned_allowed": true, "verified_allowed": false, "patterns_allowed": ["step-security/harden-runner@*", "zizmorcore/zizmor-action@*"]}' |
  gh api -X PUT "$R/actions/permissions/selected-actions" --input - --silent

step "Merges: squash only, commit title = PR title, delete branch on merge, no auto-merge"
gh api -X PATCH "$R" -F allow_squash_merge=true -F allow_merge_commit=false -F allow_rebase_merge=false \
  -f squash_merge_commit_title=PR_TITLE -f squash_merge_commit_message=PR_BODY \
  -F delete_branch_on_merge=true -F allow_auto_merge=false --silent

step "Dependabot alerts, then security updates"
gh api -X PUT "$R/vulnerability-alerts" --silent
gh api -X PUT "$R/automated-security-fixes" --silent

step "Immutable releases"
gh api -X PUT "$R/immutable-releases" --silent

step "Labels"
gh label create no-ship --repo "$REPO" --force --color B60205 \
  --description "Owner waiver: skip the ship-stamp check"
gh label create needs-maintainer --repo "$REPO" --force --color FBCA04 \
  --description "Needs the owner: a decision, access, or a PR the agent must not touch"

# --- 2. Public --------------------------------------------------------------

step "Visibility: public"
if [ "$(gh api "$R" --jq .visibility)" = public ]; then
  echo "already public"
else
  gh repo edit "$REPO" --visibility public --accept-visibility-change-consequences
fi

# --- 3. Public-only settings ------------------------------------------------

step "Secret scanning and push protection"
gh api -X PATCH "$R" -f 'security_and_analysis[secret_scanning][status]=enabled' \
  -f 'security_and_analysis[secret_scanning_push_protection][status]=enabled' --silent

step "Private vulnerability reporting (SECURITY.md points to it)"
gh api -X PUT "$R/private-vulnerability-reporting" --silent

step "Fork PRs: approval required for all external contributors"
gh api -X PUT "$R/actions/permissions/fork-pr-contributor-approval" \
  -f approval_policy=all_external_contributors --silent

step "Workflow execution protection: only push and pull_request may start a workflow"
# No `|| true` here: if the GET fails, stop rather than assume the policy exists.
existing=$(gh api "$R/actions/policies" --jq '.. | objects | select(.name? == "events-push-pr") | .name')
if [ "$existing" = events-push-pr ]; then
  echo "policy events-push-pr already exists"
else
  printf '%s' '{"name": "events-push-pr", "enforcement": "active", "rules": [{"type": "restrict_action_events", "parameters": {"allowed_events": ["push", "pull_request"]}}]}' |
    gh api -X POST "$R/actions/policies" --input - --silent
  echo "created policy events-push-pr"
fi

# --- 4. Rulesets (after going public: rulesets on private repos need a paid plan)

step "Ruleset main: PR required, all-green required, branch up to date, no bypass"
sha=$(gh api "$R/commits/main" --jq .sha)
app=$(gh api "$R/commits/$sha/check-runs?check_name=all-green" --jq '.check_runs[0].app.id // empty')
if [ -z "$app" ]; then
  echo "No all-green check run on main ($sha). Wait for the bootstrap CI run to finish, then re-run this script." >&2
  exit 1
fi
ruleset main "$(cat <<EOF
{"name": "main", "target": "branch", "enforcement": "active", "bypass_actors": [],
 "conditions": {"ref_name": {"include": ["~DEFAULT_BRANCH"], "exclude": []}},
 "rules": [
  {"type": "deletion"},
  {"type": "non_fast_forward"},
  {"type": "pull_request", "parameters": {
    "required_approving_review_count": 0, "require_code_owner_review": false,
    "dismiss_stale_reviews_on_push": false, "require_last_push_approval": false,
    "required_review_thread_resolution": false, "allowed_merge_methods": ["squash"]}},
  {"type": "required_status_checks", "parameters": {
    "strict_required_status_checks_policy": true, "do_not_enforce_on_create": false,
    "required_status_checks": [{"context": "all-green", "integration_id": $app}]}}
 ]}
EOF
)"

step "Ruleset tags-v: release tags can be created, never moved or deleted"
ruleset tags-v '{"name": "tags-v", "target": "tag", "enforcement": "active", "bypass_actors": [],
 "conditions": {"ref_name": {"include": ["refs/tags/v*"], "exclude": []}},
 "rules": [{"type": "update", "parameters": {"update_allows_fetch_and_merge": false}}, {"type": "deletion"}]}'

# --- 5. GitHub Pages (docs/HANDOVER.md §15.1) --------------------------------

step "GitHub Pages: deployed by CI's pages job, at maintenance-manager.oss.life.li"
# The DNS record comes first: CNAME maintenance-manager.oss.life.li →
# prosperity-solutions.github.io.
if gh api "$R/pages" --silent 2>/dev/null; then
  gh api -X PUT "$R/pages" -f build_type=workflow --silent
  echo "Pages already on"
else
  gh api -X POST "$R/pages" -f build_type=workflow --silent
  echo "Pages turned on"
fi
gh api -X PUT "$R/pages" -f cname=maintenance-manager.oss.life.li --silent
# Only main may deploy the site: a branch run could otherwise publish through
# the pages job. Set explicitly, since CI creates the environment without a
# policy if it gets there first.
printf '%s' '{"deployment_branch_policy": {"protected_branches": false, "custom_branch_policies": true}}' |
  gh api -X PUT "$R/environments/github-pages" --input - --silent
if gh api "$R/environments/github-pages/deployment-branch-policies" --jq '.branch_policies[].name' | grep -qx main; then
  echo "github-pages already deploys from main only"
else
  gh api -X POST "$R/environments/github-pages/deployment-branch-policies" -f name=main -f type=branch --silent
  echo "github-pages now deploys from main only"
fi
# GitHub issues the certificate a few minutes after the domain is set; until
# then it refuses to enforce HTTPS.
if err=$(gh api -X PUT "$R/pages" -F https_enforced=true --silent 2>&1); then
  echo "HTTPS enforced"
else
  printf 'HTTPS not enforced yet (%s). Usually the certificate is still being issued: re-run this script in 15 minutes.\n' "$err" >&2
fi

# --- 6. Summary, as read back -----------------------------------------------

step "Summary (read back from GitHub)"
gh api "$R" --jq '{visibility, allow_squash_merge, allow_merge_commit, allow_rebase_merge, squash_merge_commit_title, delete_branch_on_merge, allow_auto_merge, security_and_analysis}'
gh api "$R/actions/permissions/workflow"
gh api "$R/actions/permissions"
gh api "$R/actions/permissions/selected-actions"
gh api "$R/actions/permissions/fork-pr-contributor-approval"
gh api "$R/actions/policies" || true
printf 'vulnerability alerts: '
if gh api "$R/vulnerability-alerts" --silent 2>/dev/null; then echo enabled; else echo DISABLED; fi
printf 'automated security fixes: '; gh api "$R/automated-security-fixes" --jq .enabled
printf 'immutable releases: '; gh api "$R/immutable-releases" --jq .enabled
printf 'private vulnerability reporting: '; gh api "$R/private-vulnerability-reporting" --jq .enabled
gh api "$R/pages" --jq '{build_type, cname, https_enforced}'
gh api "$R/environments/github-pages" --jq .deployment_branch_policy
printf 'github-pages deploys from: '
gh api "$R/environments/github-pages/deployment-branch-policies" --jq '[.branch_policies[].name] | join(" ")' || echo NONE
for id in $(gh api "$R/rulesets" --jq '.[].id'); do
  gh api "$R/rulesets/$id" --jq '{name, target, enforcement, bypass_actors, conditions, rules}'
done
gh label list --repo "$REPO" --search no-ship
gh label list --repo "$REPO" --search needs-maintainer

printf '\n✅ Done. Tell Claude; it verifies every setting with read-only calls.\n'
