#!/bin/sh
# Release notes for one version, from the commits in a range.
#
# Usage: scripts/release-notes.sh <previous-tag|""> <new-tag>
#
# Prints Markdown on stdout. The release workflow runs exactly this, so a local
# run shows the notes before anything is tagged:
#
#   scripts/release-notes.sh "$(git tag --list 'v*' --sort=-v:refname | head -1)" v0.2.0
#
# A commit that does not follow the convention is listed under "Other" rather
# than dropped, so nothing disappears without being seen.
set -eu

previous="${1:-}"
tag="${2:?usage: release-notes.sh <previous-tag> <new-tag>}"
repo="${GITHUB_REPOSITORY:-devidevio/gitone}"

if [ -n "$previous" ]; then
	range="${previous}..HEAD"
else
	range=HEAD
fi

# US is the unit separator: a subject can contain anything else.
git log --no-merges --reverse --format='%H%x1f%s' "$range" |
	awk -F'\037' -v tag="$tag" -v previous="$previous" -v repo="$repo" '
	{
		sha = substr($1, 1, 7)
		subject = $2
		type = "other"; scope = ""; rest = subject; breaking = 0

		if (match(subject, /^[a-z]+(\([^)]+\))?!?: /)) {
			head = substr(subject, 1, RLENGTH - 2)
			rest = substr(subject, RLENGTH + 1)
			if (head ~ /!$/) { breaking = 1; sub(/!$/, "", head) }
			if (match(head, /\(/)) {
				type  = substr(head, 1, RSTART - 1)
				scope = substr(head, RSTART + 1, length(head) - RSTART - 1)
			} else {
				type = head
			}
		}

		line = "- "
		if (scope != "") line = line "**" scope ":** "
		line = line rest " (`" sha "`)\n"

		if (breaking) breaks = breaks line
		else          body[type] = body[type] line
	}
	END {
		printf "## %s\n\n", tag

		if (breaks != "") printf "### Breaking changes\n\n%s\n", breaks

		# Order of the sections, and the heading each type gets. chore, ci,
		# style and test are absent on purpose: they are not news.
		types = split("feat fix perf refactor docs build other", key, " ")
		split("Features|Bug fixes|Performance|Refactoring|Documentation|Build|Other", name, "|")

		printed = 0
		for (i = 1; i <= types; i++) {
			if (body[key[i]] == "") continue
			printf "### %s\n\n%s\n", name[i], body[key[i]]
			printed = 1
		}
		if (!printed && breaks == "") print "No notable changes.\n"

		if (previous != "")
			printf "**Full changelog**: https://github.com/%s/compare/%s...%s\n", repo, previous, tag
	}
	'
