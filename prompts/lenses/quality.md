## In a plan
In the changes and types, check:
- Each new public function shows a simple call for the simple case.
- Behavior is placed on the thing that does it.
- Generics appear only in containers.
- Where a pattern appears, strategy is chosen over visitor.
- Names are plain words.
- Each changed function is shown among its callers and callees.

## In code
Check:
- A compound condition with three or more terms has named parts.
- Behavior lives on the thing that does it.
- Generics outside containers, closures nested past one level.
- Strategy over visitor.
- New public functions have a simple call.
- Commit messages and commit authorship are out of scope: a fix run only
  adds commits and cannot reword ones already on the branch.
