// Named dispatch functions (Decision #17): Datastar signal watchers call these
// instead of inlining new CustomEvent(...) expressions in templates.

window.dispatchOpenTerminal = function(url) {
    if (!url) return;
    document.dispatchEvent(new CustomEvent('open-terminal', {detail: {url: url}, bubbles: true}));
};

window.dispatchCopyStandup = function(html, markdown) {
    document.dispatchEvent(new CustomEvent('copy-standup', {detail: {html: html, markdown: markdown}, bubbles: true}));
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
        if (url) window.openTerminal(url);
    });

    // Body-level signal-patch watcher dispatches 'close-terminal' when
    // $modals.terminal flips to false. Route through ctl.close() so the IIFE's
    // iframe ref is kept consistent — never a parallel teardown path.
    document.addEventListener('close-terminal', function() {
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
