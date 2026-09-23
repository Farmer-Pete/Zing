// console.test.js — node --test over keyboard.mjs (design section 6.4,
// Makefile target test-js). This file is never served: the static mux
// (internal/console/server.go) allowlists five named assets, and this path
// is not one of them (design section 5, 12).
//
// This is a Task 1 placeholder so `make test-js` is green before Task 4
// exists: the real coverage (the chord state machine and timeout,
// key-to-action resolution, input-context suppression, the mac/non-mac
// send chord, the id-based focus step, reconcileFocus, and
// collectPatchWork) lands in Task 4 alongside the real keyboard.mjs.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { ready } from './keyboard.mjs';

test('keyboard.mjs loads as an ES module', () => {
	assert.equal(ready(), true);
});
