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
        modal.style.display = '';
        if (backdrop) backdrop.style.display = '';
    }
    if (opts.closeBtn) opts.closeBtn.addEventListener('click', close);
    if (backdrop) backdrop.addEventListener('click', close);
    document.addEventListener('keydown', function (e) {
        if (e.key === 'Escape' && modal.style.display !== 'none') close();
    });
    return { open: open, close: close };
}

// Repo chooser modal — close-only wiring; opening is handled by Datastar signal $modals.repoChooser.
mountModal({
    modal: document.getElementById('repo-chooser-modal'),
    backdrop: document.getElementById('repo-chooser-backdrop'),
    closeBtn: document.getElementById('repo-chooser-close'),
});

// Standup modal
(function() {
    var modal = document.getElementById('standup-modal');
    var copyBtn = document.getElementById('standup-copy-btn');
    var renderedEl = document.getElementById('standup-rendered');
    var markdownEl = document.getElementById('standup-markdown');
    var tabs = modal.querySelectorAll('[data-standup-tab]');
    var activeTab = 'rendered';
    var standupData = {};

    function switchTab(tab) {
        activeTab = tab;
        tabs.forEach(function(t) {
            t.classList.toggle('active', t.getAttribute('data-standup-tab') === tab);
        });
        renderedEl.style.display = tab === 'rendered' ? '' : 'none';
        markdownEl.style.display = tab === 'markdown' ? '' : 'none';
    }

    tabs.forEach(function(t) {
        t.addEventListener('click', function() {
            switchTab(t.getAttribute('data-standup-tab'));
        });
    });

    var ctl = mountModal({
        modal: modal,
        backdrop: document.getElementById('standup-modal-backdrop'),
        closeBtn: document.getElementById('standup-modal-close'),
    });

    copyBtn.addEventListener('click', function() {
        var text;
        if (activeTab === 'markdown') {
            text = standupData.markdown || '';
            navigator.clipboard.writeText(text).then(function() {
                copyBtn.textContent = '\u2713 Copied!';
                setTimeout(function() { copyBtn.textContent = 'Copy'; }, 1500);
            });
        } else {
            // Copy rich text (HTML) so it pastes formatted in Slack/etc.
            var blob = new Blob([standupData.html || ''], {type: 'text/html'});
            var textBlob = new Blob([standupData.markdown || ''], {type: 'text/plain'});
            var item = new ClipboardItem({'text/html': blob, 'text/plain': textBlob});
            navigator.clipboard.write([item]).then(function() {
                copyBtn.textContent = '\u2713 Copied!';
                setTimeout(function() { copyBtn.textContent = 'Copy'; }, 1500);
            });
        }
    });

    // Expose loader for the toolbar button
    window.openStandup = function(data) {
        standupData = data;
        renderedEl.innerHTML = data.html || '';
        markdownEl.textContent = data.markdown || '';
        switchTab('rendered');
        ctl.open();
    };
})();

// Terminal modal
(function() {
    var modal = document.getElementById('terminal-modal');
    var iframe = document.getElementById('terminal-modal-iframe');
    var title = document.getElementById('terminal-modal-title');

    var ctl = mountModal({
        modal: modal,
        backdrop: document.getElementById('terminal-modal-backdrop'),
        closeBtn: document.getElementById('terminal-modal-close'),
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
})();
