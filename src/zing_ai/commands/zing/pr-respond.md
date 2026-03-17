
<objective>
Address all GitHub PR feedback — unresolved review threads, top-level review summaries, and issue comments — fix valid issues, commit and push changes, reply to comments on GitHub (resolving threads where applicable), then check CI status and offer to re-check or exit.
</objective>

<process>

<step name="resolve_pr">
Determine which PR to work on:

1. If the user provided a full GitHub PR URL (e.g., `https://github.com/owner/repo/pull/123`), extract the PR number.
2. If the user provided a PR number (e.g., `123`, `#123`), use that directly.
3. If neither was provided:
   Before asking the user, send a browser notification so they know input is needed:
   Call `notification_send(session_id, title="Input needed", body="Provide the PR number to respond to.")` where `session_id` is the session ID from the zing file frontmatter.
   Use AskUserQuestion to ask the user to provide a PR link or number. Do not proceed until one is given.

Once you have the PR number, run:
```
gh pr view {number} --json number,headRefName,baseRefName,title,url,body
```

Store the PR number, head branch, base branch, title, and URL for later use.

Get the `{owner}/{repo}` from:
```
gh repo view --json nameWithOwner --jq '.nameWithOwner'
```

### Create phase tasks

After resolving the PR, create top-level phase tasks to track progress:

```
TaskCreate: "Merge latest base branch" (description: "Merge latest {baseRefName} into the PR branch and resolve conflicts")
TaskCreate: "Fetch unresolved comments" (description: "Query GitHub for unresolved review threads, review summaries, and issue comments on PR #{number}")
TaskCreate: "Address review comments" (description: "Analyze and fix each unresolved comment")
TaskCreate: "Commit & push" (description: "Stage, commit, and push code changes")
TaskCreate: "Reply & resolve on GitHub" (description: "Post replies and resolve threads via GitHub API")
TaskCreate: "Check CI" (description: "Verify all CI checks pass")
TaskCreate: "Re-request reviews" (description: "Re-request reviews from stale reviewers")
```

All tasks start as pending. Each subsequent step marks its corresponding phase task as `in_progress` when starting and `completed` when done.
</step>

<step name="checkout_pr">
Check out the PR branch locally:

```
gh pr checkout {number}
```

If this fails (e.g., due to uncommitted changes), tell the user and exit.
</step>

<step name="merge_base">
Mark the "Merge latest base branch" phase task as `in_progress` using TaskUpdate.

Merge the latest base branch into the PR branch to ensure it's up to date:

```
git fetch origin {baseRefName}
git merge origin/{baseRefName}
```

**If the merge completes cleanly**, tell the user:
```
Merged latest {baseRefName} — no conflicts.
```

**If there are merge conflicts**, resolve them:
1. Run `git diff --name-only --diff-filter=U` to list conflicted files.
2. For each conflicted file, read it to understand the conflict markers.
3. Resolve each conflict by analyzing both sides and choosing the correct resolution. If the intent is ambiguous:
   Before asking the user, send a browser notification so they know input is needed:
   Call `notification_send(session_id, title="Merge conflict", body="An ambiguous merge conflict needs manual resolution.")` where `session_id` is the session ID from the zing file frontmatter.
   Use AskUserQuestion to let the user decide.
4. After resolving all conflicts, stage the resolved files (specific files only, NEVER `git add -A` or `git add .`) and complete the merge:
   ```
   git commit --no-edit
   ```
5. Tell the user which files had conflicts and how they were resolved.

**If the base branch is already up to date** (merge says "Already up to date."), note this and move on.

After the merge is complete (or was already up to date), push the updated branch:
```
git push
```

Mark the "Merge latest base branch" phase task as `completed` using TaskUpdate.
</step>

<step name="fetch_unresolved_comments">
Mark the "Fetch unresolved comments" phase task as `in_progress` using TaskUpdate.

Fetch all review threads and their resolution status using the GraphQL API:

```
gh api graphql -f query='
  query {
    repository(owner: "{owner}", name: "{repo}") {
      pullRequest(number: {number}) {
        reviewThreads(first: 100) {
          nodes {
            id
            isResolved
            line
            path
            comments(first: 50) {
              nodes {
                databaseId
                body
                author {
                  login
                }
              }
            }
          }
        }
      }
    }
  }
'
```

From the returned list, identify **unresolved** threads — those where `isResolved` is `false`. For each unresolved thread, note:
- `id` — the thread's GraphQL node ID (needed later for resolving)
- `path` — the file the comment is on
- `line` — the line number
- `comments.nodes[0].databaseId` — the top-level comment's REST API ID (needed for replying)
- `comments.nodes[0].body` — the original comment text
- `comments.nodes[0].author.login` — who wrote it
- All subsequent `comments.nodes` entries — replies in the thread

Only consider threads where the issue has NOT already been addressed (look at the thread replies — if someone already pushed a fix or the original reviewer said "resolved", skip it).

### Fetch top-level review bodies

Fetch reviews that contain a summary body (the text submitted alongside a review, not inline comments):

```
gh api repos/{owner}/{repo}/pulls/{number}/reviews --paginate
```

Filter to reviews where:
- `body` is non-empty (after trimming whitespace)
- `state` is not `DISMISSED`
- `user.login` is not the PR author
- `user.login` does not end in `[bot]`

For each matching review, note:
- `id` — the review's REST API ID
- `body` — the review summary text
- `user.login` — who wrote it
- `state` — the review state (e.g., `CHANGES_REQUESTED`, `COMMENTED`)
- `html_url` — link to the review on GitHub

**Detecting "already addressed":** Check if a later review or issue comment by the PR author exists that references or responds to this review. If uncertain, include it — the user can skip.

### Fetch issue comments

Fetch general PR conversation comments (not inline code comments):

```
gh api repos/{owner}/{repo}/issues/{number}/comments --paginate
```

Filter to comments where:
- `user.login` is not the PR author
- `user.login` does not end in `[bot]`

For each matching comment, note:
- `id` — the comment's REST API ID
- `body` — the comment text
- `user.login` — who wrote it
- `created_at` — when it was posted
- `html_url` — link to the comment on GitHub

**Detecting "already addressed":** Check if a later issue comment by the PR author exists that appears to respond to this comment. If uncertain, include it — the user can skip.

### Finish fetch step

Mark the "Fetch unresolved comments" phase task as `completed` using TaskUpdate.

If all three lists are empty (no unresolved review threads, no review summaries, and no issue comments), tell the user:
```
No unresolved comments found on PR #{number}.
```
Mark the "Address review comments", "Commit & push", and "Reply & resolve on GitHub" phase tasks as `completed` (nothing to do). Then skip directly to the **check_ci** step.
</step>

<step name="present_comments">
Present all comments to the user, grouped by type, as a single numbered list with continuous numbering:

```
Found {total_count} unresolved comment(s) on PR #{number}:

Review threads ({thread_count}):
1. [{file_path}:{line}] @{reviewer}: "{comment_body_truncated}"
2. [{file_path}:{line}] @{reviewer}: "{comment_body_truncated}"

Review summaries ({summary_count}):
3. [Review: {state}] @{reviewer}: "{comment_body_truncated}"

General comments ({issue_comment_count}):
4. [Comment] @{author}: "{comment_body_truncated}"
...
```

Omit any group header if that group has zero items. Truncate long comment bodies to ~100 characters with "..." for the summary view.

### Create comment-level tasks

Create one task per comment to track individual progress, with type-appropriate labels:

```
TaskCreate: "[{file_path}:{line}] @{reviewer}: '{comment_body_truncated}'"          # review threads
TaskCreate: "[Review: {state}] @{reviewer}: '{comment_body_truncated}'"              # review summaries
TaskCreate: "[Comment] @{author}: '{comment_body_truncated}'"                        # issue comments
```

All comment tasks start as pending.

Then say: "Will address each comment one at a time."
</step>

<step name="address_comments">
Mark the "Address review comments" phase task as `in_progress` using TaskUpdate.

For each comment, in order — marking the corresponding comment task as `in_progress` when starting it:

1. **Show the comment** in full, with a type-appropriate header:

   For **review threads**:
   ```
   --- Comment {N}/{total} [Review Thread] ---
   File: {file_path}:{line}
   Reviewer: @{reviewer}
   Comment: {full_comment_body}
   ```

   For **review summaries**:
   ```
   --- Comment {N}/{total} [Review Summary: {state}] ---
   Reviewer: @{reviewer}
   Comment: {full_comment_body}
   ```

   For **issue comments**:
   ```
   --- Comment {N}/{total} [General Comment] ---
   Author: @{author}
   Comment: {full_comment_body}
   ```

2. **Read the relevant code:**
   - **Review threads:** Read at least 20 lines around the commented line in the specified file, plus any related code the comment references.
   - **Review summaries and issue comments:** Parse the comment body for file references, code blocks, or function/class names. Read whatever files or symbols the comment references. If the comment is general (e.g., architectural feedback), read the most relevant files.

3. **Analyze the comment.** Determine whether it is:
   - A **valid finding** that requires a code change
   - A **question** that needs an answer
   - A **style/nit** suggestion
   - A **misunderstanding** that should be clarified
   - **Not actionable** (e.g., an observation, praise, or already addressed)

4. **If the comment is not actionable** (already addressed, praise, informational), tell the user:
   ```
   This comment appears to be {reason}. Skipping — will reply acknowledging it.
   ```
   Record that a reply should be posted but no code change is needed. Move to the next comment.

5. **If there is exactly one clear fix**, propose it to the user:
   ```
   Proposed fix: {brief description of the change}
   ```
   Then make the code change.

6. **If there are multiple valid approaches**:
   Before asking the user, send a browser notification so they know input is needed:
   Call `notification_send(session_id, title="Input needed", body="A PR comment needs your decision on how to address it.")` where `session_id` is the session ID from the zing file frontmatter.
   Present them as a menu using AskUserQuestion:
   - Question: "How should this be addressed?"
   - Options: List each approach with a short label and description
   - Include an "Other" option so the user can describe their own approach

   Wait for the user's selection, then implement the chosen approach.

7. **If the comment is a question or misunderstanding**, draft a reply and show it to the user for approval before recording it.

After addressing each comment, mark its comment task as `completed` using TaskUpdate, and record:
- The comment ID
- The comment type: `review_thread`, `review_body`, or `issue_comment`
- What was done (code change, reply only, skipped)
- The reply text to post on GitHub

After all comments are addressed, mark the "Address review comments" phase task as `completed`.
</step>

<step name="commit_and_push">
Mark the "Commit & push" phase task as `in_progress` using TaskUpdate.

After all comments have been addressed:

1. Run `git status` to see what changed.
2. If there are changes:
   - Stage the specific changed files (NEVER use `git add -A` or `git add .`)
   - Commit with a message like: `Address PR review comments on #{number}` — always include `Co-Authored-By: Zing <zing@farmerpete.net>` in the commit message
   - Push to the remote branch:
     ```
     git push
     ```
3. If there are no code changes (all comments were replied to without code changes), skip the commit/push and note this to the user.

Mark the "Commit & push" phase task as `completed` using TaskUpdate.
</step>

<step name="reply_and_resolve">
Mark the "Reply & resolve on GitHub" phase task as `in_progress` using TaskUpdate.
For each comment that was addressed, post a reply on GitHub using the appropriate method for the comment type.

The reply text should be concise and professional. For code fixes, mention what was changed (e.g., "Fixed — added null check as suggested." or "Updated to use `const` instead of `let`."). For questions, provide the answer. For non-actionable comments, acknowledge them (e.g., "Thanks for the feedback!" or "Acknowledged.").

### For `review_thread` comments:

**Reply to the comment:**
```
gh api repos/{owner}/{repo}/pulls/{number}/comments \
  -X POST \
  -f body='{reply_text}' \
  -F in_reply_to={comment_id}
```

**Resolve the thread** using the GraphQL API:
```
gh api graphql -f query='
  mutation {
    resolveReviewThread(input: {threadId: "{thread_node_id}"}) {
      thread {
        isResolved
      }
    }
  }
'
```

Use the thread's GraphQL node ID (`id`) that was already fetched in the **fetch_unresolved_comments** step. Each thread's `id` was stored alongside its comment data — use it directly in the `resolveReviewThread` mutation.

### For `review_body` comments:

Post an issue comment that references the original review:
```
gh api repos/{owner}/{repo}/issues/{number}/comments \
  -X POST \
  -f body='Re: @{reviewer}'\''s [review]({html_url})

{reply_text}'
```

There is no resolve step — review bodies do not have a resolution concept.

### For `issue_comment` comments:

Post an issue comment that mentions the original author:
```
gh api repos/{owner}/{repo}/issues/{number}/comments \
  -X POST \
  -f body='@{author} {reply_text}'
```

There is no resolve step — issue comments do not have a resolution concept.

### Finish reply step

After all replies (and resolutions, where applicable) are done, mark the "Reply & resolve on GitHub" phase task as `completed` using TaskUpdate.

Tell the user:
```
Replied to {count} comment(s) on PR #{number}. Resolved {resolved_count} review thread(s).
```
</step>

<step name="check_ci">
Mark the "Check CI" phase task as `in_progress` using TaskUpdate.
Check the CI status for the PR:

```
gh pr checks {number}
```

Parse the output to categorize each check as:
- **passed** — completed successfully
- **failed** — completed with failure
- **pending** — still running or queued

### Create CI-level tasks

Create one task per CI check:

```
TaskCreate: "CI: {check_name}" (description: "{status}")
```

- Passed checks → immediately mark as `completed` using TaskUpdate
- Failed checks → leave as pending (will be marked `in_progress` when fixing)
- Pending checks → leave as pending

**If all checks passed:**
```
All CI checks passed on PR #{number}. You're good to go!
```
Mark the "Check CI" phase task as `completed` using TaskUpdate. Proceed to the **re_request_reviews** step.

**If any checks failed:**
Show the status of all checks as a table:
```
CI Status for PR #{number}:

| Status | Check | Details |
|--------|-------|---------|
| ✅ | {check_name} | passed |
| ❌ | {check_name} | failed |
| ⏳ | {check_name} | pending |
```

For each failed check, mark its CI task as `in_progress` using TaskUpdate, then run:
```
gh run view {run_id} --log-failed
```
to get the failure logs. Analyze the failures and attempt to fix them. After fixing, mark the CI task as `completed`, commit and push the changes (same process as the commit_and_push step, with message like `Fix CI failures on #{number}`), then loop back to check CI again.

**If any checks are still pending (and none have failed):**

Poll with increasing backoff until all non-ignored checks complete. The backoff schedule is: 30s, 60s, 90s, 120s, then 120s for all subsequent attempts.

**Ignored checks:** Skip checks matching "chromatic" (case-insensitive) — do not wait for them or include them in the pending count. They should still appear in the status table but should not block completion.

On each poll iteration:
1. Wait for the current backoff interval.
2. Re-run `gh pr checks {number}`.
3. Show the status table with ALL checks (including ignored ones, marked with ⏭️ instead of ⏳):
   ```
   CI Status for PR #{number} (attempt {N}, next check in {backoff}s):

   | Status | Check | Details |
   |--------|-------|---------|
   | ✅ | {check_name} | passed |
   | ❌ | {check_name} | failed |
   | ⏳ | {check_name} | pending |
   | ⏭️ | {check_name} | skipped (ignored) |
   ```
4. Update CI-level tasks: mark newly passed checks as `completed`.
5. If all non-ignored checks have completed (passed or failed), exit the poll loop.
6. If any checks failed, stop polling and handle failures (see "If any checks failed" above).
7. If non-ignored checks are still pending, continue to the next iteration with increased backoff.

After all non-ignored checks pass, mark the "Check CI" phase task as `completed` and proceed to the **re_request_reviews** step.

After handling failures (fix, commit, push), go back to the **fetch_unresolved_comments** step and re-run the entire cycle from there: fetch any new unresolved comments, address them, commit and push if needed, reply and resolve, then check CI again. On re-entry, create fresh comment-level and CI-level tasks for any newly discovered items. Phase-level tasks are re-marked through the `in_progress` → `completed` lifecycle again.
</step>

<step name="re_request_reviews">
Mark the "Re-request reviews" phase task as `in_progress` using TaskUpdate.

Re-request reviews from anyone who has a stale review (i.e., they submitted a review but new commits have been pushed since).

Fetch the list of reviewers with stale reviews:
```
gh api repos/{owner}/{repo}/pulls/{number}/reviews --paginate
```

From the returned list, collect the `user.login` of each reviewer whose review `state` is `CHANGES_REQUESTED` or `COMMENTED`. Deduplicate the list (a reviewer may have submitted multiple reviews).

Re-request review from each of them:
```
gh api repos/{owner}/{repo}/pulls/{number}/requested_reviewers \
  -X POST \
  -f 'reviewers[]={login1}' \
  -f 'reviewers[]={login2}'
```

Tell the user:
```
Re-requested reviews from: @{login1}, @{login2}
```

If there are no stale reviewers, say:
```
No stale reviews to re-request.
```

Mark the "Re-request reviews" phase task as `completed` using TaskUpdate.
</step>

</process>

<anti_patterns>
- NEVER use `git add -A` or `git add .` — stage specific files only
- NEVER push without committing first
- NEVER resolve a comment thread without first replying to it
- NEVER make a code change without showing the user what will be done
- NEVER skip a comment without telling the user why
- NEVER guess at what a reviewer meant — if a comment is ambiguous, ask the user
- NEVER reply to comments with AI-generated fluff — keep replies concise and specific
- NEVER combine unrelated fixes into one commit — if addressing comments requires changes across different concerns, make separate commits
- NEVER omit `Co-Authored-By: Zing <zing@farmerpete.net>` from commit messages
- NEVER include bot comments (users whose login ends in `[bot]`) — always filter them out
- NEVER reply to the same comment twice — before posting a reply to a review body or issue comment, check if a reply has already been posted in the current run
</anti_patterns>

<success_criteria>
The skill is complete when:

- [ ] PR was identified (from argument, URL, or user input)
- [ ] PR branch was checked out locally
- [ ] All unresolved review threads were fetched and presented
- [ ] All top-level review bodies (summaries) were fetched and presented
- [ ] All issue comments were fetched and presented
- [ ] Bot comments were filtered out from all three sources
- [ ] Each comment was analyzed and addressed (fixed, answered, or acknowledged)
- [ ] When multiple fix approaches existed, the user was given a choice
- [ ] Code changes were committed and pushed
- [ ] Replies were posted to all addressed comments on GitHub (using the correct API per type)
- [ ] Review thread comments were resolved via the GraphQL API
- [ ] CI status was checked and reported
- [ ] Failed CI checks were investigated and fixed (if any)
- [ ] Pending CI checks were polled with backoff until completion
- [ ] Re-requested reviews from reviewers with stale reviews
</success_criteria>
