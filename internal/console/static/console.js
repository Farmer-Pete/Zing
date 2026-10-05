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
	sendChordLabel,
	resolveToken,
	stepFocus,
	reduceNav,
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
	collectPatchWork,
	describeAction,
	nextPendingNav,
	draftConflictMessage,
	TOAST_DISMISS_MS,
	scheduleToastDismiss,
	clearReplyInputs,
	emptyStreamStatus,
	reduceStreamStatus,
	staleMarkerText,
	STREAM_SETTLE_MS,
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
	// streamConnected and pendingNav (bug fix): #stream-ctl's data-init
	// fires the first GET /stream before console.js's own script can prove
	// Datastar has finished wiring up #stream-ctl's data-on:zing-nav
	// listener (and every nav-link's data-on:click). A zing-nav dispatched
	// in that window -- the first click on a Threads-sidebar row right
	// after a page load -- could be caught by nothing and silently
	// dropped, leaving the main pane on the project list until a second
	// click. streamConnected flips true the first time the patch observer
	// sees a real mutation from the live stream (installPatchObserver's
	// markStreamConnected), proving the page is fully wired up; pendingNav
	// holds the last nav seen before that point so it can be re-applied
	// once it is.
	streamConnected: false,
	pendingNav: null,
	// replyFocus is the reply box the owner was last typing into (design:
	// "Snapshot document.activeElement ... and its selection before patch
	// work"), kept so runPatchWork's restoreReplyFocus can give it back if a
	// /stream patch blurred or replaced the node. null when no reply box has
	// focus, or once a deliberate blur's own focusout timeout has run.
	replyFocus: null,
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
	// nextPendingNav (keyboard.mjs, bug fix): remember this destination
	// until the stream proves connected, in case Datastar's own
	// data-on:zing-nav listener was not actually bound yet to act on the
	// event this handler just saw.
	state.pendingNav = nextPendingNav(state.streamConnected, nav);
}

// markStreamConnected flips state.streamConnected on the first real patch
// from the live stream (installPatchObserver's MutationObserver callback,
// never its one-time initial scan) and re-dispatches any nav queued before
// that point (bug fix: see state.pendingNav above). dispatchNav, not
// navigate, because the destination already went through reduceNav once;
// re-running it through onZingNav a second time is what actually applies
// it now that the stream -- and so Datastar's own listener -- is known to
// be live.
function markStreamConnected() {
	if (state.streamConnected) {
		return;
	}
	state.streamConnected = true;
	if (state.pendingNav) {
		const pending = state.pendingNav;
		state.pendingNav = null;
		dispatchNav(pending, false);
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

// findDraftConflictEl locates the reply box's own conflict span
// (thread.templ's freeReply: a ".draft-conflict" sibling inside the same
// ".reply" wrapper), the element showDraftConflict/clearDraftConflict
// below fill in or empty.
function findDraftConflictEl(inputEl) {
	return inputEl.closest('.reply')?.querySelector('.draft-conflict') ?? null;
}

function showDraftConflict(inputEl, message) {
	const el = findDraftConflictEl(inputEl);
	if (el) {
		el.textContent = message;
	}
}

// findDraftSavedEl/showDraftSaved mirror findDraftConflictEl/
// showDraftConflict above, for freeReply's own ".draft-saved" span (bug fix
// 11): the one place postDraftRequest reports a successful save back to the
// box it came from.
function findDraftSavedEl(inputEl) {
	return inputEl.closest('.reply')?.querySelector('.draft-saved') ?? null;
}

function showDraftSaved(inputEl, message) {
	const el = findDraftSavedEl(inputEl);
	if (el) {
		el.textContent = message;
	}
}

// postDraft handles Enter inside a question input (design section 6.4,
// 6.7): data-draft-ticket/data-draft-question on the focused input, its
// value as the free-text reply.
//
// It leaves the input's text in place (bug fix 11: Enter saved the draft --
// the serve log and the store both showed it -- but the box emptied and
// stayed empty, looking like the reply was lost). The earlier code cleared
// the box synchronously on Enter on the assumption that the live /stream's
// next patch would refill it from the now-saved draft, the way a full page
// load already does (01e4713); it doesn't, by design (answer.go's
// handleDraft: a draft publishes no bus wake, so saving one never patches
// #main), so the clear was never undone. Leaving the text alone sidesteps
// that gap entirely: the box already shows what got saved, postDraftRequest
// below reports success beside it (.draft-saved, "Saved."), and the box is
// only ever cleared by a real re-render -- after a successful send
// (sendBatch), when the draft drops out of the ticket's in-progress answer
// and the next patch renders the box empty.
function postDraft() {
	const el = document.activeElement;
	const body = unsavedReplyBody(el);
	if (!body) {
		return false;
	}
	postDraftRequest(el, body.ticket, body.question, body.text);
	return true;
}

// postDraftRequest reports its outcome only if el still holds the same text
// it was sent with: the owner may have kept typing while the request was in
// flight, and a stale "Saved."/conflict for text that is no longer in the
// box would be as misleading as the bug this fixes. It resolves true when
// the draft saved, so sendBatch can wait for the save before it sends.
//
// Clearing el's own conflict/saved notes first, rather than at each call
// site (postDraft, fireReplyAutosave, postSendBatch), keeps that reset
// beside the one function that actually posts the draft it is about (bug
// fix, quality: the two lines were copied at three call sites and could
// drift).
async function postDraftRequest(el, ticket, question, text) {
	showDraftConflict(el, '');
	showDraftSaved(el, '');
	try {
		const resp = await fetch('/draft', {
			method: 'POST',
			headers: { 'Content-Type': 'application/json', 'Datastar-Request': 'true' },
			body: JSON.stringify({ ticket, question, text }),
		});
		if (el.value !== text) {
			return resp.ok;
		}
		if (resp.ok) {
			showDraftSaved(el, 'Saved.');
			// A box targets a question (data-draft-question); a thread-level
			// reply (question === null, unsavedReplyBody) has no autosave key to
			// record against, since replyAutosaveBody only ever covers
			// question-targeted boxes.
			if (question != null) {
				lastSavedText.set(replyAutosaveKeyFor(el), text);
			}
			return true;
		}
		console.error('console.js: POST /draft', resp.status);
		const reason = (await resp.text()).trim();
		showDraftConflict(el, draftConflictMessage(reason));
		return false;
	} catch (err) {
		console.error('console.js: POST /draft', err);
		return false;
	}
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

// sendResultSelector/showSendResult (bug fix: Cmd+Enter sent the batch, but
// nothing in the console said so, so the owner thought it had done
// nothing). The banner is appended to document.body, a sibling of #main and
// #rail rather than a child of either (buildHelpOverlay, below, uses the
// same placement): POST /send's own bus.Publish wakes /stream almost
// immediately, and a #main patch lands right after the response this
// banner is built from, so an element server-rendered inside #main would
// be morphed back to empty before a reader could see it. A plain,
// client-owned node outside every patched region has nothing to race.
//
// sendResultTimerID (bug fix 12: the toast never went away on its own, so a
// "Sent 1 answer." from minutes ago kept looking current) tracks the one
// pending auto-dismiss across calls. showSendResult reuses the single
// #send-result element and always overwrites its text, so a second toast
// already replaces the first on screen; scheduleToastDismiss (keyboard.mjs)
// additionally cancels the first toast's own dismiss timer, so it cannot
// fire after the fact and remove the second toast early.
let sendResultTimerID = null;

function showSendResult(text) {
	let el = document.getElementById('send-result');
	if (!el) {
		el = document.createElement('div');
		el.id = 'send-result';
		el.className = 'send-result';
		el.setAttribute('role', 'status');
		el.setAttribute('aria-live', 'polite');
		document.body.appendChild(el);
	}
	el.textContent = text;
	sendResultTimerID = scheduleToastDismiss(
		sendResultTimerID,
		() =>
			setTimeout(() => {
				el.textContent = '';
				sendResultTimerID = null;
			}, TOAST_DISMISS_MS),
		clearTimeout,
	);
}

// sendBatch handles the send chord (design section 6.4, 6.7): POST /send
// against the ticket this module's own nav state already tracks, then
// shows its plain-text result (sendResultText, answer.go) via
// showSendResult -- "Sent N answer(s)." on 200, or the 409 body ("Nothing
// to send.") otherwise -- so Cmd+Enter is never silent.
//
// Every "#main .reply-input" holding unsaved text is saved first
// (unsavedReplyBodies), not just whichever one has focus (bug fix, Q11: the
// owner's note was in a box that had lost focus, and the old focused-only
// save left it behind while the send still reported success).
function sendBatch() {
	if (!state.nav.open) {
		return false;
	}
	postSendBatch(state.nav.open);
	return true;
}

// autosaveTimers maps a reply box's "ticket:question" key (replyAutosaveKeyFor
// below) to its pending debounced-save timer id (installReplyAutosave).
// autosaveInFlight maps the same key to the in-flight POST /draft promise a
// timer has already fired, if any (bug fix: cancelAutosaves only ever
// cleared a timer that had not fired yet; a timer that fired just before a
// Cmd+Enter was already running its fetch by the time cancelAutosaves ran,
// and postSendBatch never waited for it. If that fetch resolved after
// postSendBatch's own post-send reset, its lastSavedText.set call silently
// put the just-sent text's key back to the old, already-sent value, so a
// later retype of that same text looked "unchanged" and never autosaved
// again). lastSavedText maps the same key to the text last known saved for
// that box, so a later keystroke that merely re-types what is already saved
// does not re-post it.
const autosaveTimers = new Map();
const autosaveInFlight = new Map();
const lastSavedText = new Map();

// replyAutosaveKeyFor builds the one "ticket:question" key every autosave
// map above and postDraftRequest's own lastSavedText.set share, named once
// so the several call sites cannot drift into different keys for the same
// box (bug fix, quality).
function replyAutosaveKeyFor(el) {
	return `${el.dataset.draftTicket}:${el.dataset.draftQuestion}`;
}

// mainReplyInput returns the ".reply-input" at or above target when it sits
// inside #main, or null otherwise: the one check installReplyAutosave,
// installReplyFocusTracking, and clearReplyFocusUnlessInReplyBox each need,
// named once so the three do not drift (design section 6.4, 6.7: autosave,
// focus tracking, and the deliberate-blur check all key off the same reply
// box).
function mainReplyInput(target) {
	const el = target?.closest?.('.reply-input');
	return el && el.closest('#main') ? el : null;
}

// lastSavedFor returns the text last known saved for el's "ticket:question"
// key, falling back to el's own defaultValue (the server-rendered draft) the
// first time a key is seen, so a box no one has typed in since page load is
// not treated as having no saved text at all. fireReplyAutosave and
// postSendBatch share this one lookup, named once so the two call sites
// cannot drift into different fallbacks (bug fix, quality).
function lastSavedFor(el) {
	const key = replyAutosaveKeyFor(el);
	return lastSavedText.has(key) ? lastSavedText.get(key) : el.defaultValue;
}

// fireReplyAutosave posts el's current text as a draft if it differs from
// lastSavedFor(el). It is the one place that decides and posts an autosave,
// called both from scheduleReplyAutosave's debounce timer and from
// postSendBatch flushing a box the owner emptied right before Cmd+Enter (bug
// fix: cancelling that pending timer outright, rather than flushing it, left
// the box's already-saved, now-deleted text as the ticket's draft, and
// /send still sent it). Returns null, synchronously, when there is nothing
// to post; otherwise the postDraftRequest promise, which the caller can
// track.
function fireReplyAutosave(el) {
	const body = replyAutosaveBody(el, lastSavedFor(el));
	if (!body) {
		return null;
	}
	return postDraftRequest(el, body.ticket, body.question, body.text);
}

// scheduleReplyAutosave (re-)arms el's debounced save, keyed by
// replyAutosaveKeyFor so a later keystroke in the same box clears and
// replaces its own pending timer rather than stacking a second one
// (AUTOSAVE_DEBOUNCE_MS after the owner stops typing). It reuses
// scheduleToastDismiss's cancel-then-arm step, the same "replace, not stack"
// shape showSendResult's toast dismiss already needed, rather than writing
// clearTimeout/setTimeout out by hand a second time.
//
// The fired timer's own save is chained onto autosaveInFlight.get(key), not
// just recorded there, so two saves for the same box -- one still in flight
// when the next debounce fires -- run one after another instead of racing
// (bug fix: recording the second save's promise over the first's let the
// first's own `.finally` delete the second's still-pending entry once the
// first settled, so postSendBatch's await over autosaveInFlight.values()
// stopped waiting for it, and the two fetches could also reach the server
// out of order). The `autosaveInFlight.get(key) === result` check in the
// `finally` guards the same race the other way: only the most recent chain
// for this key may clear the entry, so a superseded promise settling late
// can never delete a newer one postSendBatch still needs to await.
function scheduleReplyAutosave(el) {
	const key = replyAutosaveKeyFor(el);
	const fire = () => {
		autosaveTimers.delete(key);
		const prior = autosaveInFlight.get(key) ?? Promise.resolve();
		const result = prior.then(() => fireReplyAutosave(el));
		autosaveInFlight.set(key, result);
		result.finally(() => {
			if (autosaveInFlight.get(key) === result) {
				autosaveInFlight.delete(key);
			}
		});
	};
	autosaveTimers.set(key, scheduleToastDismiss(autosaveTimers.get(key) ?? null, () => setTimeout(fire, AUTOSAVE_DEBOUNCE_MS), clearTimeout));
}

// cancelAutosaves cancels every pending debounced autosave timer, so
// postSendBatch below -- which saves and sends every box itself -- can never
// race a stale autosave into re-posting a box the send is about to clear. It
// does not wait for a timer that had already fired; postSendBatch awaits
// autosaveInFlight separately for that (bug fix).
function cancelAutosaves() {
	for (const timerID of autosaveTimers.values()) {
		clearTimeout(timerID);
	}
	autosaveTimers.clear();
}

// installReplyAutosave wires a delegated 'input' listener over every
// "#main .reply-input" (design: "Autosave the reply box as a draft,
// debounced on input"), delegated from document like installSideBox above
// because #main is morphed by every /stream patch.
function installReplyAutosave() {
	document.addEventListener('input', (event) => {
		const el = mainReplyInput(event.target);
		if (el) {
			scheduleReplyAutosave(el);
		}
	});
}

// installReplyFocusTracking keeps state.replyFocus current with whichever
// "#main .reply-input" the owner is typing into (design: "Snapshot
// document.activeElement ... and its selection before patch work"),
// delegated from document like installReplyAutosave above, because #main is
// morphed by every /stream patch.
//
// A deliberate blur (Esc, a click elsewhere, the post-send blur) and a
// patch-caused one (the morph replacing or moving the focused node) both
// fire the same focusout event, with nothing in the event itself telling
// them apart. clearReplyFocusUnlessInReplyBox is deferred to the next task
// via setTimeout(0), rather than run immediately, for the same reason: a
// deliberate blur has no mutation in the same task, so this timeout is the
// next thing to run and clears it; a patch-caused blur's MutationObserver
// callback is a microtask, which runs before this timeout, so
// restoreReplyFocus (runPatchWork, below) still finds the snapshot live and
// can restore it. Checking document.activeElement again inside the timeout,
// rather than clearing unconditionally, also covers the owner tabbing from
// one reply box straight to another: that focusout must not clear the
// snapshot the new box's own focusin just set.
function clearReplyFocusUnlessInReplyBox() {
	if (!mainReplyInput(document.activeElement)) {
		state.replyFocus = null;
	}
}

function installReplyFocusTracking() {
	const snapshotFromTarget = (target) => {
		const el = mainReplyInput(target);
		if (!el) {
			return;
		}
		state.replyFocus = replyFocusSnapshot(el);
	};
	document.addEventListener('focusin', (event) => snapshotFromTarget(event.target));
	document.addEventListener('input', (event) => snapshotFromTarget(event.target));
	// 'selectionchange' fires document-wide for every selection change,
	// including a bare caret move (an arrow key, Home/End, or a click with
	// nothing selected) that the narrower 'select' event misses: without
	// this, restoreReplyFocus could put the caret back at a stale position
	// after a patch replaced the box. It covers every case 'select' would
	// have (a selected range fires both), so 'select' is not also listened
	// for (bug fix, simplification). It is routed back through
	// document.activeElement rather than an event target, since
	// 'selectionchange' carries none.
	document.addEventListener('selectionchange', () => snapshotFromTarget(document.activeElement));
	document.addEventListener('focusout', () => setTimeout(clearReplyFocusUnlessInReplyBox, 0));
}

async function postSendBatch(ticket) {
	cancelAutosaves();
	// A timer that had already fired before cancelAutosaves ran is no
	// longer in autosaveTimers for it to cancel -- it is already awaiting
	// its own fetch. Waiting for those here (bug fix) keeps their
	// lastSavedText.set calls from landing after this function's own
	// post-send reset below, which would otherwise silently put a
	// just-sent box's key back to its pre-send text.
	await Promise.all(autosaveInFlight.values());

	const inputs = Array.from(document.querySelectorAll('#main .reply-input'));
	// Recorded before any further await below, so a box the owner keeps
	// typing into while this function's own fetches are in flight is never
	// cleared out from under them: the post-send clear only ever touches a
	// box whose value still matches what was actually sent (bug fix).
	const sentValues = new Map(inputs.map((el) => [el, el.value]));
	const pending = unsavedReplyBodies(inputs);
	// A box the owner emptied in the second before Cmd+Enter has no
	// "unsaved text" (unsavedReplyBodies, above, skips every empty box), but
	// cancelAutosaves above just dropped the pending timer that would have
	// cleared its saved draft. Flushing it here posts that clear before
	// /send runs, so /send -- which sends whatever the store still has
	// saved -- never sends text the box no longer shows (bug fix, Q3: "an
	// emptied box has to undo its saved draft, or the deleted text still
	// sends").
	const emptied = emptiedReplyBodies(inputs, lastSavedFor);
	const post = ({ el, body }) => postDraftRequest(el, body.ticket, body.question, body.text);

	const [results, clearResults] = await Promise.all([Promise.all(pending.map(post)), Promise.all(emptied.map(post))]);

	const failedClears = emptied.filter((_, i) => !clearResults[i]).map(({ el }) => el);
	if (failedClears.length > 0) {
		// A clear that failed leaves its old, deleted text saved as the
		// ticket's draft: sending now would silently resend text the owner
		// just emptied the box of (bug fix). The box itself stays empty --
		// there is no text left to show or keep -- but the send is held
		// back rather than risk it going out unseen.
		console.error('console.js: POST /draft (clear)', failedClears.length, 'box(es) failed to clear');
		showSendResult(clearFailedResult(failedClears.length));
		return;
	}

	// A box whose save just failed but already had an earlier autosave in
	// the store still sends that older draft: it is stale, not simply unsent
	// (bug fix). Both kinds keep their current text and are left out of the
	// post-send clear below.
	const { failed, stale } = partitionFailedSaves(pending, results, lastSavedFor);
	const notSent = [...failed, ...stale];

	try {
		const resp = await fetch('/send', {
			method: 'POST',
			headers: { 'Content-Type': 'application/json', 'Datastar-Request': 'true' },
			body: JSON.stringify({ ticket }),
		});
		const text = await resp.text();
		showSendResult(sendResultWithUnsent(text, failed.length, stale.length));
		if (resp.ok) {
			// A 200 means every saved draft just sent, so each of those boxes
			// should not keep showing text the owner just sent (design
			// section 22.7): Datastar's own morph never refills a focused
			// input, so this module clears it directly. A box in notSent
			// keeps its text and (for failed) its conflict note: either
			// nothing about it reached /send, or what did was an older
			// version than what the box still shows. A box whose value has
			// since changed (sentValues, above) is left alone either way: the
			// owner kept typing during this function's own awaits, and that
			// keystroke's own autosave timer is still armed to save it (bug
			// fix: clearing it here, or resetting its lastSavedText, would
			// otherwise drop that typing silently).
			const sent = inputs.filter((el) => !notSent.includes(el) && el.value === sentValues.get(el));
			clearReplyInputs(sent);
			// lastSavedText otherwise still holds the text that was just
			// sent, not the box's new, empty value: a later retype of that
			// same text would then look "unchanged" to replyAutosaveBody and
			// never autosave (bug fix: that retype would then depend on
			// focus or another Cmd+Enter to ever reach the store again).
			//
			// state.replyFocus otherwise still names this box with the
			// just-sent text as its value to restore (bug fix): the
			// document.activeElement?.blur?.() below fires a focusout whose
			// clearReplyFocusUnlessInReplyBox only runs on the next
			// setTimeout(0), and the very send that got here also wakes the
			// bus for a /stream patch, whose MutationObserver callback is a
			// microtask that can run first. Left live, that patch's
			// restoreReplyFocus would see the box now empty, differ from the
			// stale snapshot's value, and silently put the just-sent text
			// back in -- the next Cmd+Enter would then send it again.
			// Clearing the snapshot here, synchronously, cannot race either
			// path.
			for (const el of sent) {
				if (el.dataset.draftQuestion) {
					lastSavedText.set(replyAutosaveKeyFor(el), '');
				}
				if (state.replyFocus && state.replyFocus.ticket === el.dataset.draftTicket && state.replyFocus.question === el.dataset.draftQuestion) {
					state.replyFocus = null;
				}
			}
			document.activeElement?.blur?.();
		}
		if (!resp.ok && resp.status !== 409) {
			console.error('console.js: POST /send', resp.status);
		}
	} catch (err) {
		console.error('console.js: POST /send', err);
	}
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
			suppressPatchSignal = true;
			reply.outerHTML = html;
			suppressPatchSignal = false;
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

// ---- manual intake pickup (PKG9-PLAN.md D29) ------------------------------

// pickupIssue handles a click on the project view's "Pick up" button
// (PKG9-PLAN.md D29): reads the issue number from the box's own number
// input and the project id off the box's data-pickup-project attribute,
// posts POST /projects/{id}/pickup, and on a non-2xx response shows the
// response body -- D29's own exact refusal text -- in the box's error span.
// Unlike postJSON's other callers, the response body matters on failure, so
// this builds its own fetch rather than using postJSON, the same reason
// postSide above does.
async function pickupIssue(button) {
	const box = button.closest('.pickup-box');
	const input = box?.querySelector('.pickup-n');
	const errorSpan = box?.querySelector('.pickup-error');
	const projectID = box?.dataset?.pickupProject;
	if (!box || !input || !errorSpan || !projectID) {
		return;
	}
	const n = Number(input.value);
	if (!Number.isInteger(n) || n <= 0) {
		suppressPatchSignal = true;
		errorSpan.textContent = 'enter a positive issue number';
		suppressPatchSignal = false;
		return;
	}
	try {
		const resp = await fetch(`/projects/${projectID}/pickup`, {
			method: 'POST',
			headers: { 'Content-Type': 'application/json', 'Datastar-Request': 'true' },
			body: JSON.stringify({ n }),
		});
		if (!resp.ok) {
			const text = await resp.text();
			suppressPatchSignal = true;
			errorSpan.textContent = text;
			suppressPatchSignal = false;
			return;
		}
		input.value = '';
		suppressPatchSignal = true;
		errorSpan.textContent = '';
		suppressPatchSignal = false;
	} catch (err) {
		console.error('console.js: POST /projects/{id}/pickup', err);
		suppressPatchSignal = true;
		errorSpan.textContent = 'request failed';
		suppressPatchSignal = false;
	}
}

// installPickupBox wires the project view's pickup button (PKG9-PLAN.md
// D29), delegated from document like installSideBox above, because #main
// is morphed by every /stream patch (design section 6.3).
function installPickupBox() {
	document.addEventListener('click', (event) => {
		const button = event.target.closest?.('.pickup-box button[type="submit"]');
		if (!button) {
			return;
		}
		event.preventDefault();
		pickupIssue(button);
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

// ---- the patch observer: focus reconcile, (guarded) mermaid, send-chord
// hints ------------------------------------------------------------------

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

// sendChordSelector names an unprocessed send-chord placeholder
// (thread.templ's draftBanner and freeReply, bug fix: the composer saves a
// draft silently and sends only on a chord, with nothing on screen saying
// so). sendChordProcessedAttr marks a node once filled, the same guard
// processedAttr gives mermaid's diagram nodes above, so a later unrelated
// patch does not re-walk it.
const sendChordSelector = '.send-chord:not([data-send-chord-processed])';
const sendChordProcessedAttr = 'data-send-chord-processed';

// runSendChordHints fills every unprocessed ".send-chord" placeholder with
// the platform-correct glyph (design section 6.4's send-chord check, reused
// here since this server-rendered page cannot know the browser's platform):
// "⌘+Enter" on macOS, "Ctrl+Enter" elsewhere (sendChordLabel, keyboard.mjs).
// Marking each node processed before writing its text avoids reprocessing
// it on the childList mutation that textContent itself fires, the same
// bounded-single-extra-pass shape runMermaidGuarded's processedAttr gives
// mermaid's own diagram nodes.
function runSendChordHints() {
	const nodes = document.querySelectorAll(`#main ${sendChordSelector}`);
	if (nodes.length === 0) {
		return;
	}
	const label = sendChordLabel(isMac());
	for (const el of nodes) {
		el.setAttribute(sendChordProcessedAttr, '');
		el.textContent = label;
	}
}

// restoreReplyFocus is runPatchWork's last step (design: "If the morph
// blurred it, restore focus and selection in runPatchWork"): it looks up the
// box for the same ticket and question state.replyFocus names, and -- only
// when the patch itself left focus on document.body, per
// restoreFocusDecision (keyboard.mjs) -- gives it back focus and selection,
// refilling its value first if the replacement node came up empty. Running
// last, after setFocusedID/runMermaidGuarded/runSendChordHints above have
// already settled this patch's other DOM effects, keeps this the one step
// that can move focus.
function restoreReplyFocus() {
	const snapshot = state.replyFocus;
	if (!snapshot) {
		return;
	}
	const target = document.querySelector(
		`#main .reply-input[data-draft-ticket="${CSS.escape(snapshot.ticket)}"][data-draft-question="${CSS.escape(snapshot.question)}"]`,
	);
	const active = document.activeElement;
	const { focus, restoreValue } = restoreFocusDecision(
		snapshot,
		{ isBody: active === null || active === document.body },
		target ? { value: target.value } : null,
	);
	if (!focus) {
		return;
	}
	if (restoreValue) {
		target.value = snapshot.value;
	}
	target.focus({ preventScroll: true });
	target.setSelectionRange(snapshot.start, snapshot.end);
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
	runSendChordHints();
	restoreReplyFocus();
}

// ---- stream reconnect ---------------------------------------------------

// streamStatus/reconnectTimerID (the reconnect plan): applyStreamEvent's own
// state, mirroring reduceStreamStatus's StreamStatus across calls, and the
// one pending reconnect/settle timer so a later event can cancel it.
let streamStatus = emptyStreamStatus();
let reconnectTimerID = null;

// suppressPatchSignal is true for the duration of a client-side write under
// #main or #rail that is not a live /stream patch (postSide's
// reply.outerHTML swap, pickupIssue's errorSpan.textContent writes):
// installPatchObserver's MutationObserver fires for those too, and without
// this flag it would wrongly read them as proof the stream is live, clearing
// the stale marker and resetting the reconnect backoff while /stream itself
// may still be down.
let suppressPatchSignal = false;

// applyStreamEvent is console.js's one entry point into reduceStreamStatus
// (keyboard.mjs): it folds event into streamStatus, then applies the pure
// result's effect as DOM/timer side effects -- canceling a pending
// reconnect, scheduling the settle check that proves a started request
// live, or scheduling the backoff reconnect itself (dispatchNav(state.nav),
// so the server renders the full view again) -- before re-rendering the
// stale marker.
function applyStreamEvent(event) {
	const { status, effect } = reduceStreamStatus(streamStatus, event, Date.now());
	streamStatus = status;
	if (effect.cancelReconnect) {
		clearTimeout(reconnectTimerID);
		reconnectTimerID = null;
	}
	if (effect.settleGen !== null) {
		const gen = effect.settleGen;
		setTimeout(() => applyStreamEvent({ type: 'settled', gen }), STREAM_SETTLE_MS);
	}
	if (effect.reconnectIn !== null) {
		console.warn('console.js: /stream reconnecting', { delayMs: effect.reconnectIn, attempt: streamStatus.attempt });
		clearTimeout(reconnectTimerID);
		reconnectTimerID = setTimeout(() => {
			reconnectTimerID = null;
			applyStreamEvent({ type: 'reconnecting' });
			dispatchNav(state.nav, false);
		}, effect.reconnectIn);
	}
	renderStreamStatus();
}

// renderStreamStatus mirrors streamStatus.staleSince onto #stream-status (a
// client-owned node outside #nav/#main/#rail, the #send-result pattern) and
// onto body.stream-stale, which shell.templ's palette uses to dim
// nav.templ's "All clear." while the page is stale.
function renderStreamStatus() {
	const text = staleMarkerText(streamStatus.staleSince);
	document.body.classList.toggle('stream-stale', text !== '');
	let el = document.getElementById('stream-status');
	if (!el) {
		el = document.createElement('div');
		el.id = 'stream-status';
		el.setAttribute('role', 'status');
		el.setAttribute('aria-live', 'polite');
		document.body.appendChild(el);
	}
	el.textContent = text;
}

// installStreamWatch listens for Datastar's datastar-fetch events on
// #stream-ctl (design: shell.templ's retryMaxCount: 0 hands every /stream
// attempt's started/finished/error/retrying/retries-failed to
// applyStreamEvent instead of to Datastar's own retry). console.js loads
// before datastar.js (shell.templ), so this listener is already bound
// before data-init fires /stream's very first `started` event.
function installStreamWatch() {
	document.addEventListener('datastar-fetch', (event) => {
		if (event.detail?.el?.id !== 'stream-ctl') {
			return;
		}
		const { type } = event.detail;
		if (type === 'error' || type === 'retries-failed') {
			console.error('console.js: /stream', type, event.detail);
		}
		applyStreamEvent({ type });
	});
}

// installPatchObserver installs the one MutationObserver on #main and
// #rail (design section 6.3): childList + subtree only, deliberately
// without `attributes: true`, so the observer never reacts to its own
// focus-class or data-mermaid-processed writes (design section 6.3: "does
// not react to its own class or attribute changes"). It runs one initial
// scan on install, matching "It does one initial scan on install".
function installPatchObserver() {
	// The callback only ever runs for an actual #main/#rail mutation, never
	// for the installPatchObserver's own initial scan below, which calls
	// runPatchWork() directly -- so reaching this callback is itself proof
	// that a real /stream frame patched the page, the signal
	// markStreamConnected (bug fix, state.streamConnected above) needs, and
	// also a live signal for applyStreamEvent's own reduceStreamStatus
	// 'patched' case. The one exception is suppressPatchSignal: postSide and
	// pickupIssue also write under #main/#rail outside of a /stream frame,
	// so while that flag is set this skips only the 'patched' signal into
	// the reconnect state machine, not markStreamConnected/runPatchWork,
	// which stay correct for those writes too.
	const observer = new MutationObserver(() => {
		markStreamConnected();
		if (!suppressPatchSignal) {
			applyStreamEvent({ type: 'patched' });
		}
		runPatchWork();
	});
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

// install wires every delegated listener synchronously, before awaiting
// loadBindings' own /static/keys.json fetch (bug fix): none of
// installStreamWatch, installNavBridge, installPatchObserver, installSideBox,
// installLogControls, installChipActivation, installPickupBox,
// installReplyAutosave, or installReplyFocusTracking reads state.bindings, so
// there was no reason their listeners -- installNavBridge above all, the
// zing-nav bridge a Threads-sidebar click needs live as early as possible --
// sat behind an unrelated network round trip. Only
// onKeyDown needs the parsed bindings, so it alone waits on the fetch.
// installStreamWatch runs first (the reconnect plan): it must already be
// bound before data-init's own @get('/stream') can fire the very first
// datastar-fetch 'started' event.
async function install() {
	installStreamWatch();
	installNavBridge();
	installPatchObserver();
	installSideBox();
	installLogControls();
	installChipActivation();
	installPickupBox();
	installReplyAutosave();
	installReplyFocusTracking();
	await loadBindings();
	document.addEventListener('keydown', onKeyDown);
}

install();
