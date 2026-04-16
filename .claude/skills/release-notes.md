---
description: Generate a markdown press release and changelog from changes since the last git tag, then copy to clipboard.
---

<objective>
Look at all commits and changes since the most recent git tag, categorize them into major features and minor/bug fixes, generate a polished markdown press release with a changelog section, and copy the result to the clipboard.
</objective>

<process>

<step name="find_last_tag">
Find the most recent git tag:

```bash
git describe --tags --abbrev=0
```

If no tags exist, tell the user and exit.

Store the tag name and its date:
```bash
git log -1 --format=%ai {tag}
```
</step>

<step name="gather_changes">
Gather all information about changes since the last tag:

1. **Commit log** — get all commits since the tag with their full messages:
   ```bash
   git log {tag}..HEAD --pretty=format:"%h %s%n%b---"
   ```

2. **Diff stat** — get a summary of files changed:
   ```bash
   git diff {tag}..HEAD --stat
   ```

3. **Full diff** — read the actual diff to understand the substance of changes (not just commit messages, which can be vague):
   ```bash
   git diff {tag}..HEAD
   ```

Read the diff output carefully. Commit messages alone are often insufficient — the diff reveals what actually changed.
</step>

<step name="categorize">
Analyze the commits and diff to categorize changes:

**Major features** — new capabilities, significant enhancements, or architectural changes that users would care about. These become the press release highlights.

**Minor improvements / bug fixes** — small enhancements, dependency bumps, refactors, test additions, CI changes, documentation updates, bug fixes. These go in the changelog section.

Use your judgment to determine what counts as "major." A good heuristic: if it would merit its own paragraph in a blog post, it's major.
</step>

<step name="generate_markdown">
Generate a markdown document with this structure:

```markdown
# {Project Name} {New Version} Release

**Released: {today's date}**
**Previous version: {last tag}**

## Highlights

{For each major feature, write 1-2 paragraphs in a press-release style. Be specific about what the feature does and why it matters. Use a friendly, professional tone. Each feature gets its own subsection with a ### heading.}

## Changelog

### Improvements
{Bulleted list of minor improvements, one per line}

### Bug Fixes
{Bulleted list of bug fixes, one per line}

### Dependencies
{Bulleted list of dependency updates, if any}

### Internal
{Bulleted list of refactors, CI changes, test additions, etc., if any}
```

Omit any changelog subsection that has no entries. Do not fabricate changes — only include what is actually in the diff.

For the "New Version" in the title, suggest a version by incrementing the last tag (e.g., if the last tag is `v24.03.02`, suggest `v24.03.03`). If the version scheme is unclear, just use "Next Release".
</step>

<step name="copy_to_clipboard">
Copy the generated markdown to the system clipboard:

```bash
pbcopy
```

Pipe the full markdown content to `pbcopy`. Then tell the user:

```
Release notes copied to clipboard.
```

Also print the full markdown to the conversation so the user can review it.
</step>

</process>
