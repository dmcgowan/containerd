#!/bin/bash
# Cherry-pick GitHub PRs into current branch
#
# Usage: Edit the invocations below, then run this script

set -e

cherry_pick_pr() {
    local repo=$1
    local pr=$2
    local pr_branch="pr-${repo//\//-}-$pr"

    echo "=================================================="
    echo "Cherry-picking $repo#$pr"
    echo "=================================================="

    # Fetch the PR
    echo "Fetching PR #$pr from $repo..."
    if ! git fetch "https://github.com/$repo" "pull/$pr/head:$pr_branch"; then
        echo "Error: Failed to fetch PR #$pr from $repo"
        return 1
    fi

    # Get merge base and commits
    local merge_base=$(git merge-base HEAD "$pr_branch")
    local commits=$(git rev-list --reverse "$merge_base..$pr_branch")
    local count=$(echo "$commits" | wc -l)

    echo "Found $count commit(s) to cherry-pick"
    echo ""
    git --no-pager log --oneline "$merge_base..$pr_branch"
    echo ""

    # Cherry-pick each commit
    for commit in $commits; do
        echo "Cherry-picking: $(git log -1 --oneline $commit)"
        if ! git cherry-pick --empty=drop "$commit"; then
            echo ""
            echo "Error: Cherry-pick failed!"
            echo "Resolve conflicts, then run:"
            echo "  git cherry-pick --continue  # or --skip or --abort"
            git branch -D "$pr_branch" 2>/dev/null || true
            exit 1
        fi
    done

    echo "✓ Successfully cherry-picked $repo#$pr"
    echo ""

    # Cleanup
    git branch -D "$pr_branch" 2>/dev/null || true
}

echo "Current branch: $(git branch --show-current)"
echo ""

# ============================================================================
# Edit this section to add/remove PRs to cherry-pick
# ============================================================================

# cherry_pick_pr <owner/repo> <pr_number>
cherry_pick_pr containerd/containerd 12608
cherry_pick_pr containerd/containerd 12562 # This also contains a commit from 12608, but that's fine since it will be skipped
cherry_pick_pr containerd/containerd 12667
cherry_pick_pr containerd/containerd 12785
cherry_pick_pr containerd/containerd 12865
cherry_pick_pr dmcgowan/containerd 11 # This needs a PR on containerd/containerd still
cherry_pick_pr containerd/containerd 12921 # sys, dialer: support AF_UNIX sockets on Windows


