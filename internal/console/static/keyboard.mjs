// keyboard.mjs — the console's pure keyboard logic (design section 6.4):
// the chord state machine, key-to-action resolution over a parsed
// keys.json, input-context detection, the send-chord platform check, the
// id-based focus step, reconcileFocus, and collectPatchWork. No DOM
// access, no fetch, and no browser-absolute imports, so Node can import
// this module directly (make test-js runs node --test against it).
//
// Every export takes and returns plain data (strings, arrays, plain
// objects), never a DOM node or an Event, so it stays Node-testable.
// console.js is the only module that touches the DOM, fetch, or the
// keyboard event object itself; it normalizes those into the plain
// descriptors these functions take (design section 6.4: "Each takes plain
// data, not DOM nodes").

// CHORD_TIMEOUT_MS bounds how long a leading "g" stays armed waiting for
// its second key (design section 6.4: "a leading g arms a chord with a
// short timeout"). 1000ms is comfortable for a deliberate two-key chord
// without lingering long enough to swallow an unrelated "g" typed shortly
// after, e.g. while composing a reply.
export const CHORD_TIMEOUT_MS = 1000;

/**
 * emptyChordState is the chord machine's rest state: no leader armed.
 * @returns {{leader: string|null, armedAt: number|null}}
 */
export function emptyChordState() {
	return { leader: null, armedAt: null };
}

/**
 * advanceChord processes one normalized key against the chord state
 * (design section 6.4: "a leading g arms a chord with a short timeout; a
 * following i, r, or f sets view ...; any other key or the timeout clears
 * the chord"). It never itself decides whether the resulting chord is a
 * real action — that is resolveAction's job over the returned chord string
 * — so an unarmed "g" followed by an unrecognized third key still clears
 * back to the rest state, matching "any other key ... clears the chord".
 *
 * @param {{leader: string|null, armedAt: number|null}} state
 * @param {string} key - one normalized key token, e.g. "g", "i", "j"
 * @param {number} now - Date.now()-style milliseconds for this keypress
 * @returns {{state: object, chord: string|null}} chord is "g i"-style when
 *   this key completes a chord that was armed within CHORD_TIMEOUT_MS, else
 *   null (either no chord was armed, or the arm expired).
 */
export function advanceChord(state, key, now) {
	const armed = Boolean(state?.leader) && state.armedAt !== null && now - state.armedAt <= CHORD_TIMEOUT_MS;
	if (armed) {
		return { state: emptyChordState(), chord: `${state.leader} ${key}` };
	}
	if (key === 'g') {
		return { state: { leader: 'g', armedAt: now }, chord: null };
	}
	return { state: emptyChordState(), chord: null };
}

/**
 * resolveAction looks up a normalized key token (a single key like "j", or
 * a completed chord like "g i") in a parsed keys.json, returning the one
 * action it binds to, or null when the token binds nothing (design section
 * 6.4: "key-to-action resolution (given a parsed keys.json passed in)").
 *
 * @param {string|null} token
 * @param {{keys: string[], action: string}[]} bindings - parsed keys.json
 * @returns {string|null}
 */
export function resolveAction(token, bindings) {
	if (!token || !bindings) {
		return null;
	}
	for (const binding of bindings) {
		if (binding.keys?.includes(token)) {
			return binding.action;
		}
	}
	return null;
}

/**
 * isInputContext reports whether a plain descriptor of the current event
 * target names a text input, a textarea, or a contenteditable element
 * (design section 6.4: "input-context detection (are we in an
 * input/textarea)"). console.js passes a plain {tagName, isContentEditable}
 * built from document.activeElement / event.target, never the element
 * itself.
 *
 * @param {{tagName?: string, isContentEditable?: boolean}} descriptor
 * @returns {boolean}
 */
export function isInputContext(descriptor) {
	if (!descriptor) {
		return false;
	}
	const tag = (descriptor.tagName || '').toUpperCase();
	return tag === 'INPUT' || tag === 'TEXTAREA' || Boolean(descriptor.isContentEditable);
}

/**
 * isSendChord reports whether a plain key-event descriptor is the
 * platform's send chord: Cmd-Enter on macOS, Ctrl-Enter elsewhere (design
 * section 6.4: "the send-chord platform check (Cmd on mac, Ctrl
 * elsewhere)"). isMac is passed in rather than read from navigator here, so
 * this stays pure and Node-testable for both platforms.
 *
 * @param {{key?: string, metaKey?: boolean, ctrlKey?: boolean}} descriptor
 * @param {boolean} isMac
 * @returns {boolean}
 */
export function isSendChord(descriptor, isMac) {
	if (!descriptor || descriptor.key !== 'Enter') {
		return false;
	}
	return isMac ? Boolean(descriptor.metaKey) : Boolean(descriptor.ctrlKey);
}

/**
 * sendChordToken names the keys.json key string the platform's send chord
 * resolves through: "Cmd-Enter" on macOS, "Ctrl-Enter" elsewhere.
 * @param {boolean} isMac
 * @returns {string}
 */
export function sendChordToken(isMac) {
	return isMac ? 'Cmd-Enter' : 'Ctrl-Enter';
}

/**
 * stepFocus returns the next id to focus given the current ordered ids and
 * the presently focused id (design section 6.4: "the next-and-previous
 * focus step over a list of ids"). From no focus, "next" (j) lands on the
 * first id and "previous" (k) on the last (design section 6.4: "From no
 * focus, j focuses the first item and k the last"). Stepping past either
 * end holds at that end rather than wrapping, since the design names no
 * wrap-around behavior and clamping is the least surprising default for a
 * flat list. A focusedID no longer present in ids (for example, a stale id
 * from before a patch, ahead of the next reconcileFocus call) is treated
 * the same as no focus, so a step never gets stuck on a vanished row.
 *
 * @param {string[]} ids - ordered focusable ids
 * @param {string} focusedID - "" for no focus
 * @param {'next'|'prev'} direction
 * @returns {string} the id to focus, or "" when ids is empty
 */
export function stepFocus(ids, focusedID, direction) {
	if (!ids || ids.length === 0) {
		return '';
	}
	const i = focusedID ? ids.indexOf(focusedID) : -1;
	if (i === -1) {
		return direction === 'next' ? ids[0] : ids[ids.length - 1];
	}
	if (direction === 'next') {
		return i + 1 < ids.length ? ids[i + 1] : ids[i];
	}
	return i - 1 >= 0 ? ids[i - 1] : ids[i];
}

/**
 * reconcileFocus computes the id to focus after a patch changes the
 * focusable list (design section 6.3, 6.4). When focusedID is still present
 * in currentIDs, it is returned unchanged. Otherwise its row was removed:
 * the id now sitting at its old ordinal in currentIDs takes over (the row
 * that shifted into its place), else the id before that ordinal, else no
 * focus. previousIDs is what supplies the "old ordinal" — it is not
 * recoverable from currentIDs alone once the row is gone, which is why
 * console.js retains the previous order across patches (design section
 * 6.4: "reconcileFocus(previousIDs, currentIDs, focusedID) -> nextFocusedID
 * (focused id gone: item at old ordinal in currentIDs, else the one
 * before, else empty)").
 *
 * @param {string[]} previousIDs
 * @param {string[]} currentIDs
 * @param {string} focusedID
 * @returns {string}
 */
export function reconcileFocus(previousIDs, currentIDs, focusedID) {
	if (!focusedID) {
		return '';
	}
	if (currentIDs.includes(focusedID)) {
		return focusedID;
	}
	const oldOrdinal = previousIDs.indexOf(focusedID);
	if (oldOrdinal === -1) {
		return '';
	}
	if (oldOrdinal < currentIDs.length) {
		return currentIDs[oldOrdinal];
	}
	if (oldOrdinal - 1 >= 0 && oldOrdinal - 1 < currentIDs.length) {
		return currentIDs[oldOrdinal - 1];
	}
	return '';
}

/**
 * collectPatchWork is the one call the MutationObserver callback makes each
 * patch (design section 6.3, 6.4): descriptors.diagramIDs passes straight
 * through (collecting which nodes are new mermaid diagrams is
 * console.js's DOM-walking job, not a decision this function makes), and
 * focusID is reconcileFocus(descriptors.previousFocusableIDs,
 * descriptors.focusableIDs, focusedID) — console.js supplies the retained
 * previous order as part of descriptors so this stays a single pure call
 * per patch, with no DOM work and no hidden state of its own.
 *
 * @param {{diagramIDs?: string[], focusableIDs?: string[], previousFocusableIDs?: string[]}} descriptors
 * @param {string} focusedID
 * @returns {{diagramIDs: string[], focusID: string}}
 */
export function collectPatchWork(descriptors, focusedID) {
	const diagramIDs = descriptors?.diagramIDs ?? [];
	const currentIDs = descriptors?.focusableIDs ?? [];
	const previousIDs = descriptors?.previousFocusableIDs ?? [];
	return { diagramIDs, focusID: reconcileFocus(previousIDs, currentIDs, focusedID) };
}
