# Zing Design System

Living reference for all UI design standards. The Zing brand is **bold**, **fast**, **warm**, and **whimsical**.

## Principles

- **Bold**: Heavy type weights (700–800), confident headings, strong color contrast
- **Fast**: Quick transitions (0.15–0.2s), speed-line gradient stripe, responsive hover states
- **Warm**: Navy + orange + amber palette, off-white backgrounds (not clinical gray), warm-tinted grays
- **Whimsical**: Subtle card rotation on hover, bouncing empty-state icons

## Color Tokens

### Brand Colors
| Token | Hex | Usage |
|-------|-----|-------|
| `--navy` | `#0f1b2d` | Header, headings, code blocks |
| `--navy-light` | `#1a2d4a` | Lighter navy backgrounds |
| `--navy-mid` | `#243b5e` | Hover states on dark elements |
| `--orange` | `#f57c20` | Primary accent, CTAs, active tab indicators |
| `--orange-hover` | `#e06810` | Orange hover/pressed states |
| `--amber` | `#f5a623` | Secondary accent, pending states |
| `--gold` | `#ffd666` | Highlights, selected states |
| `--cyan` | `#3ecfe0` | Links, info accents |
| `--cyan-dim` | `#2ba8b6` | Link hover states |
| `--red-orange` | `#e84430` | Severity high, destructive hover states |
| `--off-white` | `#f8f6f3` | Page background |

### Semantic Grays (warm-tinted)
| Token | Hex | Usage |
|-------|-----|-------|
| `--gray-50` | `#faf9f7` | Subtle backgrounds |
| `--gray-100` | `#f3f1ee` | Card backgrounds, panels |
| `--gray-200` | `#e8e5e1` | Borders, dividers |
| `--gray-300` | `#d4d0cb` | Inactive borders |
| `--gray-500` | `#8a8580` | Muted text, labels |
| `--gray-700` | `#4a4540` | Secondary text |
| `--text-primary` | `#1a1714` | Primary text (warm near-black) |

### Status Colors
| State | Background | Text |
|-------|-----------|------|
| Pending | `rgba(245,166,35,0.15)` | `--amber` |
| Started | `rgba(245,124,32,0.15)` | `--orange` |
| Ready | `rgba(62,207,224,0.15)` | `--cyan-dim` |
| Completed | `rgba(22,163,74,0.15)` | `#15803d` |

### Severity Colors
| Level | Background | Text |
|-------|-----------|------|
| Critical | `#fca5a5` | `#7f1d1d` |
| High | `#fee2e2` | `#991b1b` |
| Medium | `#fef3c7` | `#92400e` |
| Low | `#d1fae5` | `#065f46` |
| Info | `#e0e7ff` | `#3730a3` |

### Triage Action Colors (selected state)
| Action | Border/Background |
|--------|------------------|
| Accept | Green (`#16a34a` border, `#f0fdf4` bg) |
| Drop | Gray (`--gray-300` border, `--gray-50` bg) |
| Downgrade | Amber (`--amber` border, `#fffbeb` bg) |
| Discuss | Cyan (`--cyan` border, `#ecfeff` bg) |

## Typography

**Font**: Inter (Google Fonts), fallback: `system-ui, sans-serif`
**Weights loaded**: 500, 700, 800

| Element | Size | Weight | Color |
|---------|------|--------|-------|
| Page heading (h1) | 1.75rem | 800 | `--navy` |
| Section heading (h2) | 1.25rem | 700 | `--navy` |
| Body text | 1rem | 400 | `--text-primary` |
| Small/meta text | 0.875rem | 500 | `--gray-500` |
| Extra small | 0.75rem | 500–700 | varies |
| Monospace | 0.8125rem | — | `ui-monospace, SFMono-Regular, Menlo, monospace` |

## Spacing

| Context | Value |
|---------|-------|
| Page padding | 2rem |
| Max content width | 960px |
| Card padding | 1rem |
| Card gap / margin-bottom | 1rem |
| Section gap | 1.5rem |
| Small gap | 0.5rem |
| Border radius (cards) | 10px |
| Border radius (badges) | 999px |
| Border radius (buttons) | 6px |

## Components

### Brand Header Bar
- Full-width navy (`--navy`) background, bleeds to edges
- Contains logo SVG (links to `/dashboard`), height 111px
- `::after` pseudo-element: 3px gradient stripe (red-orange → orange → amber → gold → cyan)
- Padding: `0.75rem 2rem`

### Timeline Cards (Dashboard)
- Entire card is an `<a>` tag linking to `/{session_id}`
- Left border: 3px solid transparent (turns `--orange` on hover)
- Border radius: 10px
- Content order: title (bold) → metadata (zing_file, step, finding count) → footer (time + delete)
- Status badge aligned top-right via flex
- Hover: orange left border, slight rotation (`rotate(-0.3deg) translateY(-1px)`), elevated shadow
- Delete button: transparent bg, gray text, turns red-orange on hover; uses `event.stopPropagation()`

### Status Badges
- Pill shape: `border-radius: 999px`, `padding: 0.125rem 0.625rem`
- Weight 700, uppercase, `font-size: 0.7rem`, `letter-spacing: 0.05em`
- Colors per state (see Status Colors above)

### Severity Badges
- Same pill treatment as status badges
- Colors per severity level (see Severity Colors above)

### Triage Action Buttons
- Base: `padding: 0.375rem 0.75rem`, `border: 2px solid --gray-200`, `border-radius: 6px`, `font-weight: 600`
- Hover: border darkens
- Selected: color-coded border and background per action (see Triage Action Colors)
- Transition: `all 0.15s ease`

### Finding Cards
- White background, `border: 1px solid --gray-200`, `border-radius: 10px`
- Shadow: `0 1px 3px rgba(26,23,20,0.06)`
- Evaluation findings: `border-left: 3px solid --amber`
- Blockquotes inside: `border-left: 3px solid --amber`
- Links inside: `--cyan` color

### Tab Navigation
- Flex row, sits on a `2px solid --gray-200` bottom border
- Each tab: `padding: 0.625rem 1.25rem`, `font-weight: 600`, `font-size: 0.875rem`
- Active tab: `--navy` text, `3px solid --orange` bottom border, overlaps nav border by `-2px`
- Inactive: `--gray-500` text, transparent bottom border
- Hover: text darkens, subtle `--gray-300` bottom border

### Agent Status Panel
- Background: `--gray-100`, `border: 1px solid --gray-200`, `border-radius: 10px`
- Title: uppercase, `0.75rem`, weight 700, `letter-spacing: 0.05em`, `--gray-500`
- Running agents: `--orange` color with pulsing dot
- Completed agents: `--gray-500` color with checkmark

### Submit / CTA Buttons
- `--orange` background, white text, weight 700, `border-radius: 8px`
- `padding: 0.75rem 2rem`, `font-size: 1rem`
- Shadow: `0 4px 14px rgba(245,124,32,0.3)`
- Hover: `--orange-hover` bg, shadow intensifies, `translateY(-1px)`

### Command Center Card Buttons
- Two visual tiers based on what the button does:
  - **Orange (primary)**: Executes a server-side action (launch session, start ticket, setup environment). Uses `card-btn card-btn-primary`.
  - **White (default)**: Copies a command to the clipboard for the user to run manually. Uses `card-btn` only.
- Small size: `0.55rem`, `padding: 0.2rem 0.45rem`, `border-radius: 4px`
- Hidden at rest (`opacity: 0.15`), revealed on card hover (`opacity: 1`)
- Transition: `background 0.1s, opacity 0.15s`

### Delete / Destructive Buttons
- Transparent background, `--gray-500` text, `1px solid --gray-300` border
- Hover: `--red-orange` text, `--red-orange` border, `rgba(232,68,48,0.05)` background
- Small size: `0.75rem`

### Empty States
- Centered text, `--gray-500` color
- Animated icon (bounce keyframe, 2s infinite)
- Friendly, whimsical copy

## Interactions

| Element | Effect | Duration |
|---------|--------|----------|
| Timeline cards | Orange border + rotation + lift + shadow | 0.2s ease |
| Timeline dots | Scale 1.3× | 0.15s ease |
| Buttons | Background/border color change | 0.15s ease |
| Submit button | Lift + shadow intensify | 0.2s ease |
| Tab hover | Bottom border appears | 0.15s |
| Card hover | Box-shadow elevation | 0.2s ease |

## Layout

- Max width: 960px, centered
- Header: full-width bleed (negative margins + padding to compensate)
- Body background: `--off-white` with subtle warm radial gradient at top
- Main content wrapped in `<main class="main-content">` with `padding: 2rem 0`


## Datastar conventions

The server uses Datastar v1.0.0 for declarative reactivity. Most UI state lives in `data-signals` / `data-show` / `data-class` attributes; per-action endpoints respond with SSE patches. See the **Datastar usage** section in the root `CLAUDE.md` for the full architecture rule, decision tree, and helper APIs. Below are the design-system-specific bits.

### Signal naming

`camelCase`. Modal-open booleans group into a `modals: {}` sub-object on the page envelope (e.g. `$modals.drawer`, `$modals.standup`, `$modals.terminal`). Drawer-internal state lives on the drawer fragment itself.

### Button-state pattern

For server-side actions (kill, cleanup, launch, etc.):

- Wire the button with `data-on:click="@post(...)"` carrying a payload that includes a stable `btn_id`.
- Use `data-indicator="$busyButtons.X"` to flip a per-button busy signal automatically while the request is in flight.
- Pre-initialize the busy signal to `false` in the parent `data-signals` envelope (Datastar v1 defaults uninitialized indicator signals to `true`).
- For post-completion ok/err copy ("✓ Launched!", "Failed"), the server yields `_sse_btn_state(btn_id, "✓ Launched!", kind="ok", reset_html=<original>)` from `src/zing_ai/server/sse_helpers.py`. The patched button auto-restores the original markup after `reset_after_ms`.

### Toast pattern

For transient notifications, the server yields `_sse_toast("Refreshed", "ok")` from `sse_helpers.py`. Toasts append to `#cc-toast-container` and self-remove after 5s via `data-init__delay.5000ms="el.remove()"`. Toast kinds: `ok`, `err`, `info` — each maps to a `cc-toast-{kind}` class.
