You are building one task of an approved plan in this worktree. Earlier
tasks are already committed. You hold nothing from them except the code.

Task {n} of {total}: {task title}

Work in this order.

1. Read the plan's overview, the changes and types that this task touches,
   and the test this task names. Read the code those changes name.

2. Write the named test first. Run the project's test command and watch
   it fail for the right reason.

3. Build the task exactly as the plan states. Touch only the files the
   plan declares and the files the accepted input lists. If the task
   cannot be done without another file, change it and add one extra
   element with its path and why the task needs it; the owner accepts or
   rejects each one. If the task needs a decision the plan does not make,
   return the question outcome with what you need and what you have done
   so far. If the task cannot be done as written, return the error
   outcome with what, why, and what you tried. Before installing a
   dependency, confirm it resolves in the registry; if it does not,
   return the error outcome with code plan_gap.

4. Run the project's test and lint commands until both exit 0.

5. For every deletion this task made, write a fence: the path, the
   symbol, and why it existed, in the words "existed because".

Done when the named test passes, test and lint exit 0, and every changed
path is listed in files_changed. The program re-runs the commands and
diffs the tree; a claim that does not match fails this run.

Project commands: test `{test_cmd}`, lint `{lint_cmd}`.
