// Named dispatch functions (Decision #17): Datastar signal watchers call these
// instead of inlining new CustomEvent(...) expressions in templates.

/**
 * Pure substring fuzzy-match helper for the Flow palette.
 * Returns true if the trimmed lowercase query is empty or is a substring of the haystack.
 * No DOM mutation; safely callable from data-show in a hot loop.
 */
window.flowPaletteMatch = function (query, haystack) {
  var q = (query || '').trim().toLowerCase();
  if (q === '') return true;
  return (haystack || '').toLowerCase().includes(q);
};

window.dispatchOpenTerminal = function(url, title) {
    if (!url) return;
    document.dispatchEvent(new CustomEvent('open-terminal', {detail: {url: url, title: title}, bubbles: true}));
};

window.dispatchCopyStandup = function(markdown) {
    // Read HTML straight from the DOM rather than from a $standupHtml signal —
    // the rendered HTML already lives in #standup-modal-body and storing a
    // duplicate in the signal store added O(HTML-size) to every patch_signals.
    var bodyEl = document.getElementById('standup-modal-body');
    var html = bodyEl ? bodyEl.innerHTML : '';
    document.dispatchEvent(new CustomEvent('copy-standup', {detail: {html: html, markdown: markdown}, bubbles: true}));
};

// Centralised clipboard write — replaces 20 inline navigator.clipboard.writeText
// expressions in kanban_card.html. One site to add toast/error feedback in.
window.dispatchCopyCmd = function(text) {
    if (!text) return;
    navigator.clipboard.writeText(text).catch(function() {
        // Silent failure is fine — kebab buttons don't have a feedback affordance
        // today. Centralised here so a future toast can be added in one place.
    });
};

// Wire a modal: backdrop click, close button click, and ESC all dismiss it.
// Returns { open, close } so callers can drive show/hide programmatically.
// onClose runs after the modal is hidden — used by the terminal modal to tear
// down the iframe (avoids the browser "Leave site?" dialog on src navigation).
function mountModal(opts) {
    var modal = opts.modal;
    var backdrop = opts.backdrop || null;
    function close() {
        modal.style.display = 'none';
        if (backdrop) backdrop.style.display = 'none';
        if (opts.onClose) opts.onClose();
    }
    function open() {
        if (opts.onOpen) opts.onOpen();
        modal.style.display = 'flex';
        if (backdrop) backdrop.style.display = '';
    }
    if (opts.closeBtn) opts.closeBtn.addEventListener('click', close);
    if (backdrop) backdrop.addEventListener('click', close);
    document.addEventListener('keydown', function (e) {
        if (e.key === 'Escape' && modal.style.display !== 'none') close();
    });
    return { open: open, close: close };
}

// Terminal modal — sole owner of the iframe lifecycle.
// Both the X button (modal.style.display) and the body-level signal-patch
// watcher (which dispatches 'close-terminal') route through ctl.close() so the
// onClose hook runs exactly once and the IIFE-scoped iframe variable stays in
// sync with the DOM (no stale-reference bug on reopen).
(function() {
    var modal = document.getElementById('terminal-modal');
    var iframe = document.getElementById('terminal-modal-iframe');
    var title = document.getElementById('terminal-modal-title');

    var ctl = mountModal({
        modal: modal,
        backdrop: document.getElementById('terminal-modal-backdrop'),
        onClose: function () {
            // Remove the iframe from the DOM entirely to avoid triggering the
            // "Leave site?" dialog when navigating it to about:blank.
            if (iframe && iframe.parentNode) iframe.parentNode.removeChild(iframe);
            iframe = null;
        },
    });

    window.openTerminal = function(url, sessionName) {
        title.textContent = sessionName || 'Terminal';
        // Create a fresh iframe each time (the previous one was removed on close).
        if (!iframe) {
            iframe = document.createElement('iframe');
            iframe.id = 'terminal-modal-iframe';
            iframe.className = 'terminal-modal-iframe';
            modal.appendChild(iframe);
        }
        iframe.src = url;
        ctl.open();
    };

    // Datastar signal-watcher entry point: the #terminal-launcher div fires
    // dispatchOpenTerminal($terminalUrl) via data-on-signal-patch, which dispatches
    // this event.
    document.addEventListener('open-terminal', function(e) {
        var url = e.detail && e.detail.url;
        var sessionTitle = e.detail && e.detail.title;
        if (url) window.openTerminal(url, sessionTitle);
    });

    // Body-level signal-patch watcher dispatches 'close-terminal' when
    // $modals.terminal flips to false. Route through ctl.close() so the IIFE's
    // iframe ref is kept consistent — never a parallel teardown path.
    document.addEventListener('close-terminal', function() {
        ctl.close();
    });
})();

// Launch popup — live terminal iframe with a Send-to-Flow footer button.
// Mirrors the terminal modal IIFE pattern: mountModal owns backdrop/ESC/close
// lifecycle; dispatchOpenLaunchPopup is the single Datastar-to-JS entry point.
(function() {
    var modal = document.getElementById('launch-popup-modal');
    var body = document.getElementById('launch-popup-body');
    var iframe = null;

    var ctl = mountModal({
        modal: modal,
        backdrop: document.getElementById('launch-popup-backdrop'),
        onClose: function () {
            if (iframe && iframe.parentNode) iframe.parentNode.removeChild(iframe);
            iframe = null;
        },
    });

    window.openLaunchPopup = function(url) {
        // Always remove-and-recreate the iframe. The wake-up refresh path
        // (/command-center/ttyd/refresh, host=popup) calls this with a new
        // URL while the modal is still open and the previous ttyd iframe is
        // still mounted — setting iframe.src on a live cross-origin frame
        // fires ttyd's beforeunload handler and the browser shows the
        // "Leave site?" dialog. Tearing the old iframe down first severs
        // the listener so the new src lands on a frame with no history.
        if (iframe && iframe.parentNode) iframe.parentNode.removeChild(iframe);
        iframe = document.createElement('iframe');
        iframe.id = 'launch-popup-iframe';
        iframe.className = 'terminal-modal-iframe';
        body.appendChild(iframe);
        iframe.src = url;
        ctl.open();
    };

    window.dispatchOpenLaunchPopup = function(url) {
        if (!url) return;
        document.dispatchEvent(new CustomEvent('open-launch-popup', {detail: {url: url}, bubbles: true}));
    };

    document.addEventListener('open-launch-popup', function(e) {
        var url = e.detail && e.detail.url;
        if (url) window.openLaunchPopup(url);
    });

    document.addEventListener('close-launch-popup', function() {
        ctl.close();
    });
})();

// Standup copy: dispatched by the Copy button via dispatchCopyStandup($standupHtml, $standupMarkdown).
document.addEventListener('copy-standup', function(e) {
    var html = e.detail && e.detail.html;
    var markdown = e.detail && e.detail.markdown;
    var copyBtn = document.getElementById('standup-copy-btn');
    if (!html && !markdown) return;
    var blob = new Blob([html || ''], {type: 'text/html'});
    var textBlob = new Blob([markdown || ''], {type: 'text/plain'});
    var item = new ClipboardItem({'text/html': blob, 'text/plain': textBlob});
    navigator.clipboard.write([item]).then(function() {
        if (copyBtn) {
            copyBtn.textContent = '\u2713 Copied!';
            setTimeout(function() { copyBtn.textContent = 'Copy'; }, 1500);
        }
    }).catch(function() {
        if (copyBtn) {
            copyBtn.textContent = 'Failed';
            setTimeout(function() { copyBtn.textContent = 'Copy'; }, 1500);
        }
    });
});
