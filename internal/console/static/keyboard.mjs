// keyboard.mjs — the console's pure keyboard logic (design section 6.4):
// the chord state machine, key-to-action resolution over a parsed
// keys.json, event-to-token resolution (resolveToken), input-context
// detection, the send-chord platform check, the id-based focus step,
// reconcileFocus, and collectPatchWork. No DOM
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
	// Require exactly the platform modifier, nothing else held (PR #16
	// review, CodeRabbit console.js:567 / cubic console.js:565): an OS chord
	// that happens to also hold Alt (e.g. Ctrl-Alt-Enter) or Shift is not
	// this app's send chord, and must not misfire it. Mac and
	// non-mac are also mutually exclusive, so a Ctrl-Cmd-Enter (unlikely, but
	// not impossible on an external keyboard) does not send on either read.
	if (descriptor.altKey || descriptor.shiftKey) {
		return false;
	}
	return isMac
		? Boolean(descriptor.metaKey) && !descriptor.ctrlKey
		: Boolean(descriptor.ctrlKey) && !descriptor.metaKey;
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
 * sendChordLabel is the human-readable glyph for the platform's send chord
 * (bug fix: the composer saves a draft silently and sends only on this
 * chord, Q31, with nothing on screen saying so): "⌘+Enter" on macOS,
 * "Ctrl+Enter" elsewhere. Distinct from sendChordToken, which names the
 * keys.json binding string ("Cmd-Enter"/"Ctrl-Enter") rather than what a
 * reader sees on screen; console.js's runSendChordHints fills this into
 * every ".send-chord" placeholder thread.templ renders.
 * @param {boolean} isMac
 * @returns {string}
 */
export function sendChordLabel(isMac) {
	return isMac ? '⌘+Enter' : 'Ctrl+Enter';
}

/**
 * draftConflictMessage maps a POST /draft 409 body (store.ConflictError's
 * Reason, console_writes.go's own small closed set of conflict() call
 * sites) to the sentence the reply box shows beside itself (bug fix:
 * pressing Enter on a question that closed out from under a stale,
 * still-rendered reply box -- most often a race with the owner's own
 * just-sent batch answer -- got a raw "question closed" 409 and the typed
 * text silently vanished, with nothing explaining why). "question closed"
 * is the one reason a reader hits often enough to need a plain sentence;
 * every other reason (a bad option or item ref, an ambiguous or missing
 * draft) is rare enough from the real console UI, which only ever sends
 * well-formed requests, that its own store wording is shown as-is.
 *
 * @param {string} reason
 * @returns {string}
 */
export function draftConflictMessage(reason) {
	if (reason === 'question closed') {
		return 'This question is already answered.';
	}
	return reason;
}

/**
 * clearReplyInputs sets every given element's value to the empty string
 * (console.js's postSendBatch, design section 22.7): a 200 from /send
 * clears the value of every .reply-input inside #main, so a sent draft's
 * own box does not keep showing text the owner just sent (Datastar's morph
 * never refills a focused input, bug 11's own reason freeReply already
 * stopped clearing it on a plain Enter). Pure: it takes the already-queried
 * elements, or any duck-typed {value} object in a test, not a selector, so
 * this file stays DOM free.
 *
 * @param {{value: string}[]} inputs
 */
export function clearReplyInputs(inputs) {
	for (const el of inputs) {
		el.value = '';
	}
}

// TOAST_DISMISS_MS is how long showSendResult's bottom toast stays on
// screen before auto-dismissing (bug fix 12): the owner's "Sent 1 answer."
// or "Nothing to send." line never went away on its own, so it kept
// reporting a send that had happened minutes earlier as if it just had.
// 4000ms is long enough to read, short enough not to linger into the next
// action.
export const TOAST_DISMISS_MS = 4000;

/**
 * scheduleToastDismiss arms showSendResult's next auto-dismiss, canceling
 * whatever dismiss it is replacing first (bug fix 12: sending twice in
 * close succession armed two independent timers, and the first one's firing
 * could clear a toast the second send had only just shown -- "replace, not
 * stack"). schedule and clear are the caller's setTimeout/clearTimeout (or a
 * test's fakes), so the cancel-then-arm decision stays testable without a
 * real timer.
 *
 * @param {number|null} prevTimerID the previous pending dismiss's id, or
 *   null when none is pending yet
 * @param {() => number} schedule arms the next dismiss and returns its id
 * @param {(id: number) => void} clear cancels a pending dismiss by id
 * @returns {number} the new pending dismiss's id
 */
export function scheduleToastDismiss(prevTimerID, schedule, clear) {
	if (prevTimerID !== null) {
		clear(prevTimerID);
	}
	return schedule();
}

/**
 * resolveToken turns one plain keydown descriptor, plus whether it landed in
 * an input, into the token keys.json binds (design section 6.4, 8; PR review
 * fix: a Ctrl/Meta/Alt-held single key outside an input must not resolve to
 * a bare action token, or Ctrl-1, Cmd-A, Ctrl-X, and so on fire console
 * actions and block the browser's own shortcuts for them). The send chord
 * and Esc always resolve the same way regardless of context ("Keys are
 * suppressed while an input is focused, except Esc, Enter, and the send
 * chord"); Tab/Shift-Tab likewise always resolve, since moveComposerFocus
 * itself is a no-op outside the composer. Every other key is suppressed
 * while typing in an input (returns null, so the character types normally).
 * Outside an input, a single-key token is suppressed too when Ctrl, Meta, or
 * Alt is held, since isSendChord above is the one modifier-bearing binding
 * this app defines; Shift alone is not blocked, since the app binds no
 * Ctrl/Meta/Alt chords of its own and needs plain and shifted characters
 * (e.g. "?") to resolve the same way.
 *
 * @param {{key?: string, ctrlKey?: boolean, metaKey?: boolean, altKey?: boolean, shiftKey?: boolean, isComposing?: boolean, keyCode?: number}} descriptor
 * @param {boolean} inInput
 * @param {boolean} isMac
 * @returns {string|null}
 */
export function resolveToken(descriptor, inInput, isMac) {
	if (!descriptor) {
		return null;
	}
	// An IME still composing (e.g. picking a kanji candidate) fires its own
	// keydown with key "Enter" to confirm the composition, not to send or save
	// a draft (PR #16 review, CodeRabbit console.js:567 / cubic console.js:565).
	// isComposing is the modern signal; keyCode 229 is the legacy one
	// older/some mobile browsers still set instead. Returning null here, before
	// either the send-chord or Enter-in-input checks below, lets the IME's own
	// Enter handling run rather than misfiring either action.
	if (descriptor.key === 'Enter' && (descriptor.isComposing || descriptor.keyCode === 229)) {
		return null;
	}
	if (isSendChord(descriptor, isMac)) {
		return sendChordToken(isMac);
	}
	if (descriptor.key === 'Escape') {
		return 'Esc';
	}
	if (descriptor.key === 'Tab') {
		return descriptor.shiftKey ? 'Shift-Tab' : 'Tab';
	}
	if (inInput) {
		return descriptor.key === 'Enter' ? 'Enter-in-input' : null;
	}
	if (descriptor.ctrlKey || descriptor.metaKey || descriptor.altKey) {
		return null;
	}
	return descriptor.key;
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
 * navChanged reports whether next's view, open, and project differ from
 * current's (design section 6.4): navigate's own "did the destination
 * actually change" check, used both to decide whether to push the back
 * stack and whether to clear focus. A no-op navigation (the destination
 * equals the current position) must not push, or "u" needs one press per
 * repeated no-op navigation to undo (code review fix 4).
 *
 * @param {{view: string, open: number, project: number}} current
 * @param {{view: string, open: number, project: number}} next
 * @returns {boolean}
 */
export function navChanged(current, next) {
	return next.view !== current.view || next.open !== current.open || next.project !== current.project;
}

/**
 * reduceNav computes console.js's next local nav mirror and back stack from
 * one zing-nav event's detail (PR #16 review, CodeRabbit console.js:315 /
 * cubic console.js:57): the single reducer both the keyboard nav path
 * (navigate(), which dispatches zing-nav itself) and the click nav path
 * (nav.templ's temporary links, which dispatch zing-nav directly on
 * #stream-ctl) run through, so state.nav updates the same way regardless
 * of which one triggered the navigation -- previously only the keyboard
 * path updated it, leaving the client back-stack ("u") and focus model
 * desynced after a mouse click.
 *
 * detail.isBack marks a navigation as itself a back-navigation (goUp's own
 * pop), matching navigate's original opts.isBack: it must not push another
 * back-stack entry, or "u" would need repeated presses to actually go up.
 * A click-triggered detail never sets it, since only goUp ever pops.
 *
 * @param {{view: string, open: number, project: number}} currentNav
 * @param {{view: string, open: number, project: number}[]} history
 * @param {{view: string, open: number, project: number, isBack?: boolean}} detail
 * @returns {{nav: object, history: object[], changed: boolean}}
 */
export function reduceNav(currentNav, history, detail) {
	const next = { view: detail.view, open: detail.open, project: detail.project };
	const changed = navChanged(currentNav, next);
	const nextHistory = changed && !detail.isBack ? [...history, { ...currentNav }] : history;
	return { nav: next, history: nextHistory, changed };
}

/**
 * nextPendingNav decides what console.js's zing-nav bridge should remember
 * as "not yet applied" after reducing one zing-nav event (bug fix: the
 * first click on a Threads-sidebar row right after a page load could fire
 * before GET /stream's first frame proves Datastar's own data-on:zing-nav
 * listener on #stream-ctl is actually wired up, which silently dropped the
 * nav -- the main pane stayed on the project list until a second click).
 * console.js's onZingNav calls this after reduceNav on every event,
 * keyboard- or click-triggered alike; its caller re-dispatches the
 * returned nav once the stream's first real patch lands (installPatchObserver's
 * markStreamConnected), so a nav that arrived too early is applied anyway
 * instead of lost. Once the stream has connected, Datastar's own listener
 * is known to be live, so there is nothing left to remember.
 *
 * @param {boolean} streamConnected
 * @param {{view: string, open: number, project: number}} nav - reduceNav's
 *   own result for this event, the destination to re-apply if needed
 * @returns {{view: string, open: number, project: number}|null}
 */
export function nextPendingNav(streamConnected, nav) {
	return streamConnected ? null : { view: nav.view, open: nav.open, project: nav.project };
}

/**
 * stepComposerIndex returns the next composer-control index for Tab (delta
 * 1) or Shift-Tab (delta -1) stepping over count controls, given the
 * currently focused control's index or -1 when none of them has focus
 * (design section 6.4). From no focus, Tab lands on the first control
 * (index 0) and Shift-Tab on the last (count - 1) -- handled as its own
 * case rather than folded into the wrap formula below, which would
 * otherwise treat "no focus" as index -1 and land Shift-Tab one short of
 * the last control (code review fix 3).
 *
 * @param {number} count - number of composer controls; must be > 0
 * @param {number} index - the currently focused control's index, or -1
 * @param {number} delta - 1 for Tab, -1 for Shift-Tab
 * @returns {number}
 */
export function stepComposerIndex(count, index, delta) {
	if (index === -1) {
		return delta > 0 ? 0 : count - 1;
	}
	// JS "%" keeps the dividend's sign, so a plain (index + delta) % count can
	// come out negative; the extra "+ count) % count" normalizes it back into
	// [0, count).
	return ((index + delta) % count + count) % count;
}

/**
 * unsavedReplyBody builds POST /draft's JSON body for text the owner typed
 * into a reply box but has not saved with Enter (bug fix: the hint under
 * every box reads "Saved as a draft. Cmd+Enter sends.", but a draft saved
 * only on Enter, so typing then pressing Cmd+Enter sent nothing and showed
 * "Nothing to send."). sendBatch saves this body before it sends. Null when
 * the element is not a reply box or holds no text.
 *
 * @param {{dataset?: {draftTicket?: string, draftQuestion?: string}, value?: unknown} | null | undefined} el
 * @returns {{ticket: number, question: number | null, text: string} | null}
 */
export function unsavedReplyBody(el) {
	const ticket = el?.dataset?.draftTicket;
	const question = el?.dataset?.draftQuestion;
	if (!ticket || typeof el.value !== 'string' || el.value === '') {
		return null;
	}
	return { ticket: Number(ticket), question: question ? Number(question) : null, text: el.value };
}

/**
 * unsavedReplyBodies maps unsavedReplyBody over every given reply input,
 * keeping only the ones that hold unsaved text (bug fix, Q11: the send chord
 * saved only document.activeElement, through unsavedReplyBody, so a reply
 * box that did not have focus never reached POST /draft and its text was
 * silently left behind even though the send reported success). console.js's
 * postSendBatch calls this over every "#main .reply-input", not just the
 * focused one, so every box with text is saved before /send runs. Each
 * result keeps el beside body so the caller can report or keep the text for
 * whichever box a save fails against.
 *
 * @param {Iterable<{dataset?: {draftTicket?: string, draftQuestion?: string}, value?: unknown}>|null|undefined} inputs
 * @returns {{el: object, body: {ticket: number, question: number|null, text: string}}[]}
 */
export function unsavedReplyBodies(inputs) {
	const out = [];
	for (const el of inputs ?? []) {
		const body = unsavedReplyBody(el);
		if (body) {
			out.push({ el, body });
		}
	}
	return out;
}

/**
 * sendResultWithUnsent appends a sentence naming any reply box the send left
 * unsent, and any box whose failed save still sent an earlier autosaved
 * version, to showSendResult's text (bug fix, Q11: a save failure used to be
 * silent, with the send still reporting plain success). text is returned
 * unchanged when both unsent and stale are 0. Any trailing space on text is
 * trimmed before appending, so the joined sentence never ends up with two
 * spaces between.
 *
 * stale counts a failed-to-save box differently from unsent (bug fix): when
 * an earlier autosave of the same box already reached the store, /send still
 * sends that older draft, so "not sent" would be wrong -- something did go
 * out, just not the latest edit.
 *
 * @param {string} text - the base result line, e.g. "Sent 1 message."
 * @param {number} unsent - how many reply boxes failed to save, with nothing
 *   earlier ever saved for them
 * @param {number} [stale] - how many failed-to-save boxes still sent an
 *   earlier autosaved version
 * @returns {string}
 */
export function sendResultWithUnsent(text, unsent, stale = 0) {
	if (unsent === 0 && stale === 0) {
		return text;
	}
	let base = text.trimEnd();
	if (unsent === 1) {
		base = `${base} 1 reply not sent; its text is still in its box.`;
	} else if (unsent > 1) {
		base = `${base} ${unsent} replies not sent; their text is still in their boxes.`;
	}
	if (stale === 1) {
		base = `${base} 1 reply sent an earlier version; its newest edit may be missing.`;
	} else if (stale > 1) {
		base = `${base} ${stale} replies sent an earlier version; their newest edits may be missing.`;
	}
	return base;
}

// AUTOSAVE_DEBOUNCE_MS is how long installReplyAutosave (console.js) waits
// after the owner's last keystroke in a reply box before posting it as a
// draft (design: "A Reply box autosaves one second after the owner stops
// typing"). 1000ms is long enough that ordinary typing never fires a save
// per keystroke, short enough that a blur, a morph, or a closed tab shortly
// after typing stops rarely beats it.
export const AUTOSAVE_DEBOUNCE_MS = 1000;

/**
 * replyAutosaveBody builds POST /draft's JSON body for installReplyAutosave's
 * debounced save (design: "Autosave the reply box as a draft, debounced on
 * input"), given the box and the text last posted for it. Unlike
 * unsavedReplyBody, an empty value still yields a body -- with text '' --
 * so emptying a box that had a saved draft clears it (SaveDraft's new
 * "draft cleared" path) instead of leaving a stale draft the box no longer
 * shows. Null when el is not a question-targeted reply box (both
 * data-draft-ticket and data-draft-question are required: autosave only
 * covers the per-question reply box this ticket adds it to), el.value is
 * not a string, or el.value equals lastSavedText (nothing changed since the
 * last save, so there is nothing to post).
 *
 * @param {{dataset?: {draftTicket?: string, draftQuestion?: string}, value?: unknown} | null | undefined} el
 * @param {string} lastSavedText
 * @returns {{ticket: number, question: number, text: string} | null}
 */
export function replyAutosaveBody(el, lastSavedText) {
	const ticket = el?.dataset?.draftTicket;
	const question = el?.dataset?.draftQuestion;
	const isQuestionReplyBox = Boolean(ticket && question) && typeof el?.value === 'string';
	if (!isQuestionReplyBox) {
		return null;
	}
	const unchanged = el.value === lastSavedText;
	if (unchanged) {
		return null;
	}
	return { ticket: Number(ticket), question: Number(question), text: el.value };
}

/**
 * emptiedReplyBodies finds every input the owner emptied since its last save
 * (console.js's postSendBatch, bug fix: canceling a pending autosave timer
 * outright, rather than flushing it, left an emptied box's old, already-saved
 * text as the ticket's draft, and /send still sent it). lastSavedFor(el)
 * supplies the text last known saved for el, the same lookup
 * installReplyAutosave's own fireReplyAutosave uses, so a box nobody has
 * typed in since page load is not reported as needing a flush.
 *
 * @param {Iterable<{dataset?: {draftTicket?: string, draftQuestion?: string}, value?: unknown}>|null|undefined} inputs
 * @param {(el: object) => string} lastSavedFor
 * @returns {{el: object, body: {ticket: number, question: number, text: string}}[]}
 */
export function emptiedReplyBodies(inputs, lastSavedFor) {
	const out = [];
	for (const el of inputs ?? []) {
		if (el?.value !== '') {
			continue;
		}
		const body = replyAutosaveBody(el, lastSavedFor(el));
		if (body) {
			out.push({ el, body });
		}
	}
	return out;
}

/**
 * clearFailedResult is showSendResult's text when postSendBatch holds /send
 * back because at least one emptied box's clear failed to save (bug fix: a
 * clear that failed leaves its old, deleted text saved as the ticket's
 * draft, and sending anyway would silently resend text the owner just
 * emptied the box of), naming how many emptied boxes' clears failed.
 *
 * @param {number} count
 * @returns {string}
 */
export function clearFailedResult(count) {
	return count === 1
		? 'A deleted reply could not be cleared from the server, so sending was canceled. Try again.'
		: `${count} deleted replies could not be cleared from the server, so sending was canceled. Try again.`;
}

/**
 * partitionFailedSaves splits the reply boxes whose save failed at send time
 * into failed (nothing of theirs ever reached the store) and stale (an
 * earlier autosave already did, so /send still sends that older draft) (bug
 * fix: reporting a stale box as plain "not sent" told the owner nothing went
 * out when an earlier edit actually had). lastSavedFor(el) must use the same
 * fallback to el.defaultValue that fireReplyAutosave does, so a box whose
 * only save ever was the server-rendered draft (never autosaved this page
 * session) is still counted as stale rather than failed.
 *
 * A box whose earlier saved text already equals what this failed save was
 * trying to post is left out of both lists (bug fix): that save's own
 * failure changed nothing, since the exact text it would have written is
 * already the one /send is about to read. Counting it as stale would wrongly
 * warn that its "newest edit may be missing" when no edit was lost, and
 * postSendBatch would then also leave its text sitting in the box after a
 * send that in fact carried it.
 *
 * @param {{el: object, body: {question: number|null, text: string}}[]} pending
 * @param {boolean[]} results - postDraftRequest's outcome, by the same index as pending
 * @param {(el: object) => string} lastSavedFor
 * @returns {{failed: object[], stale: object[]}}
 */
export function partitionFailedSaves(pending, results, lastSavedFor) {
	const failed = [];
	const stale = [];
	pending.forEach(({ el, body }, i) => {
		if (results[i]) {
			return;
		}
		const saved = body.question != null ? lastSavedFor(el) : '';
		if (saved === body.text) {
			return;
		}
		if (saved) {
			stale.push(el);
		} else {
			failed.push(el);
		}
	});
	return { failed, stale };
}

/**
 * replyFocusSnapshot captures a reply box's identity, text, and selection at
 * the moment of a focusin, input, or select event (design: "Snapshot
 * document.activeElement ... and its selection before patch work"), so
 * console.js's restoreReplyFocus can give it back after a /stream patch
 * blurs or replaces the node. ticket and question are kept as the dataset's
 * own strings, not coerced to numbers, since they are only ever used again
 * to rebuild the same CSS attribute selector that found this box, never sent
 * over the wire. Null when el is not a question-targeted reply box.
 *
 * @param {{dataset?: {draftTicket?: string, draftQuestion?: string}, value?: unknown, selectionStart?: number, selectionEnd?: number} | null | undefined} el
 * @returns {{ticket: string, question: string, value: string, start: number, end: number} | null}
 */
export function replyFocusSnapshot(el) {
	const ticket = el?.dataset?.draftTicket;
	const question = el?.dataset?.draftQuestion;
	if (!ticket || !question) {
		return null;
	}
	return {
		ticket,
		question,
		value: typeof el.value === 'string' ? el.value : '',
		start: el.selectionStart ?? 0,
		end: el.selectionEnd ?? 0,
	};
}

/**
 * restoreFocusDecision decides whether runPatchWork should give focus back
 * to the reply box named by snapshot, and whether to refill its text first
 * (design: "If the morph blurred it, restore focus and selection in
 * runPatchWork"). active.isBody tells apart a patch-caused blur -- the morph
 * leaves nothing focused, so document.activeElement falls back to
 * document.body -- from a deliberate one (Esc, a click on some other real
 * element), which must never be undone by an unrelated later patch. focus is
 * true only when snapshot is non-null, active.isBody is true, and target
 * (the box for the same ticket and question, or null if the patch rendered
 * none) is non-null.
 *
 * restoreValue is true whenever focus is true and target's value differs
 * from snapshot's at all, not only when target came up empty (bug fix: the
 * replacement node's value comes from the server-rendered draftReply, which
 * is only ever as fresh as the last autosave -- up to AUTOSAVE_DEBOUNCE_MS
 * behind, or more while a save is in flight. snapshot.value is refreshed on
 * every input event, so it is always the newest text regardless of what the
 * replacement shows; trusting a non-empty-but-stale replacement instead
 * silently lost whatever the owner typed since the last save).
 *
 * @param {{ticket: string, question: string, value: string, start: number, end: number} | null} snapshot
 * @param {{isBody: boolean}} active
 * @param {{value: string} | null} target
 * @returns {{focus: boolean, restoreValue: boolean}}
 */
export function restoreFocusDecision(snapshot, active, target) {
	const focus = Boolean(snapshot) && Boolean(active?.isBody) && target != null;
	const staleReplacement = focus && target.value !== snapshot.value;
	return { focus, restoreValue: staleReplacement };
}

/**
 * buildChipDraftBody builds POST /draft's JSON body for an option chip's
 * activation (design section 6.6, 6.7, code review fix 1): the chip's
 * data-draft-ticket, data-draft-question, and data-option, read off its
 * DOM dataset by console.js and passed here as plain data, matching
 * store.DraftInput's option mode (answer.go's draftRequest: {ticket,
 * question, option}).
 *
 * @param {{draftTicket?: string, draftQuestion?: string, option?: string}} dataset
 * @returns {{ticket: number, question: number, option: string}}
 */
export function buildChipDraftBody(dataset) {
	return {
		ticket: Number(dataset?.draftTicket),
		question: Number(dataset?.draftQuestion),
		option: dataset?.option ?? '',
	};
}

/**
 * buildItemDraftBody builds POST /draft's JSON body for a per-item
 * accept/reject/drop/discuss control's activation (design section 6.6, 6.7,
 * code review fix 1): the control's data-draft-ticket, data-draft-question,
 * data-item-ref, and data-decision, matching store.DraftInput's item mode
 * (answer.go's draftRequest: {ticket, question, item: {ref, decision}}).
 *
 * @param {{draftTicket?: string, draftQuestion?: string, itemRef?: string, decision?: string}} dataset
 * @returns {{ticket: number, question: number, item: {ref: string, decision: string}}}
 */
export function buildItemDraftBody(dataset) {
	return {
		ticket: Number(dataset?.draftTicket),
		question: Number(dataset?.draftQuestion),
		item: { ref: dataset?.itemRef ?? '', decision: dataset?.decision ?? '' },
	};
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

/**
 * ACTION_LABELS maps every keys.json action name (internal/console/keys.go's
 * Bindings, the closed set design section 8 names) to the plain-English
 * copy the "?" help overlay shows instead of the bare identifier
 * (console.js's buildHelpOverlay). Kept here, not in console.js, so it
 * stays Node-testable like every other piece of this module's data.
 * @type {Record<string, string>}
 */
export const ACTION_LABELS = {
	'nav-inbox': 'Go to inbox',
	'nav-recent': 'Go to recent',
	'nav-feed': 'Go to feed',
	'focus-next': 'Focus next item',
	'focus-prev': 'Focus previous item',
	open: 'Open focused item',
	up: 'Go up',
	'input-next': 'Next field',
	'input-prev': 'Previous field',
	draft: 'Save draft',
	chip: 'Pick numbered option',
	send: 'Send',
	'toggle-rail': 'Toggle rail',
	'focus-side': 'Focus side box',
	stop: 'Stop ticket',
	'stop-all': 'Stop everything',
	'mark-read': 'Mark read',
	help: 'Toggle this help',
	blur: 'Close / leave input',
};

/**
 * describeAction returns ACTION_LABELS' copy for action, or action itself
 * when it names nothing in that closed set (a future keys.go action this
 * map has not yet been given a label for), so the help overlay always shows
 * something rather than an empty row.
 *
 * @param {string} action
 * @returns {string}
 */
export function describeAction(action) {
	return ACTION_LABELS[action] ?? action;
}

// RECONNECT_BASE_MS and RECONNECT_MAX_MS bound reconnectDelay's exponential
// backoff after /stream ends (bug fix: a stream that ended for good left the
// page showing its last frame forever, with nothing re-dispatching the
// current nav to open a fresh one). 1000ms is quick enough that a one-off
// drop recovers almost at once; doubling, capped at 30000ms, keeps a server
// that is actually down from being hammered.
export const RECONNECT_BASE_MS = 1000;
export const RECONNECT_MAX_MS = 30000;

// STREAM_SETTLE_MS is how long a started /stream request must stay open
// before reduceStreamStatus treats it as live (console.js's applyStreamEvent
// schedules a "settled" event this far after "started"). A patch clears the
// stale marker sooner, but an idiomorph morph of identical content can
// produce no patch at all, so settling on time elapsed alone is the
// fallback that still clears it.
export const STREAM_SETTLE_MS = 1000;

/**
 * emptyStreamStatus is reduceStreamStatus's rest state: no /stream request
 * in flight, no backoff attempt counted, and not stale.
 * @returns {{inflight: number, gen: number, attempt: number, staleSince: number|null}}
 */
export function emptyStreamStatus() {
	return { inflight: 0, gen: 0, attempt: 0, staleSince: null };
}

/**
 * reconnectDelay returns the backoff, in milliseconds, before the attempt-th
 * reconnect: RECONNECT_BASE_MS doubled per attempt, capped at
 * RECONNECT_MAX_MS.
 * @param {number} attempt - 0 for the first reconnect after a stream ends
 * @returns {number}
 */
export function reconnectDelay(attempt) {
	return Math.min(RECONNECT_BASE_MS * 2 ** attempt, RECONNECT_MAX_MS);
}

/**
 * reduceStreamStatus is console.js's one reducer over every /stream
 * lifecycle event (started, finished, error, retrying, retries-failed,
 * reconnecting, settled, patched), driving both the reconnect backoff and
 * the "Reconnecting. Stale since HH:MM." marker (bug fix: after a stream
 * ended for good, nothing noticed, and the sidebar kept showing a frame
 * that was no longer live). inflight counts /stream requests console.js has
 * seen started but not yet finished, so a navigation's requestCancellation
 * aborting an old stream while a new one is already open does not schedule
 * a reconnect the new stream makes unnecessary. gen is bumped on every
 * started request and on every failure, so a settle timer armed for an
 * older request (event.gen) is a no-op once a newer one has started or
 * failed. staleSince latches the first failure's or reconnect wait's time
 * and holds it through repeats, so retriggering does not keep moving the
 * "Stale since" clock forward.
 *
 * @param {{inflight: number, gen: number, attempt: number, staleSince: number|null}} status
 * @param {{type: 'started'|'finished'|'error'|'retrying'|'retries-failed'|'reconnecting'|'settled'|'patched', gen?: number}} event
 * @param {number} now - Date.now()-style milliseconds
 * @returns {{status: object, effect: {settleGen: number|null, reconnectIn: number|null, cancelReconnect: boolean}}}
 */
export function reduceStreamStatus(status, event, now) {
	const none = { settleGen: null, reconnectIn: null, cancelReconnect: false };
	switch (event.type) {
		case 'started': {
			const gen = status.gen + 1;
			return {
				status: { ...status, inflight: status.inflight + 1, gen },
				effect: { ...none, settleGen: gen, cancelReconnect: true },
			};
		}
		case 'finished': {
			const inflight = Math.max(0, status.inflight - 1);
			if (inflight > 0) {
				return { status: { ...status, inflight }, effect: none };
			}
			return {
				status: { ...status, inflight, gen: status.gen + 1, attempt: status.attempt + 1 },
				effect: { ...none, reconnectIn: reconnectDelay(status.attempt) },
			};
		}
		case 'error':
		case 'retrying':
		case 'retries-failed':
			return { status: { ...status, gen: status.gen + 1, staleSince: status.staleSince ?? now }, effect: none };
		case 'reconnecting':
			return { status: { ...status, staleSince: status.staleSince ?? now }, effect: none };
		case 'settled':
		case 'patched': {
			const current = event.type === 'patched' || event.gen === status.gen;
			if (status.inflight === 0 || !current) {
				return { status, effect: none };
			}
			return { status: { ...status, attempt: 0, staleSince: null }, effect: none };
		}
		default:
			return { status, effect: none };
	}
}

/**
 * staleMarkerText renders reduceStreamStatus's staleSince as the
 * #stream-status marker's text: "" while live (staleSince null), else
 * "Reconnecting. Stale since HH:MM." with the hour and minute staleSince's
 * clock time fell on, zero-padded.
 * @param {number|null} staleSince - epoch ms, or null while live
 * @returns {string}
 */
export function staleMarkerText(staleSince) {
	if (staleSince === null) {
		return '';
	}
	const d = new Date(staleSince);
	const hh = String(d.getHours()).padStart(2, '0');
	const mm = String(d.getMinutes()).padStart(2, '0');
	return `Reconnecting. Stale since ${hh}:${mm}.`;
}
