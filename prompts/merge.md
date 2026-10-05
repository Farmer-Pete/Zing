# Merge

You are merging the base branch into this ticket's branch. Git has already started the merge in your working directory. The paths under conflicts are unmerged paths; text conflicts usually hold conflict markers, while binary and delete/modify conflicts may not -- inspect their index entries and status before resolving them. Zing stages your work and makes one signed merge commit after you finish. Leave committing, resetting, aborting the merge and pushing to Zing.

Work in this order.

1. See the state. Run `git status` and `git diff`. Read every conflicting file in full. Read `git log --oneline -20 HEAD` and `git log --oneline -20 MERGE_HEAD`.
2. Find why each side changed. The ticket and plan say why this branch changed. base_log holds full messages for at most 200 commits the base side brings in; their messages may name pull requests. For a commit not listed there, inspect file-specific history instead (for example, `git log --` on the path, on both HEAD and MERGE_HEAD). Before you edit a conflict, name the commit on each side that wrote it, when you can find it.
3. Resolve every hunk. Keep both intents where both can hold. Where they cannot, keep the side that serves this ticket's goal and say in your report what the other side loses. Add no behavior that neither side had. Edit only files the merge touches. Remove every conflict marker.
4. Run `{test_cmd}`, then `{lint_cmd}`. Fix what the merge broke. Zing runs both again after you finish and sends you any failure.

If a hunk needs a decision only the owner can make, return outcome question. Name the hunk, show both sides, and say what each choice costs.

Return outcome ok. List every file you edited in files_changed. In the report, write one line per conflicting file: the file, whose intent it keeps, and any trade-off.
