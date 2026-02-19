#!/bin/bash
# Merge GitHub PRs or branches into current branch
#
# Usage: contrib/merge-prs.sh [pr-list-file]
#
# Reads a PR list file (default: contrib/pr-list) where each line contains:
#   <owner/repo> <pr_number_or_branch> [title]
#
# If the second field is a numeric PR number, the PR is fetched from GitHub
# and its base branch is determined from PR metadata.
# If the second field is a branch name (non-numeric), the branch is fetched
# directly from the repo and always rebased onto containerd/containerd:main.
#
# Blank lines and lines starting with # are ignored.
#
# Each PR/branch is merged with a --no-ff merge commit for clean history.
# If any merge has conflicts, the branch is reset to its original state.

set -e

PR_LIST="${1:-contrib/pr-list}"

if [ ! -f "$PR_LIST" ]; then
    echo "Error: PR list file not found: $PR_LIST"
    exit 1
fi

ORIGINAL_HEAD=$(git rev-parse HEAD)
ORIGINAL_BRANCH=$(git branch --show-current)

cleanup_on_failure() {
    echo ""
    echo "Resetting branch to original commit..."
    git rebase --abort 2>/dev/null || true
    git merge --abort 2>/dev/null || true
    git checkout "$ORIGINAL_BRANCH" 2>/dev/null || true
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

    # Get the base branch name from PR metadata. We use this to compute the
    # fork point locally rather than relying on GitHub's commit list, which
    # can lag after a fresh push to the PR or base branch.
    echo "Querying PR #$pr metadata from $repo..."
    local base_ref
    if ! base_ref=$(gh pr view "$pr" --repo "$repo" --json baseRefName -q '.baseRefName'); then
        echo "Error: Failed to query PR #$pr from $repo"
        cleanup_on_failure
    fi
    if [ -z "$base_ref" ]; then
        echo "Error: PR #$pr from $repo has no base branch"
        cleanup_on_failure
    fi

    local upstream="git@github.com:$repo.git"

    # Fetch the PR head.
    echo "Fetching PR #$pr from $repo..."
    if ! git fetch "$upstream" "pull/$pr/head:$pr_branch"; then
        echo "Error: Failed to fetch PR #$pr from $repo"
        cleanup_on_failure
    fi

    # Fetch the base branch fresh from upstream. GitHub's PR commit list can
    # lag after a push, but a direct fetch always reflects the current state.
    echo "Fetching base branch ($base_ref) from $repo..."
    if ! git fetch "$upstream" "$base_ref"; then
        echo "Error: Failed to fetch base branch $base_ref from $repo"
        git branch -D "$pr_branch" 2>/dev/null || true
        cleanup_on_failure
    fi
    local base_tip
    base_tip=$(git rev-parse FETCH_HEAD)

    # Compute the fork point and commit count locally using the fresh base.
    # We use merge-base against the upstream base branch (not our local HEAD,
    # which may already contain other accumulated PR merges).
    local pr_base total
    if ! pr_base=$(git merge-base "$base_tip" "$pr_branch"); then
        echo "Error: Cannot compute fork point between $base_ref and PR #$pr"
        git branch -D "$pr_branch" 2>/dev/null || true
        cleanup_on_failure
    fi
    total=$(git rev-list --count "$pr_base..$pr_branch")

    local current_head
    current_head=$(git rev-parse HEAD)
    echo "Rebasing $total PR commit(s) ($pr_base..$pr_branch) onto $current_head..."
    git stash -q 2>/dev/null || true
    if ! git rebase --onto "$current_head" "$pr_base" "$pr_branch"; then
        echo ""
        echo "Error: Rebase failed for $repo#$pr"
        git rebase --abort 2>/dev/null || true
        git checkout "$ORIGINAL_BRANCH" 2>/dev/null || true
        git stash pop -q 2>/dev/null || true
        git branch -D "$pr_branch" 2>/dev/null || true
        cleanup_on_failure
    fi
    # Return to original branch after rebase (rebase checks out pr_branch)
    git checkout "$ORIGINAL_BRANCH"
    git stash pop -q 2>/dev/null || true

    # Show commits for reference
    echo "Rebased $total commit(s) in PR:"
    echo ""
    git --no-pager log --oneline "$current_head..$pr_branch"
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

merge_branch() {
    local repo=$1
    local branch=$2
    local title=$3
    local local_branch="pr-${repo//\//-}-${branch//\//-}"
    local base_repo="containerd/containerd"
    local base_ref="main"

    # Build merge commit message
    local msg="Merge $repo:$branch"
    if [ -n "$title" ]; then
        msg="$msg: $title"
    fi

    echo "=================================================="
    echo "$msg"
    echo "=================================================="

    local upstream="git@github.com:$repo.git"
    local base_upstream="git@github.com:$base_repo.git"

    # Fetch the branch directly from the repo.
    echo "Fetching branch $branch from $repo..."
    if ! git fetch "$upstream" "refs/heads/$branch:$local_branch"; then
        echo "Error: Failed to fetch branch $branch from $repo"
        cleanup_on_failure
    fi

    # Always use containerd/containerd:main as the base for direct branches.
    echo "Fetching base branch ($base_ref) from $base_repo..."
    if ! git fetch "$base_upstream" "$base_ref"; then
        echo "Error: Failed to fetch base branch $base_ref from $base_repo"
        git branch -D "$local_branch" 2>/dev/null || true
        cleanup_on_failure
    fi
    local base_tip
    base_tip=$(git rev-parse FETCH_HEAD)

    # Compute the fork point and commit count using the fresh base.
    local pr_base total
    if ! pr_base=$(git merge-base "$base_tip" "$local_branch"); then
        echo "Error: Cannot compute fork point between $base_repo/$base_ref and $repo:$branch"
        git branch -D "$local_branch" 2>/dev/null || true
        cleanup_on_failure
    fi
    total=$(git rev-list --count "$pr_base..$local_branch")

    local current_head
    current_head=$(git rev-parse HEAD)
    echo "Rebasing $total commit(s) ($pr_base..$local_branch) onto $current_head..."
    git stash -q 2>/dev/null || true
    if ! git rebase --onto "$current_head" "$pr_base" "$local_branch"; then
        echo ""
        echo "Error: Rebase failed for $repo:$branch"
        git rebase --abort 2>/dev/null || true
        git checkout "$ORIGINAL_BRANCH" 2>/dev/null || true
        git stash pop -q 2>/dev/null || true
        git branch -D "$local_branch" 2>/dev/null || true
        cleanup_on_failure
    fi
    # Return to original branch after rebase (rebase checks out local_branch)
    git checkout "$ORIGINAL_BRANCH"
    git stash pop -q 2>/dev/null || true

    # Show commits for reference
    echo "Rebased $total commit(s) in branch:"
    echo ""
    git --no-pager log --oneline "$current_head..$local_branch"
    echo ""

    # Attempt merge
    if ! git merge --no-ff -m "$msg" "$local_branch"; then
        echo ""
        echo "Error: Merge failed for $repo:$branch"
        git branch -D "$local_branch" 2>/dev/null || true
        cleanup_on_failure
    fi

    echo "✓ Successfully merged $repo:$branch"
    echo ""

    # Cleanup
    git branch -D "$local_branch" 2>/dev/null || true
}

echo "Current branch: $(git branch --show-current)"
echo "PR list: $PR_LIST"
echo ""

while IFS= read -r line || [ -n "$line" ]; do
    # Skip blank lines and comments
    line="${line%%#*}"
    line="$(echo "$line" | xargs)"
    [ -z "$line" ] && continue

    # Parse: repo pr_or_branch [title]
    # Title may contain spaces; first two fields are repo and identifier,
    # the rest is the title.
    repo=$(echo "$line" | awk '{print $1}')
    pr=$(echo "$line" | awk '{print $2}')
    title=$(echo "$line" | sed 's/^[^ ]* *[^ ]* *//')
    [ "$title" = "$pr" ] && title=""

    # Detect whether the identifier is a PR number (purely numeric) or a branch name.
    if [[ "$pr" =~ ^[0-9]+$ ]]; then
        merge_pr "$repo" "$pr" "$title"
    else
        merge_branch "$repo" "$pr" "$title"
    fi
done < "$PR_LIST"
