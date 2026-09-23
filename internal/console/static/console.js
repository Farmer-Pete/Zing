// console.js — the console's DOM wiring (design section 6.3, 6.4): imports
// keyboard.mjs and mermaid, reads keys.json, installs the chord state
// machine and focus ring, and bridges to Datastar by dispatching a
// zing-nav CustomEvent on #stream-ctl. It is the only module with
// browser-absolute imports, and it is not unit-tested under Node
// (keyboard.mjs carries every piece of pure logic Node can exercise).
//
// This is a Task 1 skeleton, served at /static/console.js and digest-safe
// to load, but not yet wired into the shell's <script> tags and not yet
// importing mermaid (design section 6.10 wires that import at Task 4/5,
// once static/ASSETS.md's vendored-mermaid gap is resolved). The real
// wiring lands in Task 4.

import './keyboard.mjs';
