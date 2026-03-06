#!/bin/bash
# Merge GitHub PRs into current branch
#
# Usage: Edit the invocations below, then run this script
#
# merge_pr <owner/repo> <pr_number> [title]
#
# Each PR is merged with a --no-ff merge commit for clean history.
# If any merge has conflicts, the branch is reset to its original state.

set -e

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
echo ""

# ============================================================================
# Edit this section to add/remove PRs to merge
# ============================================================================

# merge_pr <owner/repo> <pr_number> [title]
merge_pr containerd/containerd 12608 "Update plugin config migration to run on load"
merge_pr containerd/containerd 12562 "Add plugins for server listeners"
merge_pr containerd/containerd 12667 "Update transfer service to support automatically garbage collecting extra references"
merge_pr containerd/containerd 12785 "Make shim socket directory use configured directory"
merge_pr containerd/containerd 12865 "Read only mounts on Darwin, support resolving uid/gid"
merge_pr dmcgowan/containerd 11 "This needs a PR on containerd/containerd still"
merge_pr containerd/containerd 12921 "sys, dialer: support AF_UNIX sockets on Windows"
merge_pr containerd/containerd 12938 "set sparse file attribute on Windows"
merge_pr containerd/containerd 12949 "Add junction support for Windows"
merge_pr containerd/containerd 12968 "Fix send stream EOF"


