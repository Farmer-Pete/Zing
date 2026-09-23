// keyboard.mjs — the console's pure keyboard logic (design section 6.4):
// the chord state machine, key-to-action resolution, input-context
// suppression, the mac/non-mac send-chord check, the id-based focus step,
// reconcileFocus, and collectPatchWork. No DOM access, no fetch, and no
// browser-absolute imports, so Node can import this module directly
// (make test-js runs node --test against it).
//
// This is a Task 1 skeleton: internal/console/keys.go, keys.json, and the
// real logic below land in Task 4 (design section 6.4, 7.3). The export
// below is a placeholder so this module has a stable shape for console.js
// to import and for a future console.test.js to exercise, and so `make
// test-js` has a real, passing target before Task 4 fills it in.

/**
 * ready reports that the module loaded, the one fact Task 1's placeholder
 * test-js target can assert without inventing keyboard behavior that
 * belongs to Task 4.
 * @returns {boolean}
 */
export function ready() {
	return true;
}
