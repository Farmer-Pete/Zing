# Zing!

<img width="500" height="607" alt="Zing! Don't wing your code. Zing your code." src="https://github.com/user-attachments/assets/57de6005-633f-4c13-93a3-427264fd3ee1" />
<br /><br /><br />

> Zing plans, builds, tests, and ships clean code with AI.<br/>
> Zing keeps you in charge at each step.<br/>
> This is the Zen of Zing.

---

## The Problem

AI is good at a lot of things. Reducing entropy isn't one of them.

Every AI-generated line of code is a small bet. Sometimes it's exactly right. Often it's close enough. But over hundreds and thousands of those bets, the misses accumulate. Variable names drift. Abstractions get duplicated. Edge cases get papered over. The codebase collapses because it decays.

AI coding assistants are great at writing functions. But shipping software isn't just writing functions. It's understanding what to build, figuring out where it fits in the codebase, breaking work into steps that make sense, building it incrementally, reviewing it properly, and tracking it in your project management tool. Today, you do all of that orchestration yourself, or you don't, and entropy wins.

## Why Zing is Different

### It's an entropy reduction engine.
Zing gives you the tools to stay in control. You make the big decisions: what to build, which approach to take, which trade-offs to accept. The AI handles the smaller implementation decisions within the boundaries you set. This is the opposite of vibe coding. It's structured coding with AI as the executor, not the architect.

### The review loop.
Code review isn't just a gate at the end. It's how the system self-improves. Four specialized review agents catch inconsistencies, flag drift from the plan, and surface entropy before it gets committed. Every review pass actively removes disorder from the system. The result is a codebase that gets cleaner over time, not messier, even though AI is writing most of the code, because humans are involved in every step of the process.

### It's a pipeline, not a prompt.
Each stage feeds into the next. The plan audit catches problems before they're built. The build follows the audited plan exactly. The code review knows what was intended because it has the spec. Entropy can't sneak in between the cracks when there are no cracks.

### Parallelism is a first-class concept.
Codebase exploration, plan evaluation, and code review all fan out across multiple specialized agents working simultaneously. This isn't just faster. It produces better results because each agent has a focused lens.

### Humans stay in the loop.
Zing doesn't disappear into a corner and come back with a PR. It checks in at every stage, confirming the spec, walking through plan improvements, discussing review findings one by one. You make the decisions. Zing does the legwork.

### Chain of thought, not chain of hope.
The pipeline structure acts as an external chain of thought. Each stage narrows the problem space before the next one starts. The AI never has to hold an entire complex system in its head at once. It specs, then plans, then builds one step at a time. This is how you get reliable output on complex systems.

### It's opinionated about discipline.
The build phase has strict anti-patterns: no deviations from the plan, no bonus features, no drive-by refactoring. Every step has acceptance criteria. Every completion gets a commit. This is how you ship reliably with AI, by keeping it on rails.

## What Zing Does

Zing is a pipeline of eight specialized AI tools that integrate with one another:

### 1. Capture (`/zing`)
Zing starts with a conversation or a Linear ticket URL. It listens, asks the right questions, and saves a structured spec to your `.zing/` directory. No templates to fill out. Just talk about what you want to build.

### 2. Plan (`/zing-plan`)
Zing explores your codebase in parallel. Multiple agents fan out simultaneously to map relevant files, understand existing patterns, and identify integration points. It asks you targeted questions, then produces a concrete action plan with acceptance criteria for every step.

### 3. Audit the Plan (`/zing-plan-audit`)
Before a single line of code is written, parallel evaluation passes stress-test your plan:

- **Design Fundamentals**: Is this the right approach? Is it overengineered?
- **Robustness & Safety**: Will it break things? Is it testable?
- **Executable Spec**: Is every step specific enough to actually build?
- **Code Quality**: Does it follow the idioms of the codebase?

Each dimension gets a rating, and weak spots come with concrete improvement options.

### 4. Track (`/zing-plan-linear`)
Zing creates a Linear project with tickets for each phase, sequential dependencies between them, and the full plan attached as a document. Your project management stays in sync without you touching it.

### 5. Build (`/zing-build`)
Zing executes the plan step by step. After each step, it commits, updates the progress checklist, and moves on. No scope creep, no unsolicited refactoring, no features that weren't in the plan. Just disciplined, incremental delivery against the spec.

### 6. Review (`/zing-build-audit`)
Four parallel review agents examine your branch's changes like senior developers:

- **Correctness**: Logic errors, edge cases, error handling
- **Security & Reliability**: Vulnerabilities, production readiness
- **Quality & Style**: Naming, readability, idiomatic code
- **Coverage & Performance**: Test gaps, bottleneck risks

Findings come with severity and confidence ratings. Zing walks you through each one individually, discusses fixes, and writes a full review report.

### 7. Ship
When the review is clean, Zing offers to open your pull request. Draft by default, because you're still in control.

### 8. PR Review (`/zing-pr-audit`)
Once a PR is open, Zing can review it the way a senior developer would — on GitHub, with line-level comments. It checks out the PR branch, reads every changed file in full, fans out four parallel review agents (the same ones from the build review), and walks you through each finding before submitting. The final review is posted via the GitHub API with inline comments on the exact lines that matter, severity ratings, and code suggestions where the fix is obvious. The review action (approve, comment, or request changes) is your call. A local markdown report is also saved so you can feed findings straight back into `/zing-plan` if fixes are needed.

---

## Installation

### Prerequisites

- Python >= 3.12
- [uv](https://docs.astral.sh/uv/getting-started/installation/)

### Recommended MCP Servers

Zing works best when your AI coding assistant has the following MCP servers installed and configured:

- **[Serena](https://github.com/oraios/serena)** — Semantic code tools via LSP for token-efficient code exploration and precise symbol-level editing
- **[AI Distiller](https://github.com/janreges/ai-distiller)** — Compact code structure extraction and specialized analysis (security audits, bug hunting, refactoring)
- **[CodeGraphContext](https://github.com/CodeGraphContext/CodeGraphContext)** — Code graph analysis for understanding call chains, detecting dead code, and architectural queries
- **[Context7](https://github.com/upstash/context7)** — Up-to-date library documentation and code examples, so your assistant doesn't rely on stale training data

These servers give Zing's agents deeper insight into your codebase during planning, building, and reviewing.

### Install from PyPI

```
uv tool install zing-ai
```

### Install bleeding edge from GitHub

```
uv tool install git+https://github.com/Farmer-Pete/Zing
```

### Set up commands for your AI coding assistant

**Interactive mode** (asks which runtime to install for):
```
zing-ai install
```

**Claude Code:**
```
zing-ai install --claude
```

**OpenCode:**
```
zing-ai install --opencode
```

**Both:**
```
zing-ai install --all
```

---

## Updating

```
uv tool upgrade zing-ai
zing-ai install --claude   # or --opencode or --all
```

If you've customized any commands, they'll be backed up to a `zing-patches/` directory before being overwritten. To see your backed-up customizations:

```
zing-ai reapply-patches --claude   # or --opencode
```

---

## Usage

After installation, the zing commands are available as slash commands in your AI coding assistant:

- `/zing` — Start a new zing (capture what you want to build)
- `/zing:plan` — Break it down into an actionable plan
- `/zing:plan-audit` — Audit the plan for soundness
- `/zing:build` — Execute the plan step by step
- `/zing:build-audit` — Review the code changes
- `/zing:pr-audit` — Review a pull request on GitHub
- `/zing:plan-linear` — Create Linear tickets from the plan

In OpenCode, use flat naming: `/zing-plan`, `/zing-build`, etc.
