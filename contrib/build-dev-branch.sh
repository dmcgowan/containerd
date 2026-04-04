#!/bin/bash
#
# Build a dev branch by rebasing and merging milestone PRs onto main.
#
# Usage: contrib/build-dev-branch.sh [--dry-run] [--no-tag]
#
# This script:
#   1. Extracts the current version from version/version.go
#   2. Finds the matching milestone in containerd/containerd
#   3. Fetches all open PRs from that milestone
#   4. Creates a new branch from upstream/main
#   5. For each PR: rebases onto main, then merges onto the branch
#   6. Creates a tag with auto-incrementing dev version
#
# PRs that fail rebase or merge are tracked and reported.

set -euo pipefail

REPO="containerd/containerd"
UPSTREAM_REMOTE="${UPSTREAM_REMOTE:-upstream}"
DRY_RUN=false
NO_TAG=false

for arg in "$@"; do
    case "$arg" in
        --dry-run) DRY_RUN=true ;;
        --no-tag) NO_TAG=true ;;
        *) echo "Unknown argument: $arg"; exit 1 ;;
    esac
done

# --- Step 1: Extract version from version/version.go ---

VERSION_FILE="version/version.go"
if [ ! -f "$VERSION_FILE" ]; then
    echo "Error: $VERSION_FILE not found. Run from repo root."
    exit 1
fi

# Extract version string, e.g. "2.3.0-beta+unknown" -> "2.3.0-beta"
FULL_VERSION=$(grep -P '^\s*Version\s*=' "$VERSION_FILE" | sed 's/.*"\(.*\)".*/\1/' | sed 's/+.*//')
MAJOR_MINOR=$(echo "$FULL_VERSION" | grep -oP '^\d+\.\d+')

echo "Version: $FULL_VERSION"
echo "Major.Minor: $MAJOR_MINOR"

# --- Step 2: Find milestone ---

# Map major.minor to milestone number. Query GitHub for milestones.
MILESTONE_NUMBER=$(gh api "repos/$REPO/milestones" --jq ".[] | select(.title == \"$MAJOR_MINOR\") | .number")

if [ -z "$MILESTONE_NUMBER" ]; then
    echo "Error: No milestone found for $MAJOR_MINOR"
    exit 1
fi

echo "Milestone: $MAJOR_MINOR (#$MILESTONE_NUMBER)"

# --- Step 3: Fetch all open PRs from milestone ---

echo ""
echo "Fetching open PRs from milestone $MAJOR_MINOR..."

# Paginate through all PRs
PR_DATA=$(gh api --paginate "repos/$REPO/issues?milestone=$MILESTONE_NUMBER&state=open&per_page=100" \
    --jq '.[] | select(.pull_request) | "\(.number)\t\(.title)"')

if [ -z "$PR_DATA" ]; then
    echo "No open PRs found in milestone $MAJOR_MINOR"
    exit 0
fi

PR_COUNT=$(echo "$PR_DATA" | wc -l)
echo "Found $PR_COUNT open PR(s) in milestone"
echo ""

if [ "$DRY_RUN" = true ]; then
    echo "PRs that would be processed:"
    echo "$PR_DATA" | while IFS=$'\t' read -r pr_num pr_title; do
        echo "  #$pr_num - $pr_title"
    done
    exit 0
fi

# --- Step 4: Create branch from upstream/main ---

echo "Fetching $UPSTREAM_REMOTE/main..."
git fetch "$UPSTREAM_REMOTE" main

# Determine the pre-release tag (e.g., "beta" from "2.3.0-beta")
PRERELEASE=$(echo "$FULL_VERSION" | sed -n 's/^[0-9]*\.[0-9]*\.[0-9]*-\(.*\)/\1/p')
if [ -n "$PRERELEASE" ]; then
    TAG_BASE="v${FULL_VERSION}.dmcg.dev"
else
    TAG_BASE="v${FULL_VERSION}-dmcg-dev"
fi

# Auto-increment: find the next available number
LAST_NUM=-1
for tag in $(git tag -l "${TAG_BASE}.*" 2>/dev/null); do
    NUM=$(echo "$tag" | grep -oP '\d+$')
    if [ "$NUM" -gt "$LAST_NUM" ] 2>/dev/null; then
        LAST_NUM=$NUM
    fi
done
NEXT_NUM=$((LAST_NUM + 1))
TAG_NAME="${TAG_BASE}.${NEXT_NUM}"

BRANCH_NAME="dev/${TAG_NAME}"

echo "Creating branch: $BRANCH_NAME (from $UPSTREAM_REMOTE/main)"
git branch -f "$BRANCH_NAME" "$UPSTREAM_REMOTE/main"

# --- Step 5: Rebase and merge each PR (in a worktree) ---

# Use a temporary worktree so the user's working tree stays untouched.
WORKTREE_DIR=$(mktemp -d "${TMPDIR:-/tmp}/containerd-dev-build.XXXXXX")

cleanup_worktree() {
    if [ -d "$WORKTREE_DIR" ]; then
        git worktree remove --force "$WORKTREE_DIR" 2>/dev/null || true
    fi
    # Clean up any leftover temp branches
    git for-each-ref --format='%(refname:short)' 'refs/heads/pr-*' 'refs/heads/rebase-*' \
        | xargs -r git branch -D 2>/dev/null || true
}
trap cleanup_worktree EXIT

git worktree add "$WORKTREE_DIR" "$BRANCH_NAME"
echo "Worktree: $WORKTREE_DIR"
echo ""

MERGED_PRS=()
REBASE_FAILED_PRS=()
MERGE_FAILED_PRS=()

while IFS=$'\t' read -r pr_num pr_title; do
    PR_BRANCH="pr-$pr_num"
    echo "=================================================="
    echo "Processing PR #$pr_num: $pr_title"
    echo "=================================================="

    # Fetch PR
    if ! git fetch "$UPSTREAM_REMOTE" "pull/$pr_num/head:$PR_BRANCH" 2>/dev/null; then
        echo "  ✗ Failed to fetch PR #$pr_num"
        REBASE_FAILED_PRS+=("$pr_num|$pr_title|fetch failed")
        continue
    fi

    # Rebase PR onto upstream/main inside the worktree
    echo "  Rebasing PR #$pr_num onto $UPSTREAM_REMOTE/main..."

    REBASE_BRANCH="rebase-$pr_num"
    git branch -f "$REBASE_BRANCH" "$PR_BRANCH"

    if ! git -C "$WORKTREE_DIR" checkout "$REBASE_BRANCH" --quiet 2>/dev/null; then
        echo "  ✗ Failed to checkout rebase branch for PR #$pr_num"
        REBASE_FAILED_PRS+=("$pr_num|$pr_title|checkout failed")
        git branch -D "$PR_BRANCH" "$REBASE_BRANCH" 2>/dev/null || true
        continue
    fi

    if ! git -C "$WORKTREE_DIR" rebase "$UPSTREAM_REMOTE/main" --quiet 2>/dev/null; then
        echo "  ✗ Rebase failed for PR #$pr_num"
        git -C "$WORKTREE_DIR" rebase --abort 2>/dev/null || true
        REBASE_FAILED_PRS+=("$pr_num|$pr_title")
        git -C "$WORKTREE_DIR" checkout "$BRANCH_NAME" --quiet 2>/dev/null || true
        git branch -D "$PR_BRANCH" "$REBASE_BRANCH" 2>/dev/null || true
        echo ""
        continue
    fi

    echo "  ✓ Rebase succeeded"

    # Switch worktree back to dev branch and merge
    git -C "$WORKTREE_DIR" checkout "$BRANCH_NAME" --quiet

    MERGE_MSG="Merge $REPO#$pr_num: $pr_title"
    if ! git -C "$WORKTREE_DIR" merge --no-ff -m "$MERGE_MSG" "$REBASE_BRANCH" 2>/dev/null; then
        echo "  ✗ Merge failed for PR #$pr_num"
        git -C "$WORKTREE_DIR" merge --abort 2>/dev/null || true
        MERGE_FAILED_PRS+=("$pr_num|$pr_title")
        git branch -D "$PR_BRANCH" "$REBASE_BRANCH" 2>/dev/null || true
        echo ""
        continue
    fi

    echo "  ✓ Merged PR #$pr_num"
    MERGED_PRS+=("$pr_num|$pr_title")

    # Cleanup temp branches
    git branch -D "$PR_BRANCH" "$REBASE_BRANCH" 2>/dev/null || true
    echo ""
done <<< "$PR_DATA"

# --- Step 6: Summary and tag ---

echo ""
echo "=================================================="
echo "Summary"
echo "=================================================="
echo ""

TAG_MSG="$TAG_NAME dev build

Merged PRs:"

if [ ${#MERGED_PRS[@]} -gt 0 ]; then
    echo "Merged (${#MERGED_PRS[@]}):"
    for entry in "${MERGED_PRS[@]}"; do
        IFS='|' read -r num title <<< "$entry"
        echo "  ✓ #$num - $title"
        TAG_MSG="$TAG_MSG
  - $REPO#$num: $title"
    done
else
    echo "  (none)"
    TAG_MSG="$TAG_MSG
  (none)"
fi

echo ""

if [ ${#REBASE_FAILED_PRS[@]} -gt 0 ]; then
    echo "Rebase failed (${#REBASE_FAILED_PRS[@]}):"
    TAG_MSG="$TAG_MSG

Rebase failed:"
    for entry in "${REBASE_FAILED_PRS[@]}"; do
        IFS='|' read -r num title <<< "$entry"
        echo "  ✗ #$num - $title"
        TAG_MSG="$TAG_MSG
  - $REPO#$num: $title"
    done
    echo ""
fi

if [ ${#MERGE_FAILED_PRS[@]} -gt 0 ]; then
    echo "Merge failed (${#MERGE_FAILED_PRS[@]}):"
    TAG_MSG="$TAG_MSG

Merge failed:"
    for entry in "${MERGE_FAILED_PRS[@]}"; do
        IFS='|' read -r num title <<< "$entry"
        echo "  ✗ #$num - $title"
        TAG_MSG="$TAG_MSG
  - $REPO#$num: $title"
    done
    echo ""
fi

# Create tag on the dev branch tip
TAG_CREATED=""
if [ "$NO_TAG" = false ] && [ ${#MERGED_PRS[@]} -gt 0 ]; then
    echo "Creating tag: $TAG_NAME"
    git tag -a "$TAG_NAME" "$BRANCH_NAME" -m "$TAG_MSG"
    TAG_CREATED="$TAG_NAME"
    echo "Tag created: $TAG_NAME"
    echo ""
    echo "To push: git push origin $TAG_NAME"
else
    if [ "$NO_TAG" = true ]; then
        echo "Skipping tag creation (--no-tag)"
    elif [ ${#MERGED_PRS[@]} -eq 0 ]; then
        echo "Skipping tag creation (no PRs merged)"
    fi
fi

echo ""
echo "Branch: $BRANCH_NAME"
echo "Tag: ${TAG_CREATED:-"(none)"}"

# Output for CI consumption
if [ -n "${GITHUB_OUTPUT:-}" ]; then
    echo "tag=$TAG_CREATED" >> "$GITHUB_OUTPUT"
    echo "branch=$BRANCH_NAME" >> "$GITHUB_OUTPUT"
fi
