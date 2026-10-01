#!/usr/bin/env bash
# Checks subjects against Conventional Commits, the form release-please
# reads to version releases and write CHANGELOG.md (see CONTRIBUTING.md).
#
#   conventional-commits.sh title "<subject>"...   check the given subjects
#   conventional-commits.sh range <base>..<head>   check each non-merge
#                                                  commit in the range
#
# Accepted: <type>[(<scope>)][!]: <summary>, type one of the list below,
# scope like `template` or `e2e/golden`; also git's `Revert "<subject>"`.
set -euo pipefail

types='feat|fix|perf|refactor|docs|test|build|ci|chore|style|revert'
pattern="^(${types})(\([A-Za-z0-9][A-Za-z0-9._/-]*\))?!?: [^[:space:]]"
revert='^Revert ".+"$'

bad=0
check() {
	if [[ $1 =~ $pattern || $1 =~ $revert ]]; then
		echo "ok:  $1"
	else
		echo "::error::not a Conventional Commit subject: $1"
		bad=1
	fi
}

usage() {
	echo "usage: $0 title <subject>... | range <base>..<head>" >&2
	exit 2
}

[ $# -ge 2 ] || usage
mode=$1
shift
case $mode in
title)
	for s in "$@"; do check "$s"; done
	;;
range)
	[ $# -eq 1 ] || usage
	while IFS= read -r s; do
		check "$s"
	done < <(git log --no-merges --format=%s "$1")
	;;
*)
	usage
	;;
esac

if [ "$bad" -ne 0 ]; then
	cat >&2 <<'EOF'

Use <type>(<scope>): <summary>, e.g. `fix(template): bool filter warns once`.
Types: feat fix perf refactor docs test build ci chore style revert; `!`
after the type/scope (or a `BREAKING CHANGE:` footer) marks a breaking
change. See CONTRIBUTING.md.
EOF
	exit 1
fi
