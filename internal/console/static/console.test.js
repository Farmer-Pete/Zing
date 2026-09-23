// console.test.js — node --test over keyboard.mjs only (design section 6.4,
// Makefile target test-js). This file is never served: the static mux
// (internal/console/server.go) allowlists five named assets, and this path
// is not one of them (design section 5, 12).
//
// Covers the chord state machine and timeout, key-to-action resolution
// from a fixture keys.json, input-context suppression, the mac/non-mac
// send chord, the id-based focus step (next, previous, from no focus,
// empty list), reconcileFocus across insertion/removal/reorder (focused
// first/middle/last row removed), and collectPatchWork.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
	CHORD_TIMEOUT_MS,
	emptyChordState,
	advanceChord,
	resolveAction,
	isInputContext,
	isSendChord,
	sendChordToken,
	stepFocus,
	reconcileFocus,
	collectPatchWork,
	navChanged,
	stepComposerIndex,
	buildChipDraftBody,
	buildItemDraftBody,
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

test('sendChordToken names the platform-correct keys.json token', () => {
	assert.equal(sendChordToken(true), 'Cmd-Enter');
	assert.equal(sendChordToken(false), 'Ctrl-Enter');
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
