#!/bin/sh
# Claude Code PreToolUse guard (matcher Bash|Monitor). docs/HANDOVER.md §20.4.
#
# Reads the hook payload on stdin. Exit 0 allows the call; exit 2 blocks it and
# the reason on stderr goes back to the agent. The hook is configured with
# onFailure "block", so a crash here blocks too: this script fails closed.
#
# It is a seatbelt, not a wall (docs/security.md, W3). It matches command text,
# so it also blocks harmless text that merely *mentions* a forbidden command,
# e.g. an echo. That is why its own tests live in fixture files
# (scripts/dev/testdata/guard/*.json), never in inline commands.
#
# Needs: POSIX sh, awk, sed, grep, jq (shipped with macOS 15+ and on every
# GitHub runner). MM_GUARD_GH overrides the gh binary, for the fixture tests.
set -eu

payload=$(cat)
cmd=$(printf '%s' "$payload" | jq -r '.tool_input.command // empty')
[ -n "$cmd" ] || exit 0 # Monitor WebSocket calls carry tool_input.ws instead
cwd=$(printf '%s' "$payload" | jq -r '.cwd // empty')
{ [ -n "$cwd" ] && [ -d "$cwd" ]; } || cwd=$(pwd)
gh=${MM_GUARD_GH:-gh}

block() {
  printf 'agent-guard: blocked. %s\n' "$1" >&2
  printf 'This is a repo rule (docs/HANDOVER.md §20.4), not a glitch: do not retry, rephrase or route around it. If the step is truly needed, record it as a block for the owner.\n' >&2
  exit 2
}

bootstrap_done=0
if [ -n "${CLAUDE_PROJECT_DIR:-}" ] && [ -f "$CLAUDE_PROJECT_DIR/notes/bootstrap.done" ]; then
  bootstrap_done=1
fi

# Whole-text checks, before any parsing.
case $cmd in
  *--no-verify*) block "--no-verify skips the lefthook pre-push gate. Fix the pre-pass instead." ;;
esac
# One simple command per line: split on ; & | newlines, subshell and
# substitution openers, drop quotes (so `sh -c 'git push -f'` is seen too) and
# leading VAR=value assignments and wrapper words.
# shellcheck disable=SC2016,SC2020 # literal $( and backtick; tr maps three separators to newlines
segments=$(printf '%s\n' "$cmd" | tr '\n' ';' |
  sed -e 's/\$(/;/g' -e 's/`/;/g' -e 's/[(){}]/;/g' |
  tr ';&|' '\n\n\n' | tr -d "\"'" |
  sed -E -e 's/^[[:space:]]+//' -e 's/[[:space:]]+$//' \
    -e ':a' -e 's/^[A-Za-z_][A-Za-z0-9_]*=[^[:space:]]*[[:space:]]*//' \
    -e 's/^(command|exec|env|nohup|nice|time|sudo|xargs|rtk proxy|rtk)[[:space:]]+//' -e 'ta' |
  grep -v '^$' || true)

# Checks that need the whole text, but only when some segment runs git push
# (not when "push" is just a word, e.g. `gh run list --event push`).
if printf '%s\n' "$segments" | grep -Eq '(^|[[:space:]/])git([[:space:]]+[^[:space:]]+)*[[:space:]]+push([[:space:]]|$)'; then
  if printf '%s' "$cmd" | grep -Eiq 'LEFTHOOK=(0|false)|LEFTHOOK_EXCLUDE|hookspath'; then
    block "a push with LEFTHOOK=0, LEFTHOOK_EXCLUDE or core.hooksPath skips the pre-push gate. Fix the pre-pass instead."
  fi
  # shellcheck disable=SC2016 # a literal $( and backtick
  if [ "$bootstrap_done" = 1 ] && printf '%s' "$cmd" | grep -Eq '\$\(|`'; then
    block "a push in a command with \$( ) or backticks: after the bootstrap, push with a literal branch name (git push -u origin <branch>)."
  fi
fi

# Each segment yields at most one verdict line:
#   BLOCK <reason>    block now
#   CURRENT <dir>     a push of the current branch of <dir>: checked below
#   PR <arg>          a checkout of PR <arg>: its author is checked below
verdicts=$(printf '%s\n' "$segments" | awk -v boot="$bootstrap_done" '
function base(s) { sub(/.*\//, "", s); return s }
function reader(c) { return c ~ /^(cat|less|more|head|tail|sed|awk|grep|egrep|rg|wc|shellcheck|diff|stat|ls|file|vi|vim|nvim|nano|code|open|bat|git|veld)$/ }
function out(s) { print s; done = 1 }

function do_git(i,    j, o, dir, opaque, remote, nref, del, a, nm, dst) {
  dir = "."; opaque = 0
  for (j = i + 1; j <= n && t[j] ~ /^-/; j++) {
    o = t[j]
    if (o == "-C") { dir = t[j+1]; j++; continue }
    if (o == "-c") { if (tolower(t[j+1]) ~ /hookspath/) { out("BLOCK git -c core.hooksPath skips the pre-push gate."); return }; j++; continue }
    if (o ~ /^-C./) { dir = substr(o, 3); continue }
    if (o == "--git-dir" || o == "--work-tree" || o == "--namespace" || o == "--config-env" || o == "--super-prefix") { opaque = 1; j++; continue }
    if (o ~ /^--(git-dir|work-tree)=/) opaque = 1
  }
  if (t[j] != "push") return
  remote = ""; nref = 0; del = 0
  for (j = j + 1; j <= n; j++) {
    a = t[j]
    if (a ~ /\$/ && boot) { out("BLOCK git push with a variable remote or refspec: after the bootstrap, name the branch literally."); return }
    if (a ~ /^--/) {
      nm = a; sub(/=.*/, "", nm)
      if (nm == "--force") { out("BLOCK git push --force rewrites remote history. Use --force-with-lease, and only on your own PR branch."); return }
      if (nm ~ /^--mi(r(r(or?)?)?)?$/ || nm ~ /^--all?$/) { out("BLOCK git push --mirror/--all pushes every ref, main included."); return }
      if (nm ~ /^--no-ve/) { out("BLOCK git push --no-verify (or an abbreviation of it) skips the pre-push gate."); return }
      if (nm ~ /^--de(l(e(te?)?)?)?$/) del = 1
      if (a !~ /=/ && (nm == "--push-option" || nm == "--repo" || nm == "--receive-pack" || nm == "--exec")) j++
      continue
    }
    if (a ~ /^-/) {
      if (a ~ /f/) { out("BLOCK git push -f rewrites remote history. Use --force-with-lease, and only on your own PR branch."); return }
      if (a ~ /d/) del = 1
      if (a == "-o") j++
      continue
    }
    if (remote == "") { remote = a; continue }
    nref++
    if (a ~ /^\+/) { out("BLOCK git push +refspec is a force push. Use --force-with-lease on your own PR branch."); return }
    dst = a; sub(/^.*:/, "", dst)
    if (dst == "main" || dst == "refs/heads/main" || (del && a == "main")) {
      if (boot) { out("BLOCK git push to main: after the bootstrap every change goes through a PR (§20.2)."); return }
      continue
    }
    if (dst == "HEAD" || a == "HEAD") {
      if (boot && opaque) { out("BLOCK git push HEAD with --git-dir/--work-tree: name the PR branch literally."); return }
      out("CURRENT " dir); return
    }
  }
  if (nref == 0) {
    if (boot && opaque) { out("BLOCK git push without a refspec and with --git-dir/--work-tree: name the PR branch literally."); return }
    out("CURRENT " dir)
  }
}

function do_gh(i,    s, j, a, k, c, v, method, body, ep, settings) {
  s = t[i+1]
  if (s == "repo" && t[i+2] ~ /^(edit|delete|archive|rename)$/) { out("BLOCK gh repo edit/delete/archive/rename changes repository settings. That is the owner job."); return }
  if (s ~ /^(secret|variable|ruleset)$/) { out("BLOCK gh secret/variable/ruleset: this repo has no secrets or variables, and rulesets are the owner job."); return }
  if (s == "pr" && t[i+2] == "merge") {
    for (j = i + 3; j <= n; j++) if (t[j] ~ /^--admin(=|$)/) { out("BLOCK gh pr merge --admin: the ruleset has no bypass. Wait for green CI and merge without --admin."); return }
    return
  }
  if (s == "pr" && t[i+2] == "checkout") {
    for (j = i + 3; j <= n; j++) {
      a = t[j]
      if (a == "-b" || a == "--branch" || a == "-R" || a == "--repo") { j++; continue }
      if (a ~ /^-/) continue
      out("PR " a); return
    }
    return
  }
  if (s != "api") return
  method = ""; body = 0; ep = ""
  for (j = i + 2; j <= n; j++) {
    a = t[j]
    if (a ~ /^--/) {
      k = a; v = ""
      if (k ~ /=/) { v = k; sub(/^[^=]*=/, "", v); sub(/=.*/, "", k) }
      else if (k ~ /^--(method|header|raw-field|field|input|jq|template|cache|preview|hostname)$/) { v = t[j+1]; j++ }
      if (k == "--method") method = toupper(v)
      if (k ~ /^--(raw-field|field|input)$/) body = 1
      continue
    }
    if (a ~ /^-./) {
      for (k = 2; k <= length(a); k++) {
        c = substr(a, k, 1)
        if (c ~ /[XHfFqtp]/) {
          v = substr(a, k + 1); sub(/^=/, "", v)
          if (v == "") { v = t[j+1]; j++ }
          if (c == "X") method = toupper(v)
          if (c == "f" || c == "F") body = 1
          break
        }
      }
      continue
    }
    if (ep == "") ep = a
  }
  if (ep == "graphql") {
    if ($0 ~ /mutation/) out("BLOCK gh api graphql mutations can change refs, settings and rulesets. Use REST reads and the allowed gh commands.")
    return
  }
  if (method == "") method = body ? "POST" : "GET"
  if (method == "PUT" || method == "PATCH" || method == "DELETE") { out("BLOCK gh api -X " method " is a write the agent never makes (repository settings belong to the owner)."); return }
  settings = "/(rulesets|actions/permissions|actions/policies|environments|collaborators|teams|hooks|keys|immutable-releases)([/?]|$)"
  if (ep ~ settings && (method != "GET" || body)) out("BLOCK gh api " method " to a settings endpoint (rulesets, actions permissions or policies, environments, collaborators, teams, hooks, keys, immutable releases). Plain GETs only.")
}

{
  n = split($0, t, /[[:space:]]+/)
  done = 0
  for (i = 1; i <= n; i++) {
    if (base(t[i]) == "harden-public-repo.sh" && !(i > 1 && reader(base(t[1])))) {
      out("BLOCK scripts/dev/harden-public-repo.sh changes repository settings. Only the owner runs it, in their own terminal.")
      break
    }
  }
  if (done) next
  for (i = 1; i <= n; i++) {
    if (base(t[i]) == "git") { do_git(i); break }
    if (base(t[i]) == "gh") { do_gh(i); break }
  }
}')

reason=$(printf '%s\n' "$verdicts" | sed -n 's/^BLOCK //p' | head -n 1)
[ -z "$reason" ] || block "$reason"

if [ "$bootstrap_done" = 1 ]; then
  for d in $(printf '%s\n' "$verdicts" | sed -n 's/^CURRENT //p'); do
    case $d in /*) ;; *) d=$cwd/$d ;; esac
    b=$(git -C "$d" symbolic-ref --quiet --short HEAD 2>/dev/null || echo "(detached)")
    case $b in
      main) block "git push from main: after the bootstrap every change goes through a PR (§20.2)." ;;
      "(detached)") block "git push of a detached HEAD without an explicit branch refspec. Name the PR branch." ;;
    esac
  done
fi

# Never check out (and so build or run) a PR the agent did not author (A9).
prs=$(printf '%s\n' "$verdicts" | sed -n 's/^PR //p')
prs="$prs
$(printf '%s\n' "$segments" | grep -Eo '(^|[[:space:]:+])(refs/)?pull/[^/[:space:]]+/' | sed -E 's#.*pull/##; s#/$##' || true)"
prs=$(printf '%s\n' "$prs" | grep -v '^$' || true)
if [ -n "$prs" ]; then
  me=$("$gh" api user --jq .login) || block "could not read the logged-in GitHub user, so cannot tell whose PR this is."
  for pr in $prs; do
    pr=${pr##*/}
    case $pr in "" | *[!0-9]*) block "PR checkout of '$pr': name the PR by number so its author can be checked." ;; esac
    author=$(cd "$cwd" && "$gh" pr view "$pr" --json author --jq .author.login) ||
      block "could not read the author of PR #$pr."
    [ "$author" = "$me" ] ||
      block "PR #$pr is by $author, not you. Never check out, build or test a PR you did not author (A9). Read it with gh pr diff $pr and label it needs-maintainer."
  done
fi

exit 0
