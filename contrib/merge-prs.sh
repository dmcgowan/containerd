#!/bin/bash
# Merge GitHub PRs into current branch
#
# Usage: contrib/merge-prs.sh [pr-list-file]
#
# Reads a PR list file (default: contrib/pr-list) where each line contains:
#   <owner/repo> <pr_number> [title]
#
# Blank lines and lines starting with # are ignored.
#
# Each PR is merged with a --no-ff merge commit for clean history.
# If any merge has conflicts, the branch is reset to its original state.

set -e

PR_LIST="${1:-contrib/pr-list}"

if [ ! -f "$PR_LIST" ]; then
    echo "Error: PR list file not found: $PR_LIST"
    exit 1
fi

ORIGINAL_HEAD=$(git rev-parse HEAD)

cleanup_on_failure() {
    echo ""
    echo "Resetting branch to original commit..."
    git merge --abort 2>/dev/null || true
    git reset --hard "$ORIGINAL_HEAD"
    echo "Branch reset to $(git log -1 --oneline "$ORIGINAL_HEAD")"
    # Clean up any leftover pr- branches
    git for-each-ref --format='%(refname:short)' 'refs/heads/pr-*' | xargs -r git branch -D 2>/dev/null || true
    exit 1
}

merge_pr() {
    local repo=$1
    local pr=$2
    local title=$3
    local pr_branch="pr-${repo//\//-}-$pr"

    # Build merge commit message
    local msg="Merge $repo#$pr"
    if [ -n "$title" ]; then
        msg="$msg: $title"
    fi

    echo "=================================================="
    echo "$msg"
    echo "=================================================="

    # Fetch the PR
    echo "Fetching PR #$pr from $repo..."
    if ! git fetch "git@github.com:$repo.git" "pull/$pr/head:$pr_branch"; then
        echo "Error: Failed to fetch PR #$pr from $repo"
        cleanup_on_failure
    fi

    # Show commits for reference
    local merge_base
    merge_base=$(git merge-base HEAD "$pr_branch")
    local total
    total=$(git rev-list --count "$merge_base..$pr_branch")

    echo "Found $total commit(s) in PR"
    echo ""
    git --no-pager log --oneline "$merge_base..$pr_branch"
    echo ""

    # Attempt merge
    if ! git merge --no-ff -m "$msg" "$pr_branch"; then
        echo ""
        echo "Error: Merge failed for $repo#$pr"
        git branch -D "$pr_branch" 2>/dev/null || true
        cleanup_on_failure
    fi

    echo "✓ Successfully merged $repo#$pr"
    echo ""

    # Cleanup
    git branch -D "$pr_branch" 2>/dev/null || true
}

echo "Current branch: $(git branch --show-current)"
echo "PR list: $PR_LIST"
echo ""

while IFS= read -r line || [ -n "$line" ]; do
    # Skip blank lines and comments
    line="${line%%#*}"
    line="$(echo "$line" | xargs)"
    [ -z "$line" ] && continue

    # Parse: repo pr [title]
    # Title may be quoted, so use a simple split: first two fields are repo and pr,
    # the rest is the title.
    repo=$(echo "$line" | awk '{print $1}')
    pr=$(echo "$line" | awk '{print $2}')
    title=$(echo "$line" | sed 's/^[^ ]* *[^ ]* *//')
    [ "$title" = "$pr" ] && title=""

    merge_pr "$repo" "$pr" "$title"
done < "$PR_LIST"
