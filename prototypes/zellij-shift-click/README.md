# Zellij Shift+Click prototype

Reproduces: Zellij's web client renders a "Shift-Click: <url>" tooltip on
hover, but actually pressing Shift+Click does nothing — the click registers
as a plain (un-modified) click.

## Run

```
uv run python prototypes/zellij-shift-click/prototype.py
# then open http://127.0.0.1:8091
```

A session named `zing-shiftclick` is created on startup, displaying three
URLs. The whole page is the iframe; the bottom strip has two log columns:

- **left** — every mouse event the browser saw, with modifier flags, both
  on the `<iframe>` element (`outer`) and inside the iframe document
  (`inner`, same-origin via the proxy).
- **right** — every WebSocket frame containing a mouse CSI sequence
  (`\x1b[<...M`/`m`), labelled with direction.

Together these answer the two basic questions:
1. Did the browser ever see the Shift modifier on the click?
2. If yes, did the modifier survive into the bytes shipped to Zellij?

## Iterating on theories

Patches live in two functions inside `prototype.py`:

- `_patch_input_js(text)` — applied to `/assets/input.js` only.
- `_patch_mouse_or_link_js(filename, text)` — applied to every JS asset
  (so it can patch `mouse.js`, `xterm*.js`, `main.js`, …).

After editing either, click **reload iframe** in the prototype (no Python
restart needed — assets are fetched per request).

The first time you load the page, every Zellij asset is dumped *unpatched*
to `./_assets_dump/`. Use the Read tool on those files to find the
function you want to patch.

## Theory checklist

| # | Theory | How to confirm in this prototype |
|---|--------|----------------------------------|
| A | Browser's native Shift+Click (extend selection) wins; xterm.js never gets a useful click. | Inner log shows `mousedown SHIFT` but `defaultPrevented=false`, then text gets selected on the page. |
| B | xterm.js link provider is registered with the wrong modifier (e.g. expects Alt/Cmd, not Shift). | Inner log shows the click reaches xterm.js with SHIFT, but no nav happens. Search dumped JS for `pointerEvents`, `LinkProvider`, `activateMatcher`, `linkActivationModifierKey`. |
| C | Mouse-protocol path swallows the click first: Zellij is in mouse-reporting mode, so the modifier is encoded into a CSI sequence sent to the server, and the server-side link handler ignores it. | Right column shows a mouse CSI frame on click; the button byte should differ for Shift (bit 4 set in xterm SGR encoding). If it's identical to a no-shift click, the modifier was dropped at encode time. |
| D | iframe sandbox / lack-of-`allow-popups` blocks the `window.open()` the link handler would call. | Inner log shows the full click chain, no `defaultPrevented`, but `window.open` returns null. Try removing/loosening any iframe `sandbox` attribute (this prototype already uses none). |

Most likely (based on the screenshot — tooltip exists, click is treated as
plain) is **A** or **C**. Start there.

## Notes

- Port 8091 (app) / 8083 (Zellij). Change `APP_PORT` / `ZELLIJ_WEB_PORT` if
  they collide with the other prototype, which uses 8090 / 8082.
- The iframe is same-origin (everything proxied through the app), so the
  prototype can attach event listeners *inside* the iframe document. If a
  patch breaks that (e.g. by changing the proxy origin), the inner log
  goes silent and you'll see a status note in the top bar.
