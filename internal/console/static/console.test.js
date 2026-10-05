// console.test.js — node --test over keyboard.mjs only (design section 6.4,
// Makefile target test-js). This file is never served: the static mux
// (internal/console/server.go) allowlists five named assets, and this path
// is not one of them (design section 5, 12).
//
// Covers the chord state machine and timeout, key-to-action resolution
// from a fixture keys.json, input-context suppression, the mac/non-mac
// send chord, the id-based focus step (next, previous, from no focus,
// empty list), reconcileFocus across insertion/removal/reorder (focused
// first/middle/last row removed), collectPatchWork, the /stream
// reconnect-and-stale-marker decisions (reduceStreamStatus, reconnectDelay,
// staleMarkerText), saving every unsaved reply box on send and naming any
// left unsent or stale (unsavedReplyBodies, sendResultWithUnsent), the
// debounced autosave decision (replyAutosaveBody), the send-time emptied-box
// flush, failed-clear block, and failed/stale save split (emptiedReplyBodies,
// clearFailedResult, partitionFailedSaves), and the patch-caused-blur-only
// focus restore decision (replyFocusSnapshot, restoreFocusDecision).

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import {
	CHORD_TIMEOUT_MS,
	emptyChordState,
	advanceChord,
	resolveAction,
	isInputContext,
	isSendChord,
	sendChordToken,
	sendChordLabel,
	draftConflictMessage,
	TOAST_DISMISS_MS,
	scheduleToastDismiss,
	clearReplyInputs,
	resolveToken,
	stepFocus,
	reconcileFocus,
	collectPatchWork,
	navChanged,
	reduceNav,
	nextPendingNav,
	stepComposerIndex,
	buildChipDraftBody,
	buildItemDraftBody,
	unsavedReplyBody,
	unsavedReplyBodies,
	sendResultWithUnsent,
	AUTOSAVE_DEBOUNCE_MS,
	replyAutosaveBody,
	emptiedReplyBodies,
	clearFailedResult,
	partitionFailedSaves,
	replyFocusSnapshot,
	restoreFocusDecision,
	describeAction,
	ACTION_LABELS,
	RECONNECT_BASE_MS,
	RECONNECT_MAX_MS,
	emptyStreamStatus,
	reconnectDelay,
	reduceStreamStatus,
	staleMarkerText,
	STREAM_IDLE_MS,
	PATCH_SUPPRESS_MS,
	handleKeyEvent,
	notePatchFocus,
	patchFocus,
} from './keyboard.mjs';

// fixtureBindings is a small parsed-keys.json fixture, shaped the same as
// the real generated static/keys.json (design section 6.4: "key-to-action
// resolution from a fixture keys.json"), covering a chord, a shared-action
// multi-key row, and a plain single key.
const fixtureBindings = [
	{ keys: ['g i'], action: 'nav-inbox' },
	{ keys: ['g r'], action: 'nav-recent' },
	{ keys: ['o', 'Enter'], action: 'open' },
	{ keys: ['j'], action: 'focus-next' },
	{ keys: ['Cmd-Enter', 'Ctrl-Enter'], action: 'send' },
];

test('chord state machine: "g" then "i" resolves to the "g i" chord', () => {
	const now = 1000;
	const armed = advanceChord(emptyChordState(), 'g', now);
	assert.equal(armed.chord, null);
	assert.equal(armed.state.leader, 'g');

	const completed = advanceChord(armed.state, 'i', now + 10);
	assert.equal(completed.chord, 'g i');
	assert.deepEqual(completed.state, emptyChordState());
});

test('chord state machine: an unrelated key clears an armed chord', () => {
	const armed = advanceChord(emptyChordState(), 'g', 1000);
	const cleared = advanceChord(armed.state, 'z', 1010);
	// "g z" is not a real chord (resolveAction returns null for it), and the
	// state resets either way, matching "any other key ... clears the chord".
	assert.equal(resolveAction(cleared.chord, fixtureBindings), null);
	assert.deepEqual(cleared.state, emptyChordState());
});

test('chord state machine: the timeout clears an armed chord', () => {
	const armed = advanceChord(emptyChordState(), 'g', 1000);
	const afterTimeout = advanceChord(armed.state, 'i', 1000 + CHORD_TIMEOUT_MS + 1);
	// Too late: this "i" is treated as a fresh key, not a chord completion.
	assert.equal(afterTimeout.chord, null);
	assert.deepEqual(afterTimeout.state, emptyChordState());
});

test('chord state machine: a fresh "g" re-arms even mid-sequence', () => {
	const first = advanceChord(emptyChordState(), 'g', 1000);
	const rearmed = advanceChord(first.state, 'g', 1000 + CHORD_TIMEOUT_MS + 1);
	assert.equal(rearmed.chord, null);
	assert.equal(rearmed.state.leader, 'g');
	assert.equal(rearmed.state.armedAt, 1000 + CHORD_TIMEOUT_MS + 1);
});

test('resolveAction: a chord token resolves through the fixture', () => {
	assert.equal(resolveAction('g i', fixtureBindings), 'nav-inbox');
	assert.equal(resolveAction('g r', fixtureBindings), 'nav-recent');
});

test('resolveAction: either key of a shared-action row resolves the same action', () => {
	assert.equal(resolveAction('o', fixtureBindings), 'open');
	assert.equal(resolveAction('Enter', fixtureBindings), 'open');
	assert.equal(resolveAction('Cmd-Enter', fixtureBindings), 'send');
	assert.equal(resolveAction('Ctrl-Enter', fixtureBindings), 'send');
});

test('resolveAction: an unbound token and empty input resolve to null', () => {
	assert.equal(resolveAction('q', fixtureBindings), null);
	assert.equal(resolveAction('', fixtureBindings), null);
	assert.equal(resolveAction(null, fixtureBindings), null);
});

test('isInputContext: an input or textarea target is an input context', () => {
	assert.equal(isInputContext({ tagName: 'input' }), true);
	assert.equal(isInputContext({ tagName: 'INPUT' }), true);
	assert.equal(isInputContext({ tagName: 'textarea' }), true);
	assert.equal(isInputContext({ tagName: 'TEXTAREA' }), true);
});

test('isInputContext: a contenteditable element is an input context', () => {
	assert.equal(isInputContext({ tagName: 'div', isContentEditable: true }), true);
});

test('isInputContext: an ordinary element, or no descriptor, is not an input context', () => {
	assert.equal(isInputContext({ tagName: 'div' }), false);
	assert.equal(isInputContext({ tagName: 'a' }), false);
	assert.equal(isInputContext(null), false);
	assert.equal(isInputContext(undefined), false);
});

test('isSendChord: Cmd-Enter sends on mac, not elsewhere', () => {
	const cmdEnter = { key: 'Enter', metaKey: true, ctrlKey: false };
	assert.equal(isSendChord(cmdEnter, true), true);
	assert.equal(isSendChord(cmdEnter, false), false);
});

test('isSendChord: Ctrl-Enter sends off mac, not on mac', () => {
	const ctrlEnter = { key: 'Enter', metaKey: false, ctrlKey: true };
	assert.equal(isSendChord(ctrlEnter, false), true);
	assert.equal(isSendChord(ctrlEnter, true), false);
});

test('isSendChord: a bare Enter, or a non-Enter key with a modifier, is never the send chord', () => {
	assert.equal(isSendChord({ key: 'Enter' }, true), false);
	assert.equal(isSendChord({ key: 'a', metaKey: true }, true), false);
});

// isSendChord: an extra modifier alongside the platform's own must not
// misfire the send chord (PR #16 review, CodeRabbit console.js:567 / cubic
// console.js:565) -- an OS chord like Ctrl-Alt-Enter, or a Cmd-Shift-Enter
// someone fat-fingers reaching for something else, is not "send".

test('isSendChord: Alt held alongside the platform modifier is not the send chord', () => {
	assert.equal(isSendChord({ key: 'Enter', metaKey: true, altKey: true }, true), false);
	assert.equal(isSendChord({ key: 'Enter', ctrlKey: true, altKey: true }, false), false);
});

test('isSendChord: Shift held alongside the platform modifier is not the send chord', () => {
	assert.equal(isSendChord({ key: 'Enter', metaKey: true, shiftKey: true }, true), false);
	assert.equal(isSendChord({ key: 'Enter', ctrlKey: true, shiftKey: true }, false), false);
});

test('isSendChord: both Ctrl and Cmd held at once is not the send chord on either platform', () => {
	assert.equal(isSendChord({ key: 'Enter', metaKey: true, ctrlKey: true }, true), false);
	assert.equal(isSendChord({ key: 'Enter', metaKey: true, ctrlKey: true }, false), false);
});

test('sendChordToken names the platform-correct keys.json token', () => {
	assert.equal(sendChordToken(true), 'Cmd-Enter');
	assert.equal(sendChordToken(false), 'Ctrl-Enter');
});

// sendChordLabel (bug fix): the glyph the "saved as a draft" hints show,
// distinct from sendChordToken's keys.json binding string.
test('sendChordLabel renders the platform-correct glyph', () => {
	assert.equal(sendChordLabel(true), '⌘+Enter');
	assert.equal(sendChordLabel(false), 'Ctrl+Enter');
});

// draftConflictMessage (bug fix): a closed-question 409 beside the reply
// box that triggered it reads as a sentence, not console_writes.go's raw
// conflict reason.
test('draftConflictMessage: "question closed" reads as a plain sentence', () => {
	assert.equal(draftConflictMessage('question closed'), 'This question is already answered.');
});

test('draftConflictMessage: any other reason is shown as-is', () => {
	assert.equal(draftConflictMessage('missing option'), 'missing option');
	assert.equal(draftConflictMessage('ambiguous draft mode'), 'ambiguous draft mode');
});

// clearReplyInputs (design section 22.7): a 200 from /send clears every
// .reply-input inside #main, so a sent draft's own box does not keep
// showing text the owner just sent.
test('clearReplyInputs: a 200 from /send clears the reply inputs', () => {
	const inputs = [{ value: 'already sent text' }, { value: 'another box' }];
	clearReplyInputs(inputs);
	assert.deepEqual(
		inputs.map((i) => i.value),
		['', ''],
	);
});

test('clearReplyInputs: no inputs is a no-op', () => {
	assert.doesNotThrow(() => clearReplyInputs([]));
});

// scheduleToastDismiss (bug fix 12): the bottom "Sent N answer(s)."/"Nothing
// to send." toast never auto-dismissed, and two sends close together could
// otherwise arm two independent dismiss timers that race each other.
test('scheduleToastDismiss: no previous timer, just arms the next one', () => {
	const cleared = [];
	const id = scheduleToastDismiss(null, () => 42, (timerID) => cleared.push(timerID));
	assert.equal(id, 42);
	assert.deepEqual(cleared, []);
});

test('scheduleToastDismiss: a pending timer is cleared before the next one arms', () => {
	const cleared = [];
	const id = scheduleToastDismiss(7, () => 8, (timerID) => cleared.push(timerID));
	assert.equal(id, 8);
	assert.deepEqual(cleared, [7]);
});

test('TOAST_DISMISS_MS is about 4 seconds', () => {
	assert.equal(TOAST_DISMISS_MS, 4000);
});

// resolveToken: outside an input, a single key held with Ctrl, Meta, or Alt
// must not resolve to a bare action token, or Ctrl-1 picks a chip and blocks
// the browser's own Ctrl-1 (tab switch); Cmd-Enter/Ctrl-Enter, the one
// modifier-bearing binding this app defines, must still resolve to the send
// chord regardless (PR review fix).

test('resolveToken: Ctrl+1 outside an input does not resolve to a token', () => {
	assert.equal(resolveToken({ key: '1', ctrlKey: true }, false, false), null);
});

test('resolveToken: Cmd+1 and Alt+1 outside an input do not resolve to a token either', () => {
	assert.equal(resolveToken({ key: '1', metaKey: true }, false, false), null);
	assert.equal(resolveToken({ key: '1', altKey: true }, false, false), null);
});

test('resolveToken: Cmd-Enter still resolves to the send chord on mac', () => {
	assert.equal(resolveToken({ key: 'Enter', metaKey: true }, false, true), 'Cmd-Enter');
});

test('resolveToken: Ctrl-Enter still resolves to the send chord off mac', () => {
	assert.equal(resolveToken({ key: 'Enter', ctrlKey: true }, false, false), 'Ctrl-Enter');
});

test('resolveToken: plain "1" outside an input still resolves to itself', () => {
	assert.equal(resolveToken({ key: '1' }, false, false), '1');
});

test('resolveToken: a modifier-free chord leader ("g") outside an input still resolves', () => {
	assert.equal(resolveToken({ key: 'g' }, false, false), 'g');
});

test('resolveToken: Shift alone does not suppress a single-key token', () => {
	assert.equal(resolveToken({ key: '?', shiftKey: true }, false, false), '?');
});

test('stepFocus: from no focus, next lands on the first id and prev on the last', () => {
	const ids = ['ticket:1', 'ticket:2', 'ticket:3'];
	assert.equal(stepFocus(ids, '', 'next'), 'ticket:1');
	assert.equal(stepFocus(ids, '', 'prev'), 'ticket:3');
});

test('stepFocus: next and previous move by one from the current id', () => {
	const ids = ['ticket:1', 'ticket:2', 'ticket:3'];
	assert.equal(stepFocus(ids, 'ticket:1', 'next'), 'ticket:2');
	assert.equal(stepFocus(ids, 'ticket:2', 'next'), 'ticket:3');
	assert.equal(stepFocus(ids, 'ticket:3', 'prev'), 'ticket:2');
	assert.equal(stepFocus(ids, 'ticket:2', 'prev'), 'ticket:1');
});

test('stepFocus: stepping past either end holds at that end', () => {
	const ids = ['ticket:1', 'ticket:2', 'ticket:3'];
	assert.equal(stepFocus(ids, 'ticket:3', 'next'), 'ticket:3');
	assert.equal(stepFocus(ids, 'ticket:1', 'prev'), 'ticket:1');
});

test('stepFocus: an empty list returns no focus', () => {
	assert.equal(stepFocus([], '', 'next'), '');
	assert.equal(stepFocus([], 'ticket:1', 'next'), '');
});

test('stepFocus: a stale focusedID absent from ids falls back like no focus', () => {
	const ids = ['ticket:1', 'ticket:2'];
	assert.equal(stepFocus(ids, 'ticket:99', 'next'), 'ticket:1');
	assert.equal(stepFocus(ids, 'ticket:99', 'prev'), 'ticket:2');
});

test('reconcileFocus: an id still present is returned unchanged', () => {
	const previous = ['a', 'b', 'c'];
	const current = ['a', 'b', 'c'];
	assert.equal(reconcileFocus(previous, current, 'b'), 'b');
});

test('reconcileFocus: an insertion ahead of the focused row leaves it unchanged', () => {
	const previous = ['a', 'b'];
	const current = ['z', 'a', 'b'];
	assert.equal(reconcileFocus(previous, current, 'b'), 'b');
});

test('reconcileFocus: the focused first row removed falls to the row that took its place', () => {
	const previous = ['a', 'b', 'c'];
	const current = ['b', 'c'];
	assert.equal(reconcileFocus(previous, current, 'a'), 'b');
});

test('reconcileFocus: the focused middle row removed falls to the row that took its place', () => {
	const previous = ['a', 'b', 'c'];
	const current = ['a', 'c'];
	assert.equal(reconcileFocus(previous, current, 'b'), 'c');
});

test('reconcileFocus: the focused last row removed falls to the row before it', () => {
	const previous = ['a', 'b', 'c'];
	const current = ['a', 'b'];
	assert.equal(reconcileFocus(previous, current, 'c'), 'b');
});

test('reconcileFocus: removing the only row leaves no focus', () => {
	const previous = ['a'];
	const current = [];
	assert.equal(reconcileFocus(previous, current, 'a'), '');
});

test('reconcileFocus: a reorder that keeps the focused id present leaves it unchanged', () => {
	const previous = ['a', 'b', 'c'];
	const current = ['c', 'a', 'b'];
	assert.equal(reconcileFocus(previous, current, 'a'), 'a');
});

test('reconcileFocus: no focus, or an id unknown even to previousIDs, resolves to no focus', () => {
	assert.equal(reconcileFocus(['a'], ['a'], ''), '');
	assert.equal(reconcileFocus(['a'], ['a', 'b'], 'ghost'), '');
});

test('collectPatchWork: passes diagram ids through and reconciles focus', () => {
	const descriptors = {
		diagramIDs: ['diagram:1', 'diagram:2'],
		focusableIDs: ['a', 'c'],
		previousFocusableIDs: ['a', 'b', 'c'],
	};
	const work = collectPatchWork(descriptors, 'b');
	assert.deepEqual(work.diagramIDs, ['diagram:1', 'diagram:2']);
	assert.equal(work.focusID, 'c'); // b removed; c took its ordinal
});

test('collectPatchWork: an unchanged focusable list keeps the same focus and empty diagram work', () => {
	const descriptors = {
		diagramIDs: [],
		focusableIDs: ['a', 'b'],
		previousFocusableIDs: ['a', 'b'],
	};
	const work = collectPatchWork(descriptors, 'b');
	assert.deepEqual(work.diagramIDs, []);
	assert.equal(work.focusID, 'b');
});

test('collectPatchWork: missing descriptor fields default to empty', () => {
	const work = collectPatchWork({}, '');
	assert.deepEqual(work, { diagramIDs: [], focusID: '' });
});

// navChanged: console.js's navigate() only pushes the back stack and clears
// focus on a real destination change (code review fix 4).

test('navChanged: a differing view, open, or project each count as changed', () => {
	const current = { view: 'inbox', open: 0, project: 0 };
	assert.equal(navChanged(current, { view: 'thread', open: 0, project: 0 }), true);
	assert.equal(navChanged(current, { view: 'inbox', open: 5, project: 0 }), true);
	assert.equal(navChanged(current, { view: 'inbox', open: 0, project: 5 }), true);
});

test('navChanged: an identical destination is not a change', () => {
	const current = { view: 'thread', open: 7, project: 0 };
	assert.equal(navChanged(current, { view: 'thread', open: 7, project: 0 }), false);
});

// reduceNav: console.js's onZingNav runs every zing-nav event -- console.js's
// own keyboard-triggered dispatch and nav.templ's click-triggered dispatch
// alike -- through this one reducer (PR #16 review, CodeRabbit
// console.js:315 / cubic console.js:57), so state.nav and the back stack
// update the same way regardless of what triggered the navigation.

test('reduceNav: a real destination change updates nav, pushes history, and reports changed', () => {
	const current = { view: 'inbox', open: 0, project: 0 };
	const detail = { view: 'thread', open: 7, project: 0 };
	const result = reduceNav(current, [], detail);
	assert.deepEqual(result.nav, detail);
	assert.deepEqual(result.history, [current]);
	assert.equal(result.changed, true);
});

test('reduceNav: a click-triggered event (no isBack) updates nav the same way a keyboard one does', () => {
	// nav.templ's zingNavExpr links dispatch zing-nav with only view/open/
	// project in the detail -- no isBack field at all -- which is exactly
	// what previously never reached console.js's local nav mirror.
	const current = { view: 'inbox', open: 0, project: 0 };
	const detail = { view: 'project', open: 0, project: 3 };
	const result = reduceNav(current, [], detail);
	assert.deepEqual(result.nav, detail);
	assert.deepEqual(result.history, [current]);
});

test('reduceNav: a no-op navigation does not push history', () => {
	const current = { view: 'thread', open: 7, project: 0 };
	const result = reduceNav(current, [{ view: 'inbox', open: 0, project: 0 }], { view: 'thread', open: 7, project: 0 });
	assert.deepEqual(result.history, [{ view: 'inbox', open: 0, project: 0 }]);
	assert.equal(result.changed, false);
});

test('reduceNav: a back navigation (isBack) updates nav without pushing another history entry', () => {
	const current = { view: 'thread', open: 7, project: 0 };
	const history = [{ view: 'inbox', open: 0, project: 0 }];
	const result = reduceNav(current, history, { view: 'inbox', open: 0, project: 0, isBack: true });
	assert.deepEqual(result.nav, { view: 'inbox', open: 0, project: 0 });
	assert.deepEqual(result.history, history);
	assert.equal(result.changed, true);
});

// nextPendingNav: console.js's onZingNav calls this after reduceNav on
// every zing-nav event (bug fix: the first Threads-sidebar click right
// after a page load did nothing because it could race GET /stream's first
// frame, the only proof Datastar's own listener is wired up).

test('nextPendingNav: before the stream connects, the event becomes the pending nav to re-apply', () => {
	const nav = { view: 'thread', open: 7, project: 0 };
	assert.deepEqual(nextPendingNav(false, nav), { view: 'thread', open: 7, project: 0 });
});

test('nextPendingNav: once the stream has connected, there is nothing to remember', () => {
	const nav = { view: 'thread', open: 7, project: 0 };
	assert.equal(nextPendingNav(true, nav), null);
});

// stepComposerIndex: console.js's moveComposerFocus() over the composer's
// own controls (code review fix 3).

test('stepComposerIndex: from no focus, Tab goes to the first control and Shift-Tab to the last', () => {
	assert.equal(stepComposerIndex(3, -1, 1), 0);
	assert.equal(stepComposerIndex(3, -1, -1), 2);
});

test('stepComposerIndex: steps by one from the current index', () => {
	assert.equal(stepComposerIndex(3, 0, 1), 1);
	assert.equal(stepComposerIndex(3, 1, 1), 2);
	assert.equal(stepComposerIndex(3, 1, -1), 0);
});

test('stepComposerIndex: wraps past either end', () => {
	assert.equal(stepComposerIndex(3, 2, 1), 0);
	assert.equal(stepComposerIndex(3, 0, -1), 2);
});

// buildChipDraftBody / buildItemDraftBody: the /draft POST body a chip or
// item-decision activation builds from its element's dataset (code review
// fix 1). These are what installChipActivation (console.js) hands to
// postJSON, so this is the "pick then send" path's pure-logic coverage: a
// wrong body here means the composer queues nothing, the same way the
// bug shipped (chip.click() toggled "picked" and never posted at all).

test('buildChipDraftBody: reads ticket, question, and option off the dataset', () => {
	const dataset = { draftTicket: '12', draftQuestion: '34', option: 'b' };
	assert.deepEqual(buildChipDraftBody(dataset), { ticket: 12, question: 34, option: 'b' });
});

test('buildItemDraftBody: reads ticket, question, item ref, and decision off the dataset', () => {
	const dataset = { draftTicket: '12', draftQuestion: '34', itemRef: 'src/main.go', decision: 'accept' };
	assert.deepEqual(buildItemDraftBody(dataset), {
		ticket: 12,
		question: 34,
		item: { ref: 'src/main.go', decision: 'accept' },
	});
});

// describeAction / ACTION_LABELS: the "?" help overlay's copy for a raw
// keys.json action name (console.js's buildHelpOverlay used to render the
// bare identifier, e.g. "nav-inbox" or "toggle-rail", straight into the
// overlay). allKeysGoActions mirrors internal/console/keys.go's Bindings()
// action column one for one; keys_test.go already pins that file's own
// shape (TestBindingsCoverEvery14KeyExactlyOnce and its neighbors), so this
// list only needs to be kept in sync by hand when a future action is added
// there, which the completeness test below catches.

const allKeysGoActions = [
	'nav-inbox',
	'nav-recent',
	'nav-feed',
	'nav-project',
	'focus-next',
	'focus-prev',
	'open',
	'up',
	'input-next',
	'input-prev',
	'draft',
	'chip',
	'send',
	'toggle-rail',
	'focus-side',
	'help',
	'blur',
];

test('ACTION_LABELS has a human label for every keys.go action', () => {
	for (const action of allKeysGoActions) {
		assert.ok(
			Object.hasOwn(ACTION_LABELS, action),
			`ACTION_LABELS is missing a label for action ${JSON.stringify(action)}`,
		);
		assert.notEqual(ACTION_LABELS[action], action, `ACTION_LABELS[${JSON.stringify(action)}] just repeats the raw identifier`);
	}
});

test('describeAction returns the mapped label for a known action', () => {
	assert.equal(describeAction('nav-inbox'), 'Go to inbox');
});

test('describeAction falls back to the raw action name for one outside ACTION_LABELS', () => {
	assert.equal(describeAction('some-future-action'), 'some-future-action');
});

// ACTION_LABELS names no action outside keys.go: the inverse of "has a
// human label for every keys.go action" above. Without this, a removed
// binding's label (e.g. stop, stop-all, mark-read) could linger in
// ACTION_LABELS and advertise a key the console no longer accepts.
test('ACTION_LABELS names no action outside keys.go', () => {
	for (const action of Object.keys(ACTION_LABELS)) {
		assert.ok(allKeysGoActions.includes(action), `ACTION_LABELS has a label for ${JSON.stringify(action)}, which is not a keys.go action`);
	}
});

// unsavedReplyBody: Cmd+Enter saves the focused reply box's typed text
// before it sends, so typing then sending without Enter still sends.

test('unsavedReplyBody: a reply box with text yields its draft body', () => {
	const el = { dataset: { draftTicket: '7', draftQuestion: '9' }, value: 'why?' };
	assert.deepEqual(unsavedReplyBody(el), { ticket: 7, question: 9, text: 'why?' });
});

test('unsavedReplyBody: an empty box, a non-reply element, or nothing focused yields null', () => {
	assert.equal(unsavedReplyBody({ dataset: { draftTicket: '7', draftQuestion: '9' }, value: '' }), null);
	assert.equal(unsavedReplyBody({ dataset: {}, value: 'text' }), null);
	assert.equal(unsavedReplyBody(null), null);
});

// unsavedReplyBodies: a box holding text is saved on send even when focus is
// elsewhere (bug fix, Q11: Cmd+Enter saved only document.activeElement, so a
// 459-character note in a reply box that did not have focus never reached
// POST /draft, and the send still reported "Sent 1 message.").
test('unsavedReplyBodies: a box holding text is saved on send even when focus is elsewhere', () => {
	const noteBox = { dataset: { draftTicket: '18', draftQuestion: '11' }, value: 'x'.repeat(459) };
	const emptyBox = { dataset: { draftTicket: '18', draftQuestion: '12' }, value: '' };
	const notAReplyBox = { dataset: {}, value: 'x' };
	assert.deepEqual(unsavedReplyBodies([noteBox, emptyBox, notAReplyBox]), [
		{ el: noteBox, body: { ticket: 18, question: 11, text: 'x'.repeat(459) } },
	]);
	// With focus on the body rather than the box, the old focused-only path
	// (unsavedReplyBody(document.activeElement)) found nothing to save.
	assert.equal(unsavedReplyBody({ dataset: {} }), null);
});

test('unsavedReplyBodies: no inputs yields an empty list', () => {
	assert.deepEqual(unsavedReplyBodies([]), []);
	assert.deepEqual(unsavedReplyBodies(undefined), []);
});

// sendResultWithUnsent: the send result line names any reply left unsent so
// the owner is told rather than finding out when the text is simply gone.
test('sendResultWithUnsent: a clean send is unchanged', () => {
	assert.equal(sendResultWithUnsent('Sent 1 message.', 0), 'Sent 1 message.');
});

test('sendResultWithUnsent: one unsent reply gets the singular sentence', () => {
	assert.equal(
		sendResultWithUnsent('Sent 1 message.', 1),
		'Sent 1 message. 1 reply not sent; its text is still in its box.',
	);
});

test('sendResultWithUnsent: more than one unsent reply gets the plural sentence', () => {
	assert.equal(
		sendResultWithUnsent('Sent 1 message.', 2),
		'Sent 1 message. 2 replies not sent; their text is still in their boxes.',
	);
});

test('sendResultWithUnsent: a trailing space on text is trimmed before appending', () => {
	assert.equal(
		sendResultWithUnsent('Sent 1 message. ', 1),
		'Sent 1 message. 1 reply not sent; its text is still in its box.',
	);
});

// sendResultWithUnsent's stale count: a box whose save failed at send time
// but had an earlier autosave already in the store is not "unsent" -- an
// older version did go out (bug fix, r2f9: reporting it as plain "not sent"
// told the owner nothing went out when an earlier edit actually had).

test('sendResultWithUnsent: a stale-sent reply gets its own sentence, distinct from unsent', () => {
	assert.equal(
		sendResultWithUnsent('Sent 1 message.', 0, 1),
		'Sent 1 message. 1 reply sent an earlier version; its newest edit may be missing.',
	);
});

test('sendResultWithUnsent: more than one stale-sent reply gets the plural sentence', () => {
	assert.equal(
		sendResultWithUnsent('Sent 1 message.', 0, 2),
		'Sent 1 message. 2 replies sent an earlier version; their newest edits may be missing.',
	);
});

test('sendResultWithUnsent: unsent and stale both append, unsent first', () => {
	assert.equal(
		sendResultWithUnsent('Sent 1 message.', 1, 1),
		'Sent 1 message. 1 reply not sent; its text is still in its box. 1 reply sent an earlier version; its newest edit may be missing.',
	);
});

// replyAutosaveBody: installReplyAutosave (console.js) debounces on 'input'
// and posts this body a second after the owner stops typing, so a box's text
// is never lost to a lost focus or an unmorphed send -- including an emptied
// box, which clears a previously saved draft.

test('replyAutosaveBody: changed text yields a body to post', () => {
	const el = { dataset: { draftTicket: '18', draftQuestion: '11' }, value: 'hello' };
	assert.deepEqual(replyAutosaveBody(el, ''), { ticket: 18, question: 11, text: 'hello' });
});

test('replyAutosaveBody: text unchanged from lastSavedText yields null', () => {
	const el = { dataset: { draftTicket: '18', draftQuestion: '11' }, value: 'hello' };
	assert.equal(replyAutosaveBody(el, 'hello'), null);
});

test('replyAutosaveBody: emptying a box that had saved text still yields a body, to clear the draft', () => {
	const el = { dataset: { draftTicket: '18', draftQuestion: '11' }, value: '' };
	assert.deepEqual(replyAutosaveBody(el, 'hello'), { ticket: 18, question: 11, text: '' });
});

test('replyAutosaveBody: a missing data-draft-question yields null', () => {
	const el = { dataset: { draftTicket: '18' }, value: 'hello' };
	assert.equal(replyAutosaveBody(el, ''), null);
});

test('AUTOSAVE_DEBOUNCE_MS is a reasonable debounce window', () => {
	assert.ok(AUTOSAVE_DEBOUNCE_MS >= 500 && AUTOSAVE_DEBOUNCE_MS <= 2000);
});

// emptiedReplyBodies: postSendBatch flushes an emptied box's clear through
// /draft before /send runs, or the store's last-saved draft still goes out
// (bug fix, Q3).

test('emptiedReplyBodies: an emptied box with an earlier saved draft gets flushed', () => {
	const emptied = { dataset: { draftTicket: '18', draftQuestion: '11' }, value: '' };
	const neverSaved = { dataset: { draftTicket: '18', draftQuestion: '12' }, value: '' };
	const stillTyped = { dataset: { draftTicket: '18', draftQuestion: '13' }, value: 'still here' };
	const lastSavedFor = (el) => (el === emptied ? 'an earlier note' : '');
	assert.deepEqual(emptiedReplyBodies([emptied, neverSaved, stillTyped], lastSavedFor), [
		{ el: emptied, body: { ticket: 18, question: 11, text: '' } },
	]);
});

test('emptiedReplyBodies: no inputs yields an empty list', () => {
	assert.deepEqual(emptiedReplyBodies([], () => ''), []);
	assert.deepEqual(emptiedReplyBodies(undefined, () => ''), []);
});

// clearFailedResult: a clear that failed leaves its old, deleted text saved
// as the ticket's draft, so sending anyway would silently resend text the
// owner just emptied the box of (bug fix).

test('clearFailedResult: names how many clears failed', () => {
	assert.equal(
		clearFailedResult(1),
		'A deleted reply could not be cleared from the server, so sending was canceled. Try again.',
	);
	assert.equal(
		clearFailedResult(2),
		'2 deleted replies could not be cleared from the server, so sending was canceled. Try again.',
	);
});

// partitionFailedSaves: a box whose save failed at send time but had an
// earlier autosave already in the store (lastSavedFor non-empty) still sent
// that older draft -- it is stale, not simply unsent (bug fix).

test('partitionFailedSaves: a failed save with earlier saved text is stale, one with none is unsent', () => {
	const staleBox = { dataset: {}, value: 'new edit' };
	const unsentBox = { dataset: {}, value: 'never saved' };
	const sentBox = { dataset: {}, value: 'saved fine' };
	const pending = [
		{ el: staleBox, body: { ticket: 1, question: 11, text: 'new edit' } },
		{ el: unsentBox, body: { ticket: 1, question: 12, text: 'never saved' } },
		{ el: sentBox, body: { ticket: 1, question: 13, text: 'saved fine' } },
	];
	const results = [false, false, true];
	const lastSavedFor = (el) => (el === staleBox ? 'an earlier version' : '');
	assert.deepEqual(partitionFailedSaves(pending, results, lastSavedFor), {
		failed: [unsentBox],
		stale: [staleBox],
	});
});

test('partitionFailedSaves: a thread-level reply (no question) with a failed save is always unsent, never stale', () => {
	const threadBox = { dataset: {}, value: 'a thread reply' };
	const pending = [{ el: threadBox, body: { ticket: 1, question: null, text: 'a thread reply' } }];
	const result = partitionFailedSaves(pending, [false], () => 'something saved earlier');
	assert.deepEqual(result, { failed: [threadBox], stale: [] });
});

test('partitionFailedSaves: a failed save whose saved text already matches is neither unsent nor stale', () => {
	const alreadySavedBox = { dataset: {}, value: 'unchanged' };
	const pending = [{ el: alreadySavedBox, body: { ticket: 1, question: 11, text: 'unchanged' } }];
	const lastSavedFor = () => 'unchanged';
	assert.deepEqual(partitionFailedSaves(pending, [false], lastSavedFor), { failed: [], stale: [] });
});

// replyFocusSnapshot / restoreFocusDecision: a /stream patch that blurs or
// replaces the reply box the owner is typing into (the morph swapping the
// node, bug fix: "letters run as shortcuts" once focus silently lands on
// body) must restore it, but a deliberate blur (Esc, a click elsewhere)
// must not be undone by the next unrelated patch.

test("replyFocusSnapshot: reads a reply box's ticket, question, value, and selection", () => {
	const el = { dataset: { draftTicket: '18', draftQuestion: '11' }, value: 'hello', selectionStart: 1, selectionEnd: 3 };
	assert.deepEqual(replyFocusSnapshot(el), { ticket: '18', question: '11', value: 'hello', start: 1, end: 3 });
});

test('replyFocusSnapshot: a non-reply element, or nothing, yields null', () => {
	assert.equal(replyFocusSnapshot({ dataset: {}, value: 'x' }), null);
	assert.equal(replyFocusSnapshot(null), null);
});

test('restoreFocusDecision: restores only a patch-caused blur, and refills an emptied replacement', () => {
	const snapshot = { ticket: '18', question: '11', value: 'hello', start: 2, end: 4 };

	// The morph blurred the box onto body, and the box for the same
	// question is still there with its text: give it focus back, but there
	// is nothing to refill.
	assert.deepEqual(restoreFocusDecision(snapshot, { isBody: true }, { value: 'hello' }), {
		focus: true,
		restoreValue: false,
	});

	// The replacement node came up with no text at all (a fresh node built
	// from a stale, draft-less render): the snapshot's own text is restored
	// too.
	assert.deepEqual(restoreFocusDecision(snapshot, { isBody: true }, { value: '' }), {
		focus: true,
		restoreValue: true,
	});

	// The replacement node is non-empty but stale -- it shows only what the
	// last autosave captured, a prefix of what the owner has since typed
	// (bug fix: refilling only an empty replacement lost every keystroke
	// since that autosave once the morph swapped in this node).
	assert.deepEqual(restoreFocusDecision(snapshot, { isBody: true }, { value: 'hel' }), {
		focus: true,
		restoreValue: true,
	});

	// A deliberate blur -- focus is on some other real element, not body --
	// must not be restored by the next patch.
	assert.deepEqual(restoreFocusDecision(snapshot, { isBody: false }, { value: 'hello' }), {
		focus: false,
		restoreValue: false,
	});

	// No snapshot (nothing was being typed into), or no target to restore
	// into (the question's box no longer renders at all), each decline too.
	assert.deepEqual(restoreFocusDecision(null, { isBody: true }, { value: 'hello' }), {
		focus: false,
		restoreValue: false,
	});
	assert.deepEqual(restoreFocusDecision(snapshot, { isBody: true }, null), {
		focus: false,
		restoreValue: false,
	});

	// The snapshot itself held no text (an empty box had focus): an empty
	// replacement is not "lost text", so nothing is refilled.
	assert.deepEqual(restoreFocusDecision({ ...snapshot, value: '' }, { isBody: true }, { value: '' }), {
		focus: true,
		restoreValue: false,
	});
});

// reduceStreamStatus / reconnectDelay / staleMarkerText: console.js's
// applyStreamEvent runs every /stream lifecycle event through this one
// reducer, deciding both the reconnect backoff and the "Reconnecting.
// Stale since HH:MM." marker (bug fix: after a stream ended for good,
// nothing reconnected it, and the sidebar kept showing a frame that was no
// longer live).

test('reduceStreamStatus schedules a reconnect when the last /stream finishes', () => {
	let status = emptyStreamStatus();
	status = reduceStreamStatus(status, { type: 'started' }, 0).status;
	let result = reduceStreamStatus(status, { type: 'finished' }, 0);
	assert.equal(result.effect.reconnectIn, 1000);
	status = result.status;

	status = reduceStreamStatus(status, { type: 'started' }, 0).status;
	result = reduceStreamStatus(status, { type: 'finished' }, 0);
	assert.equal(result.effect.reconnectIn, 2000);
	status = result.status;

	status = reduceStreamStatus(status, { type: 'started' }, 0).status;
	result = reduceStreamStatus(status, { type: 'finished' }, 0);
	assert.equal(result.effect.reconnectIn, 4000);
	status = result.status;

	// Keep doubling past the cap: reconnectDelay clamps at RECONNECT_MAX_MS.
	for (let i = 0; i < 5; i++) {
		status = reduceStreamStatus(status, { type: 'started' }, 0).status;
		result = reduceStreamStatus(status, { type: 'finished' }, 0);
		status = result.status;
	}
	assert.equal(result.effect.reconnectIn, RECONNECT_MAX_MS);
	assert.equal(reconnectDelay(0), RECONNECT_BASE_MS);
});

test('reduceStreamStatus ignores an aborted older stream', () => {
	let status = emptyStreamStatus();
	status = reduceStreamStatus(status, { type: 'started' }, 0).status; // old stream
	status = reduceStreamStatus(status, { type: 'started' }, 0).status; // new stream, nav mid-frame
	const result = reduceStreamStatus(status, { type: 'finished' }, 0); // old stream's finished
	assert.equal(result.effect.reconnectIn, null);
	assert.equal(result.status.staleSince, null);
});

test('reduceStreamStatus marks stale on reconnecting and failures, clears on a frame or patch', () => {
	let status = emptyStreamStatus();
	let result = reduceStreamStatus(status, { type: 'reconnecting' }, 1000);
	assert.equal(result.status.staleSince, 1000);
	status = result.status;

	// A second reconnecting keeps the first stale time, not the latest one.
	result = reduceStreamStatus(status, { type: 'reconnecting' }, 2000);
	assert.equal(result.status.staleSince, 1000);

	status = emptyStreamStatus();
	result = reduceStreamStatus(status, { type: 'retries-failed' }, 3000);
	assert.equal(result.status.staleSince, 3000);
	status = result.status;

	// error and retrying mark stale the same way as retries-failed.
	assert.equal(reduceStreamStatus(emptyStreamStatus(), { type: 'error' }, 7000).status.staleSince, 7000);
	assert.equal(reduceStreamStatus(emptyStreamStatus(), { type: 'retrying' }, 8000).status.staleSince, 8000);

	// A frame or patched event while nothing is inflight (a late event after
	// the stream has already finished) must not clear a stale marker.
	const stale = { ...status, inflight: 0, attempt: 2, staleSince: 3000 };
	result = reduceStreamStatus(stale, { type: 'datastar-patch-elements' }, 9000);
	assert.equal(result.status.staleSince, 3000);
	assert.equal(result.status.attempt, 2);
	result = reduceStreamStatus(stale, { type: 'patched' }, 9000);
	assert.equal(result.status.staleSince, 3000);
	assert.equal(result.status.attempt, 2);

	// A frame while inflight is above 0 clears staleSince and resets attempt,
	// and cancels any reconnect a prior idle trip scheduled -- a late frame
	// proving the stream recovered on its own must not let a stale reconnect
	// timer later abort and reopen it (bug fix).
	const live = { ...status, inflight: 1, attempt: 2 };
	result = reduceStreamStatus(live, { type: 'datastar-patch-elements' }, 5000);
	assert.equal(result.status.staleSince, null);
	assert.equal(result.status.attempt, 0);
	assert.equal(result.effect.cancelReconnect, true);

	// A patch clears it the same way.
	result = reduceStreamStatus(live, { type: 'patched' }, 6000);
	assert.equal(result.status.staleSince, null);
	assert.equal(result.status.attempt, 0);
	assert.equal(result.effect.cancelReconnect, true);

	// started always cancels any pending reconnect, and its effect carries
	// only cancelReconnect/reconnectIn -- no settleGen.
	const started = reduceStreamStatus(emptyStreamStatus(), { type: 'started' }, 0);
	assert.equal(started.effect.cancelReconnect, true);
	assert.deepEqual(Object.keys(started.effect).sort(), ['cancelReconnect', 'reconnectIn']);
});

test('reduceStreamStatus marks a silent stream stale and reconnects after STREAM_IDLE_MS', () => {
	let status = reduceStreamStatus(emptyStreamStatus(), { type: 'started' }, 0).status;
	status = reduceStreamStatus(status, { type: 'datastar-patch-elements' }, 100).status;

	// Just under the idle window: still live.
	let result = reduceStreamStatus(status, { type: 'tick' }, 100 + STREAM_IDLE_MS - 1);
	assert.equal(result.status.staleSince, null);
	assert.equal(result.effect.reconnectIn, null);

	// At the idle window: goes stale and schedules the first reconnect.
	result = reduceStreamStatus(status, { type: 'tick' }, 100 + STREAM_IDLE_MS);
	assert.equal(result.status.staleSince, 100 + STREAM_IDLE_MS);
	assert.equal(result.effect.reconnectIn, reconnectDelay(0));
	status = result.status;

	// A second tick 1s later is still within the (restarted) idle window, so
	// it does not schedule another reconnect, and the stale clock does not
	// move forward.
	result = reduceStreamStatus(status, { type: 'tick' }, 100 + STREAM_IDLE_MS + 1000);
	assert.equal(result.effect.reconnectIn, null);
	assert.equal(result.status.staleSince, 100 + STREAM_IDLE_MS);
});

test('reduceStreamStatus keeps a reconnect stale until its first frame', () => {
	let status = reduceStreamStatus(emptyStreamStatus(), { type: 'reconnecting' }, 1000).status;
	assert.equal(status.staleSince, 1000);
	status = reduceStreamStatus(status, { type: 'started' }, 1100).status;

	// Within the idle window of the new request: stays stale at the original time.
	let result = reduceStreamStatus(status, { type: 'tick' }, 3100);
	assert.equal(result.status.staleSince, 1000);

	// Past the idle window with still no frame: another reconnect is
	// scheduled, and "Stale since" holds at the original 1000, not the trip
	// time -- the clock latches on the first stale moment and must not keep
	// moving forward on every repeat idle trip.
	result = reduceStreamStatus(status, { type: 'tick' }, 1100 + STREAM_IDLE_MS);
	assert.equal(result.effect.reconnectIn, reconnectDelay(0));
	assert.equal(result.status.staleSince, 1000);
	assert.equal(result.status.attempt, 1);
	status = result.status;

	// A second trip, STREAM_IDLE_MS after the reset lastFrameAt, backs off to
	// the next attempt and still holds staleSince at 1000.
	result = reduceStreamStatus(status, { type: 'tick' }, 1100 + 2 * STREAM_IDLE_MS);
	assert.equal(result.effect.reconnectIn, reconnectDelay(1));
	assert.equal(result.status.staleSince, 1000);
	status = result.status;

	// Only a frame clears it, and it also cancels the reconnect the last
	// idle trip armed, so that timer cannot later fire and abort a stream
	// that has already recovered.
	result = reduceStreamStatus(status, { type: 'datastar-patch-elements' }, 1100 + 2 * STREAM_IDLE_MS + 50);
	assert.equal(result.status.staleSince, null);
	assert.equal(result.status.attempt, 0);
	assert.equal(result.effect.cancelReconnect, true);
});

test('reduceStreamStatus does not trip a tick when not inflight, not started, or never framed', () => {
	// Nothing inflight at all: a tick is a no-op, even long after now=0.
	const idle = emptyStreamStatus();
	let result = reduceStreamStatus(idle, { type: 'tick' }, 10 * STREAM_IDLE_MS);
	assert.equal(result.effect.reconnectIn, null);
	assert.deepEqual(result.status, idle);

	// started then finished (inflight back to 0): a tick long after must not
	// schedule a second reconnect on top of finished's own.
	let status = reduceStreamStatus(emptyStreamStatus(), { type: 'started' }, 0).status;
	status = reduceStreamStatus(status, { type: 'finished' }, 0).status;
	result = reduceStreamStatus(status, { type: 'tick' }, STREAM_IDLE_MS);
	assert.equal(result.effect.reconnectIn, null);

	// inflight above 0 but lastFrameAt still null (should not happen in
	// practice, since started always sets lastFrameAt, but the guard must
	// hold regardless): a tick must not trip on a null lastFrameAt.
	const noFrameYet = { ...emptyStreamStatus(), inflight: 1, lastFrameAt: null };
	result = reduceStreamStatus(noFrameYet, { type: 'tick' }, 10 * STREAM_IDLE_MS);
	assert.equal(result.effect.reconnectIn, null);
	assert.deepEqual(result.status, noFrameYet);
});

test('reduceStreamStatus restarts the idle clock on visible', () => {
	const status = { ...emptyStreamStatus(), inflight: 1, lastFrameAt: 0 };

	let result = reduceStreamStatus(status, { type: 'visible' }, 60000);
	assert.equal(result.status.lastFrameAt, 60000);

	result = reduceStreamStatus(result.status, { type: 'tick' }, 61000);
	assert.equal(result.status.staleSince, null);
	assert.equal(result.effect.reconnectIn, null);

	// visible with nothing inflight is a no-op.
	const idle = emptyStreamStatus();
	assert.deepEqual(reduceStreamStatus(idle, { type: 'visible' }, 60000).status, idle);
});

test('staleMarkerText formats the stale time', () => {
	assert.equal(staleMarkerText(null), '');
	assert.equal(staleMarkerText(new Date(2026, 9, 4, 19, 25).getTime()), 'Reconnecting. Stale since 19:25.');
});

// handleKeyEvent / decideKey / notePatchFocus / patchFocus: the whole former
// body of console.js's onKeyDown and the focus step of runPatchWork, moved
// here so node --test drives the wiring itself, not only its helpers (design
// section 6.4, owner decision on #44: "the whole keydown body moves into
// handleKeyEvent(state, event, now, run)"). realBindings is the committed
// static/keys.json, loaded from disk rather than a fixture, so these tests
// prove the real s/S/x removal and the real chip/blur bindings, not a
// hand-written stand-in that could drift from keys.go.
const realBindings = JSON.parse(readFileSync(new URL('./keys.json', import.meta.url), 'utf8'));

function freshKeyState(overrides) {
	return { bindings: realBindings, chord: emptyChordState(), suppressUntil: null, isMac: false, focusedID: '', previousFocusableIDs: [], ...overrides };
}

test('handleKeyEvent: s, S and x do nothing, since keys.json binds none of them', () => {
	const state = freshKeyState();
	const calls = [];
	let preventDefaultCalls = 0;
	for (const key of ['s', 'S', 'x']) {
		const event = { key, target: { tagName: 'BODY' }, preventDefault: () => { preventDefaultCalls++; } };
		handleKeyEvent(state, event, 0, (action) => calls.push(action));
	}
	assert.deepEqual(calls, []);
	assert.equal(preventDefaultCalls, 0);
});

test('handleKeyEvent: a key within 1000 ms after a focus change is ignored', () => {
	const state = freshKeyState({ focusedID: 'question:4' });

	const { focusID } = patchFocus(state, { previousFocusableIDs: ['question:4', 'question:5'], focusableIDs: ['question:5'] }, 1000);
	assert.equal(focusID, 'question:5');
	assert.equal(state.suppressUntil, 2000);

	const calls = [];
	const run = (action) => calls.push(action);
	for (const key of ['1', 'Escape']) {
		const event = { key, target: { tagName: 'BODY' }, preventDefault: () => assert.fail('preventDefault should not run while suppressed') };
		handleKeyEvent(state, event, 1999, run);
	}
	assert.deepEqual(calls, []);

	let prevented = false;
	const event = { key: '1', target: { tagName: 'BODY' }, preventDefault: () => { prevented = true; } };
	handleKeyEvent(state, event, 2000, run);
	assert.deepEqual(calls, ['chip']);
	assert.equal(prevented, true);
});

test('patchFocus leaves no deadline when the focused id is unchanged', () => {
	const state = freshKeyState({ focusedID: 'question:4', previousFocusableIDs: ['question:4', 'question:5'] });

	const { focusID } = patchFocus(state, { previousFocusableIDs: ['question:4', 'question:5'], focusableIDs: ['question:4', 'question:5'] }, 1000);
	assert.equal(focusID, 'question:4');
	assert.equal(state.suppressUntil, null);

	const calls = [];
	const event = { key: 'j', target: { tagName: 'BODY' }, preventDefault: () => {} };
	handleKeyEvent(state, event, 1001, (action) => calls.push(action));
	assert.deepEqual(calls, ['focus-next']);
});

test('notePatchFocus sets a deadline only when the focused id changed', () => {
	assert.equal(PATCH_SUPPRESS_MS, 1000);

	let state = { suppressUntil: null };
	assert.equal(notePatchFocus(state, 'question:4', 'question:5', 1000), 2000);
	assert.equal(state.suppressUntil, 2000);

	state = { suppressUntil: null };
	assert.equal(notePatchFocus(state, 'question:4', 'question:4', 1000), null);

	state = { suppressUntil: null };
	assert.equal(notePatchFocus(state, '', 'question:5', 1000), null);

	state = { suppressUntil: null };
	assert.equal(notePatchFocus(state, 'question:4', '', 1000), 2000);

	state = { suppressUntil: 1500 };
	assert.equal(notePatchFocus(state, 'question:4', 'question:4', 1000), 1500);
});

test('handleKeyEvent: g p calls run with the project action', () => {
	const state = freshKeyState();
	const calls = [];
	const run = (action) => calls.push(action);

	handleKeyEvent(state, { key: 'g', target: { tagName: 'BODY' }, preventDefault: () => {} }, 0, run);
	handleKeyEvent(state, { key: 'p', target: { tagName: 'BODY' }, preventDefault: () => {} }, 10, run);

	assert.deepEqual(calls, ['nav-project']);
});
