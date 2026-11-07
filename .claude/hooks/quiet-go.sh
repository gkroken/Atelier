#!/usr/bin/env bash
# PreToolUse hook for Bash: rewrite a bare `go test|vet|build ...` into the quiet
# wrappers, so passing output never reaches Claude's context. Only simple commands are
# rewritten (no pipes, redirects, `&&`, subshells or variables); anything else passes
# through untouched. Prefix a command with ATELIER_RAW=1 to bypass on purpose.
input=$(cat)
cmd=$(jq -r '.tool_input.command // ""' <<<"$input")
dir=${CLAUDE_PROJECT_DIR:-$(pwd)}

safe='^go (test|vet|build)( [^;&|<>`$#\\(){}[:cntrl:]]*)?$'
if [[ ! $cmd =~ $safe ]]; then
  echo '{}'
  exit 0
fi

sub=${BASH_REMATCH[1]}
rest=${cmd#go "$sub"}
case $sub in
  test) new="\"$dir/scripts/test.sh\"$rest" ;;
  *)    new="\"$dir/scripts/run-quiet.sh\" $sub -- go $sub$rest" ;;
esac

jq -c --arg c "$new" '{
  hookSpecificOutput: {
    hookEventName: "PreToolUse",
    updatedInput: (.tool_input + {command: $c}),
    additionalContext: "Ran through a quiet wrapper: only failures are shown, the full output is in .logs/. Prefix the command with ATELIER_RAW=1 to see raw output."
  }
}' <<<"$input"
