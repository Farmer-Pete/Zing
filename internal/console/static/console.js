// console.js — the console's DOM wiring (design section 6.3, 6.4): reads
// /static/keys.json, imports the pure logic from keyboard.mjs, installs a
// global keydown handler that runs the chord machine and dispatches
// actions, tracks focusedID and railOpen as plain JS state, and bridges to
// Datastar by dispatching a zing-nav CustomEvent on #stream-ctl. It is the
// only module with browser-absolute imports, and it is not unit-tested
// under Node (keyboard.mjs carries every piece of pure logic Node can
// exercise; design section 6.4: "console.js imports keyboard.mjs ... does
// the DOM walking and wiring ... it is not unit-tested under Node").
//
// mermaid (design section 6.3, 6.10): static/mermaid.js is the vendored,
// self-contained UMD build, loaded once by templates/shell.templ with a
// classic, non-module <script> tag in the document head, which sets
// window.mermaid before this module's own script (type=module, so it is
// deferred) runs. This module never imports mermaid itself; it only reads
// the global, in runMermaidGuarded below.

import {
	emptyChordState,
	advanceChord,
	resolveAction,
	isInputContext,
	isSendChord,
	sendChordToken,
	resolveToken,
	stepFocus,
	reduceNav,
	stepComposerIndex,
	buildChipDraftBody,
	buildItemDraftBody,
	collectPatchWork,
	describeAction,
} from './keyboard.mjs';

// defaultNav is the shell's own data-signals default (templates/shell.templ:
// `data-signals="{view: 'inbox', open: 0, project: 0}"`). console.js starts
// its local mirror of the navigation state from the same literal so the
// very first keyboard nav (before any zing-nav has fired) agrees with what
// the page already shows.
const defaultNav = { view: 'inbox', open: 0, project: 0 };

// isMac decides the platform for the send chord (design section 6.4: "Cmd
// on mac, Ctrl elsewhere"). userAgentData is preferred where available;
// userAgent is the fallback for browsers that do not yet implement it.
function isMac() {
	const uaData = globalThis.navigator?.userAgentData;
	if (uaData?.platform) {
		return uaData.platform === 'macOS';
	}
	return /Mac|iPhone|iPad|iPod/.test(globalThis.navigator?.userAgent ?? '');
}

// state is every piece of plain, client-only console.js state (design
// section 6.3, 6.4: "Focus and rail are not Datastar signals; they are
// plain console.js state"). Bundled in one object so the module has a
// single, greppable place naming what it tracks, not because callers pass
// it around.
const state = {
	bindings: [], // parsed keys.json; empty until it loads
	chord: emptyChordState(),
	nav: { ...defaultNav }, // this module's mirror of view/open/project
	navHistory: [], // for "u" (up one level); design section 6.4
	focusedID: '',
	previousFocusableIDs: [],
	railOpen: false,
	helpOpen: false,
};

// ---- keys.json loading -----------------------------------------------

// loadBindings fetches and parses /static/keys.json (design section 6.4:
// "It reads its binding table from /static/keys.json"). A fetch failure or
// a malformed body leaves state.bindings empty, so every key resolves to no
// action rather than throwing out of the keydown handler.
async function loadBindings() {
	try {
		const resp = await fetch('/static/keys.json');
		if (!resp.ok) {
			console.error('console.js: GET /static/keys.json', resp.status);
			return;
		}
		const parsed = await resp.json();
		if (Array.isArray(parsed)) {
			state.bindings = parsed;
		}
	} catch (err) {
		console.error('console.js: load keys.json', err);
	}
}

// ---- navigation: the zing-nav bridge -----------------------------------

// dispatchNav dispatches the zing-nav CustomEvent on #stream-ctl (design
// section 6.3): the one supported way imperative code writes the view,
// open, and project signals, since plain JavaScript has no signal-write
// API of its own. isBack rides along in detail (read back out by
// onZingNav/reduceNav below) so a "u" pop does not push its own destination
// back onto the history it just popped from.
function dispatchNav(next, isBack) {
	const ctl = document.getElementById('stream-ctl');
	if (!ctl) {
		return;
	}
	ctl.dispatchEvent(
		new CustomEvent('zing-nav', { detail: { view: next.view, open: next.open, project: next.project, isBack: Boolean(isBack) } }),
	);
}

// navigate moves to next (a full {view, open, project} triple) by
// dispatching zing-nav. It does not touch state.nav itself; onZingNav below
// is the one place that updates the local mirror, so a mouse click on one
// of nav.templ's temporary links -- which dispatches this exact same event
// directly on #stream-ctl, bypassing this function entirely -- keeps
// state.nav in sync too (PR #16 review, CodeRabbit console.js:315 / cubic
// console.js:57). Datastar's dispatchEvent runs
// every listener synchronously, so state.nav already reflects next by the
// time this call returns.
function navigate(next, opts = {}) {
	dispatchNav(next, opts.isBack);
}

// onZingNav is the zing-nav bridge's single receiver (design section 6.3,
// PR #16 review, CodeRabbit console.js:315 / cubic console.js:57):
// shell.templ's own data-on:zing-nav listener (which writes the Datastar
// signals and re-fetches /stream) and this one are both bound to
// #stream-ctl, so every navigation -- keyboard (navigate, above)
// or mouse (nav.templ's zingNavExpr links) -- runs through here exactly
// once, updating this module's local mirror, its back stack, and clearing
// focus on a real destination change (design section 6.4: "A view change
// clears both focusedID and the stored previous order"), the same way
// regardless of what triggered the navigation. reduceNav (keyboard.mjs) is
// the pure decision; this handler only applies its result as DOM/state
// effects.
function onZingNav(event) {
	const { nav, history, changed } = reduceNav(state.nav, state.navHistory, event.detail ?? {});
	state.nav = nav;
	state.navHistory = history;
	if (changed) {
		setFocusedID('');
		state.previousFocusableIDs = [];
	}
}

// goUp handles "u": pop the back stack, or fall back to Inbox when it is
// empty (design section 6.4: "u goes up one level").
function goUp() {
	const prev = state.navHistory.pop() ?? { ...defaultNav };
	navigate(prev, { isBack: true });
}

// openFocused handles "o"/Enter on a focused list row (design section 6.4:
// "o or Enter on a focused list row sets open and view=thread"). A
// ticket-namespaced focus id opens that ticket directly. A message-
// namespaced focus id (Feed's rows; design section 6.5) opens the ticket it
// belongs to instead, read off the row's own data-ticket-id (feed.templ,
// PR #16 review, cubic console.js:140): a message id names nothing '/stream'
// can render on its own, so without this a focused Feed row's 'o'/Enter did
// nothing. Thread's own message rows carry no data-ticket-id -- there is
// nothing more useful to open from inside the thread that already shows
// them -- so this stays a no-op there, unchanged from before. Anything else
// (no focus, or a focus id from a namespace this task does not yet make
// focusable) is also a no-op, returning false so dispatchAction skips
// preventDefault and any native behavior (e.g. a plain Enter inside a
// non-composer control) still runs.
function openFocused() {
	if (state.focusedID.startsWith('ticket:')) {
		const id = Number(state.focusedID.slice('ticket:'.length));
		if (!Number.isFinite(id) || id <= 0) {
			return false;
		}
		navigate({ view: 'thread', open: id, project: 0 });
		return true;
	}
	if (state.focusedID.startsWith('message:')) {
		const ticketID = Number(findByFocusID(state.focusedID)?.dataset?.ticketId);
		if (!Number.isFinite(ticketID) || ticketID <= 0) {
			return false;
		}
		navigate({ view: 'thread', open: ticketID, project: 0 });
		return true;
	}
	return false;
}

// ---- focus ring ---------------------------------------------------------

// focusableSelector names every element the focus ring moves across:
// anything in #main carrying a stable data-focus-id (design section 6.4,
// "every focusable item carries a stable, globally unique data-focus-id").
const focusableSelector = '#main [data-focus-id]';

function collectFocusableIDs() {
	return Array.from(document.querySelectorAll(focusableSelector)).map((el) => el.getAttribute('data-focus-id'));
}

function findByFocusID(id) {
	if (!id) {
		return null;
	}
	return document.querySelector(`[data-focus-id="${CSS.escape(id)}"]`);
}

// setFocusedID moves the focus class from the previously focused element
// (if any) to the one named by id (if any), and scrolls the newly focused
// element into view (design section 6.4: "adds the focus class to that
// id's element and scrolls it into view") -- but only when id actually
// differs from the previously focused id (PR #16 review, CodeRabbit
// console.js:180). The patch observer's runPatchWork calls this on every
// #main/#rail mutation, most of which reconcile back to the same
// focusedID; scrolling on every one of those made an unrelated patch
// elsewhere on the page yank the viewport back to a row the user never
// moved away from. The focus class is still reconciled unconditionally: a
// patch can morph in a fresh DOM node for the same id, which starts
// without the class the pre-patch node carried.
function setFocusedID(id) {
	const nextID = id ?? '';
	const changed = nextID !== state.focusedID;
	const prevEl = findByFocusID(state.focusedID);
	prevEl?.classList.remove('focus');
	state.focusedID = nextID;
	const el = findByFocusID(state.focusedID);
	if (el) {
		el.classList.add('focus');
		if (changed) {
			el.scrollIntoView({ block: 'nearest' });
		}
	}
}

// moveFocus handles "j"/"k": step from the current focus over the live
// focusable ids in DOM order (design section 6.4).
function moveFocus(direction) {
	const ids = collectFocusableIDs();
	setFocusedID(stepFocus(ids, state.focusedID, direction));
}

// ---- the composer's inputs (Tab/Shift-Tab; Task 6/7 build the composer) -

// composerInputSelector names the composer's own focusable controls (design
// section 6.6, PR #16 review, cubic console.js:197): an option chip, an
// item-decision button (thread.templ's optionChips/itemRows), or a
// free-reply input (thread.templ's freeReply). There is no wrapping
// ".composer" element -- the original selector named one that thread.templ
// never introduced, so it matched nothing and Tab/Shift-Tab silently fell
// through to the browser's native tab order instead of cycling the
// rendered controls.
const composerInputSelector = '#main .chip, #main .decision, #main .reply-input';

function moveComposerFocus(delta) {
	const els = Array.from(document.querySelectorAll(composerInputSelector));
	if (els.length === 0) {
		return false; // nothing to move across yet; let the browser handle Tab
	}
	const i = els.indexOf(document.activeElement);
	els[stepComposerIndex(els.length, i, delta)].focus();
	return true;
}

// ---- draft, chips, send (Task 6/7 endpoints; wired ahead of them) -------

// postJSON is the one small fetch wrapper every forward-wired mutation
// below shares: same-origin, Datastar-Request set so mw.go's guard (or its
// Task 7/10 successors) treats it as a first-party call, and errors logged
// rather than thrown into the keydown handler. It resolves true on a 2xx
// response and false otherwise (a non-ok status or a thrown fetch error),
// so a caller that needs to know (postDraft, to clear its input only on
// success) can await it; every fire-and-forget caller below just ignores
// the resolved value, same as before.
async function postJSON(path, body) {
	try {
		const resp = await fetch(path, {
			method: 'POST',
			headers: { 'Content-Type': 'application/json', 'Datastar-Request': 'true' },
			body: JSON.stringify(body),
		});
		if (!resp.ok) {
			console.error(`console.js: POST ${path}`, resp.status);
			return false;
		}
		return true;
	} catch (err) {
		console.error(`console.js: POST ${path}`, err);
		return false;
	}
}

// postDraft handles Enter inside a question input (design section 6.4,
// 6.7): data-draft-ticket/data-draft-question on the focused input, its
// value as the free-text reply. It clears the input synchronously, the
// moment the draft is queued, rather than waiting on postJSON's fetch to
// resolve (PR #16 review, cubic console.js:255): clearing in the async
// .then left a window where fast typing after Enter landed in the input
// before the response came back, and the old callback then wiped out that
// new, unsent text along with the already-sent draft. Clearing up front
// means a failed POST (postJSON's own console.error) loses the input's
// echo of what was sent, which is an acceptable trade against silently
// eating a later keystroke.
function postDraft() {
	const el = document.activeElement;
	const ticket = el?.dataset?.draftTicket;
	const question = el?.dataset?.draftQuestion;
	if (!ticket || typeof el.value !== 'string' || el.value === '') {
		return false;
	}
	const text = el.value;
	el.value = '';
	postJSON('/draft', {
		ticket: Number(ticket),
		question: question ? Number(question) : null,
		text,
	});
	return true;
}

// installChipActivation wires a delegated click listener for the
// composer's option chips (thread.templ's optionChips) and per-item
// accept/reject/drop/discuss controls (itemRow) -- the missing half of
// "pick then save" (design section 6.6, 6.7, code review fix 1). Each
// control's own data-on:click (thread.templ's pickToggleExpr) already
// toggles its "picked" class; this listener is what actually POSTs /draft
// with the picked value, so an option or item answer queues before /send
// runs. Delegated from document, like installSideBox and
// installLogControls, because #main is morphed by every /stream patch. A
// direct mouse click and pickChip's chip.click() (the 1..9 key path) both
// dispatch the same bubbling click event, so this one listener covers both
// activation paths.
function installChipActivation() {
	document.addEventListener('click', (event) => {
		const chip = event.target.closest?.('.chip');
		if (chip) {
			postJSON('/draft', buildChipDraftBody(chip.dataset));
			return;
		}
		const decision = event.target.closest?.('.item-decisions .decision');
		if (decision) {
			postJSON('/draft', buildItemDraftBody(decision.dataset));
		}
	});
}

// pickChip handles 1..9 on the focused question (design section 6.4: "1 to
// 9 click the nth option chip of the focused question"). chip.click()
// dispatches a real click event, which installChipActivation's delegated
// listener catches the same as a direct mouse click, so this keyboard path
// saves a draft too (code review fix 1), not just the visual "picked"
// toggle.
function pickChip(n) {
	if (!state.focusedID.startsWith('question:')) {
		return false;
	}
	const question = findByFocusID(state.focusedID);
	const chip = question?.querySelector(`[data-chip-index="${n}"]`);
	if (!chip) {
		return false;
	}
	chip.click();
	return true;
}

// sendBatch handles the send chord (design section 6.4, 6.7). POST /send
// does not exist until Task 7; wired ahead of it against the ticket this
// module's own nav state already tracks.
function sendBatch() {
	if (!state.nav.open) {
		return false;
	}
	postJSON('/send', { ticket: state.nav.open });
	return true;
}

// ---- rail, side box, stop, mark-read (Task 9/10/7 backends) -------------

function toggleRail() {
	state.railOpen = !state.railOpen;
	document.getElementById('rail')?.classList.toggle('open', state.railOpen);
}

function focusSideBox() {
	const el = document.querySelector('.side-box textarea, .side-box input');
	if (!el) {
		return false;
	}
	el.focus();
	return true;
}

// postSide handles a click on the side box's submit button (design section
// 6.11, 7.1: "a text area and a submit ... posts to /side, which returns a
// fixed inert reply ... rendered in the rail"). Unlike postJSON's other
// callers, the response body is not a 204: POST /side answers with the
// rendered reply fragment as its whole body (rail.go's handleSide), which
// this swaps into #side-reply directly, rather than waiting on the live
// /stream -- the side box never touches a run or posts to the thread, so
// there is nothing for a bus signal to pick up.
async function postSide(button) {
	const box = button.closest('.side-box');
	const textarea = box?.querySelector('textarea');
	if (!box || !textarea) {
		return;
	}
	try {
		const resp = await fetch('/side', {
			method: 'POST',
			headers: { 'Content-Type': 'application/json', 'Datastar-Request': 'true' },
			body: JSON.stringify({ ticket: state.nav.open, text: textarea.value }),
		});
		if (!resp.ok) {
			console.error('console.js: POST /side', resp.status);
			return;
		}
		const html = await resp.text();
		const reply = box.querySelector('#side-reply');
		if (reply) {
			reply.outerHTML = html;
		}
	} catch (err) {
		console.error('console.js: POST /side', err);
	}
}

// installSideBox wires the side box's submit button (design section 6.11).
// It listens on document rather than the button itself, because #rail is
// morphed by every /stream patch (design section 6.3); a listener bound
// directly to the button would need re-attaching after each patch, while
// delegation from a node that #rail's morph never replaces does not.
function installSideBox() {
	document.addEventListener('click', (event) => {
		const button = event.target.closest?.('.side-box button[type="submit"]');
		if (!button) {
			return;
		}
		event.preventDefault();
		postSide(button);
	});
}

// postLogLevel handles a change on the Log rail's level select (design
// section 6.11, 6.12, 7.1): POST /loglevel with the select's chosen value.
// It is a small forward-wired affordance around the endpoint that is this
// task's real substance, kept to the same postJSON/204 shape as stopTicket
// and markRead below rather than the fixed-reply shape postSide needs.
function postLogLevel(select) {
	postJSON('/loglevel', { level: select.value });
}

// postDebugToggle handles a click on the Log rail's debug toggle button
// (design section 6.11, 6.12, 7.1): POST /debug for the open ticket. The
// rail's own re-render (design section 6.3: every /stream frame re-patches
// #rail) picks up the flipped state and relabels the button; this function
// does not toggle any local state of its own.
function postDebugToggle() {
	if (!state.nav.open) {
		return;
	}
	postJSON('/debug', { ticket: state.nav.open });
}

// installLogControls wires the Log rail's level select and debug toggle
// (design section 6.11), delegated from document like installSideBox
// below, because #rail is morphed by every /stream patch (design section
// 6.3) and a listener bound directly to either control would need
// re-attaching after each patch.
function installLogControls() {
	document.addEventListener('change', (event) => {
		const select = event.target.closest?.('.rail-log .log-level-select');
		if (select) {
			postLogLevel(select);
		}
	});
	document.addEventListener('click', (event) => {
		const button = event.target.closest?.('.rail-log .log-debug-toggle');
		if (!button) {
			return;
		}
		event.preventDefault();
		postDebugToggle();
	});
}

function stopTicket() {
	if (!state.nav.open) {
		return false;
	}
	postJSON('/stop', { ticket: state.nav.open });
	return true;
}

function stopEverything() {
	if (!globalThis.confirm?.('Stop every running ticket?')) {
		return false;
	}
	postJSON('/stop', { all: true });
	return true;
}

function markRead() {
	if (!state.focusedID.startsWith('message:')) {
		return false;
	}
	const id = Number(state.focusedID.slice('message:'.length));
	if (!Number.isFinite(id) || id <= 0) {
		return false;
	}
	postJSON('/read', { message: id });
	return true;
}

// ---- the help overlay ----------------------------------------------------

// buildHelpOverlay lazily builds the "?" help overlay from state.bindings
// (design section 6.4: "toggles the keyboard-map overlay, rendered from
// keys.json"), so the one source also feeds the overlay text, not a
// second hand-copied list.
function buildHelpOverlay() {
	let overlay = document.getElementById('keyboard-help');
	if (overlay) {
		return overlay;
	}
	overlay = document.createElement('div');
	overlay.id = 'keyboard-help';
	overlay.style.display = 'none';
	overlay.style.position = 'fixed';
	overlay.style.inset = '10%';
	overlay.style.overflow = 'auto';
	overlay.style.zIndex = '1000';
	overlay.style.background = 'var(--zing-surface, #1c1f28)';
	overlay.style.border = '1px solid var(--zing-border, #2b2f3a)';
	overlay.style.borderRadius = '0.5rem';
	overlay.style.padding = '1rem';

	const rows = state.bindings
		.map((b) => `<div><strong>${b.keys.join(' / ')}</strong> — ${describeAction(b.action)}</div>`)
		.join('');
	overlay.innerHTML = `<h2>Keyboard map</h2>${rows || '<p>Loading…</p>'}`;
	document.body.appendChild(overlay);
	return overlay;
}

function toggleHelp() {
	const overlay = buildHelpOverlay();
	state.helpOpen = !state.helpOpen;
	overlay.style.display = state.helpOpen ? 'block' : 'none';
}

// blurActive handles Esc (design section 6.4: "Esc blurs" / "?14: Esc =
// leave an input"). When the help overlay is open, Esc closes that first,
// matching ordinary overlay conventions; otherwise it blurs whatever
// element currently has focus, which is only meaningful when that element
// is an input.
function blurActive() {
	if (state.helpOpen) {
		toggleHelp();
		return true;
	}
	document.activeElement?.blur?.();
	return true;
}

// ---- action dispatch table ----------------------------------------------

// actions maps every keys.json action name to its handler (design section
// 6.4). A handler returns false to decline the keypress (nothing to act
// on yet, e.g. an empty composer), which skips preventDefault so any
// native browser behavior for that key still runs.
const actions = {
	'nav-inbox': () => navigate({ view: 'inbox', open: 0, project: 0 }),
	'nav-recent': () => navigate({ view: 'recent', open: 0, project: 0 }),
	'nav-feed': () => navigate({ view: 'feed', open: 0, project: 0 }),
	'focus-next': () => moveFocus('next'),
	'focus-prev': () => moveFocus('prev'),
	open: () => openFocused(),
	up: () => goUp(),
	'input-next': () => moveComposerFocus(1),
	'input-prev': () => moveComposerFocus(-1),
	draft: () => postDraft(),
	chip: (event) => pickChip(Number(event.key)),
	send: () => sendBatch(),
	'toggle-rail': () => toggleRail(),
	'focus-side': () => focusSideBox(),
	stop: () => stopTicket(),
	'stop-all': () => stopEverything(),
	'mark-read': () => markRead(),
	help: () => toggleHelp(),
	blur: () => blurActive(),
};

function dispatchAction(action, event) {
	const handler = action ? actions[action] : null;
	if (!handler) {
		return;
	}
	if (handler(event) !== false) {
		event.preventDefault();
	}
}

// ---- keydown: token resolution and the chord machine ---------------------

// resolveToken (keyboard.mjs) turns the raw keydown event, plus whether it
// landed in an input, into the token keys.json binds; it is pure and lives
// there so node --test can cover its modifier handling directly (design
// section 6.4, PR review: a Ctrl/Meta/Alt-held single key outside an input
// must not resolve to a token, so Ctrl-1/Cmd-A/Ctrl-X etc. do not fire
// console actions and block the browser's own shortcuts for them).

// tokensNeverChorded are resolved directly, never fed through the "g"
// chord machine: each already names a complete action on its own, and
// running it through advanceChord would let a stray "g" arm just before
// one of these and then misinterpret it as a chord's second key.
const tokensNeverChorded = new Set(['Esc', 'Enter-in-input', 'Cmd-Enter', 'Ctrl-Enter', 'Tab', 'Shift-Tab']);

function onKeyDown(event) {
	const target = event.target;
	const inInput = isInputContext({ tagName: target?.tagName, isContentEditable: target?.isContentEditable });
	const token = resolveToken(event, inInput, isMac());
	if (token === null) {
		return; // suppressed while typing; let the input handle the keystroke
	}

	if (tokensNeverChorded.has(token)) {
		state.chord = emptyChordState();
		dispatchAction(resolveAction(token, state.bindings), event);
		return;
	}

	const { state: nextChord, chord } = advanceChord(state.chord, token, Date.now());
	state.chord = nextChord;
	if (chord) {
		dispatchAction(resolveAction(chord, state.bindings), event);
		return;
	}
	if (state.chord.leader) {
		return; // armed on this key (e.g. "g"), waiting for its second key
	}
	dispatchAction(resolveAction(token, state.bindings), event);
}

// ---- the patch observer: focus reconcile + (guarded) mermaid ------------

// diagramSelector names an unprocessed mermaid fence (design section 6.3,
// 6.10: goldmark-diagram emits `<pre class="mermaid">`). processedAttr
// marks a node once its diagram work has been considered, so the observer
// does not re-collect the same node on a later, unrelated patch (design
// section 6.3: "marks processed diagram nodes and does not react to its
// own class or attribute changes").
const diagramSelector = 'pre.mermaid:not([data-mermaid-processed])';
const processedAttr = 'data-mermaid-processed';

function collectDiagramIDs() {
	const nodes = Array.from(document.querySelectorAll(`#main ${diagramSelector}, #rail ${diagramSelector}`));
	return nodes.map((el, i) => {
		const id = el.id || `diagram:${Date.now()}:${i}`;
		el.id = id;
		return id;
	});
}

// mermaidReady serializes mermaid.run() calls (design section 6.3: "Mermaid
// runs are serialized (one run resolves before the next starts)"). Chaining
// every call onto this promise, instead of firing each independently, stops
// two back-to-back patches from starting overlapping runs against nodes an
// earlier run may still be mutating. mermaidInitialized guards the one
// required mermaid.initialize() call (design section 6.3, 6.10).
let mermaidReady = Promise.resolve();
let mermaidInitialized = false;

function ensureMermaidInitialized() {
	if (mermaidInitialized) {
		return;
	}
	mermaidInitialized = true;
	globalThis.mermaid.initialize({ securityLevel: 'strict', startOnLoad: false });
}

// runMermaidGuarded marks each resolved diagram node processed, then queues
// one mermaid.run() over them (design section 6.3, 6.10: "runs mermaid.run()
// over unprocessed .mermaid nodes after each patch"). It is a no-op when
// there is nothing new to draw, or when window.mermaid never loaded (the
// classic script in shell.templ's head failed, or has not run yet), so a
// missing or slow bundle degrades to the fence's raw escaped text instead of
// throwing out of the observer callback. A rejected run is caught and
// logged (design section 6.3: "a rejected run is caught and logged, not
// left to bubble"), never left to reject mermaidReady itself, which would
// poison every run queued after it.
function runMermaidGuarded(diagramIDs) {
	const nodes = diagramIDs.map((id) => document.getElementById(id)).filter((el) => el != null);
	for (const el of nodes) {
		el.setAttribute(processedAttr, '');
	}
	if (nodes.length === 0 || !globalThis.mermaid) {
		return;
	}
	ensureMermaidInitialized();
	mermaidReady = mermaidReady
		.then(() => globalThis.mermaid.run({ nodes }))
		.catch((err) => console.error('console.js: mermaid.run', err));
}

// runPatchWork is the MutationObserver callback's one per-patch step
// (design section 6.3): collect plain descriptors from the DOM, hand them
// to the pure collectPatchWork, then apply its result as DOM effects.
function runPatchWork() {
	const descriptors = {
		diagramIDs: collectDiagramIDs(),
		focusableIDs: collectFocusableIDs(),
		previousFocusableIDs: state.previousFocusableIDs,
	};
	const { diagramIDs, focusID } = collectPatchWork(descriptors, state.focusedID);
	state.previousFocusableIDs = descriptors.focusableIDs;
	setFocusedID(focusID);
	runMermaidGuarded(diagramIDs);
}

// installPatchObserver installs the one MutationObserver on #main and
// #rail (design section 6.3): childList + subtree only, deliberately
// without `attributes: true`, so the observer never reacts to its own
// focus-class or data-mermaid-processed writes (design section 6.3: "does
// not react to its own class or attribute changes"). It runs one initial
// scan on install, matching "It does one initial scan on install".
function installPatchObserver() {
	const observer = new MutationObserver(() => runPatchWork());
	for (const id of ['main', 'rail']) {
		const el = document.getElementById(id);
		if (el) {
			observer.observe(el, { childList: true, subtree: true });
		}
	}
	runPatchWork();
}

// ---- install --------------------------------------------------------------

// installNavBridge wires onZingNav onto #stream-ctl (PR #16 review,
// CodeRabbit console.js:315 / cubic console.js:57): the same element every
// zing-nav dispatch -- console.js's own dispatchNav or nav.templ's
// zingNavExpr links -- targets, so this one listener sees every navigation
// regardless of source.
function installNavBridge() {
	const ctl = document.getElementById('stream-ctl');
	ctl?.addEventListener('zing-nav', onZingNav);
}

async function install() {
	await loadBindings();
	document.addEventListener('keydown', onKeyDown);
	installNavBridge();
	installPatchObserver();
	installSideBox();
	installLogControls();
	installChipActivation();
}

install();
