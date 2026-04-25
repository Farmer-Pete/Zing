
// ── Review Drawer ────────────────────────────────────────────────────────

// Current drawer state.
var _drawerSessionId = null;
var _drawerMode = null;

function openReviewDrawer(sessionId, mode) {
    if (!sessionId) return;
    _drawerSessionId = sessionId;
    _drawerMode = mode || 'findings';
    var container = document.getElementById('review-drawer-container');
    container.innerHTML = '<div style="position:fixed;top:50%;left:50%;transform:translate(-50%,-50%);z-index:202;font-size:0.75rem;color:var(--gray-500)">Loading\u2026</div>';
    container.style.display = 'block';
    document.body.classList.add('drawer-open');

    fetch('/command-center/drawer/' + encodeURIComponent(sessionId))
        .then(function(r) { return r.text(); })
        .then(function(html) {
            container.innerHTML = html;
            // Update triage counter after inject.
            _updateTriageCount();
        })
        .catch(function(err) {
            container.innerHTML = '<div style="position:fixed;top:50%;left:50%;transform:translate(-50%,-50%);z-index:202;color:red">Failed to load drawer: ' + err.message + '</div>';
        });
};

function closeReviewDrawer() {
    var container = document.getElementById('review-drawer-container');
    container.style.display = 'none';
    container.innerHTML = '';
    document.body.classList.remove('drawer-open');
    _drawerSessionId = null;
    _drawerMode = null;
};

// Nav arrows: prev / next in queue.
function drawerNavPrev() {
    var queue = document.querySelector('.d-queue[data-prev-session-id]');
    var prevId = queue ? queue.getAttribute('data-prev-session-id') : null;
    if (prevId) openReviewDrawer(prevId);
};

function drawerNavNext() {
    var queue = document.querySelector('.d-queue[data-next-session-id]');
    var nextId = queue ? queue.getAttribute('data-next-session-id') : null;
    if (nextId) openReviewDrawer(nextId);
    else closeReviewDrawer();
};

// Skip: advance without submitting.
function skipToNext() {
    var btn = document.getElementById('drawer-submit-btn');
    var nextId = btn ? btn.getAttribute('data-next-session-id') : null;
    // Fall back to d-queue data attribute.
    if (!nextId) {
        var queue = document.querySelector('.d-queue[data-next-session-id]');
        nextId = queue ? queue.getAttribute('data-next-session-id') : null;
    }
    if (nextId) {
        openReviewDrawer(nextId);
    } else {
        // Queue empty.
        _showAllCaughtUp();
    }
};

// Submit + load next.
function submitAndNext() {
    var btn = document.getElementById('drawer-submit-btn');
    if (!btn) return;
    var sessionId = btn.getAttribute('data-session-id');
    var stepId = btn.getAttribute('data-step-id');
    var nextId = btn.getAttribute('data-next-session-id');

    if (!sessionId || !stepId) return;

    // Gather triage responses from the current drawer DOM.
    var responses = {};
    // Triage button selections.
    document.querySelectorAll('.df-tb.sa, .df-tb.sd, .df-tb.sdisc').forEach(function(tbtn) {
        var findingId = tbtn.getAttribute('data-finding-id');
        var action = tbtn.getAttribute('data-action');
        if (findingId && action) responses[findingId] = action;
    });
    // Text answers.
    document.querySelectorAll('.df-answer').forEach(function(ta) {
        var findingId = ta.getAttribute('data-finding-id');
        if (findingId && ta.value.trim()) responses[findingId] = ta.value.trim();
    });

    btn.disabled = true;
    btn.textContent = 'Submitting\u2026';

    fetch('/' + encodeURIComponent(sessionId) + '/submit', {
        method: 'POST',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({step_id: stepId, responses: responses})
    }).then(function(r) {
        if (r.ok) {
            if (nextId) {
                openReviewDrawer(nextId);
            } else {
                _showAllCaughtUp();
            }
        } else {
            btn.disabled = false;
            btn.textContent = 'Submit \u0026 Next';
            showToast('Submit failed (HTTP ' + r.status + ')', 'error');
        }
    }).catch(function(err) {
        btn.disabled = false;
        btn.textContent = 'Submit \u0026 Next';
        showToast('Submit failed: ' + err.message, 'error');
    });
};

// Triage button toggle.
function drawerTriage(tbtn) {
    var row = tbtn.closest('.df-tr');
    if (!row) return;
    var wasActive = tbtn.classList.contains('sa') || tbtn.classList.contains('sd') || tbtn.classList.contains('sdisc');
    // Clear all in row.
    row.querySelectorAll('.df-tb').forEach(function(b) {
        b.className = 'df-tb';
        b.setAttribute('data-action', b.getAttribute('data-action'));
    });
    if (!wasActive) {
        var action = tbtn.getAttribute('data-action');
        if (action === 'accept') tbtn.classList.add('sa');
        else if (action === 'drop') tbtn.classList.add('sd');
        else if (action === 'discuss') tbtn.classList.add('sdisc');
    }
    _updateTriageCount();
};

function _updateTriageCount() {
    var counter = document.getElementById('drawer-triage-count');
    if (!counter) return;
    var total = document.querySelectorAll('.df-tr').length + document.querySelectorAll('.df-answer').length;
    var done = document.querySelectorAll('.df-tb.sa, .df-tb.sd, .df-tb.sdisc').length;
    document.querySelectorAll('.df-answer').forEach(function(ta) {
        if (ta.value.trim()) done++;
    });
    counter.textContent = done;
}

function _showAllCaughtUp() {
    var container = document.getElementById('review-drawer-container');
    // Briefly show "All caught up" then close.
    if (container) {
        container.innerHTML = '<div style="position:fixed;top:50%;left:50%;transform:translate(-50%,-50%);z-index:202;background:#fff;border-radius:12px;padding:1.5rem 2rem;text-align:center;box-shadow:0 8px 32px rgba(0,0,0,0.15)"><div style="font-size:1.5rem">&#10003;</div><div style="font-size:1rem;font-weight:700;color:var(--navy);margin-top:0.5rem">All caught up</div></div>';
    }
    setTimeout(function() { closeReviewDrawer(); }, 2000);
}

// Open terminal from drawer (pass-through to existing openTerminal helper, mode=browser).
function openTerminalFromDrawer(sessionId) {
    if (!sessionId) return;
    fetch('/command-center/attach-session', {
        method: 'POST',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({terminal_session: sessionId, mode: 'browser'})
    }).then(function(r) { return r.json().then(function(data) {
        if (data.url) {
            window.openTerminal(data.url, sessionId);
            closeReviewDrawer();
        } else {
            showToast(data.error || 'Attach failed', 'error');
        }
    }); }).catch(function(err) {
        showToast('Attach failed: ' + err.message, 'error');
    });
};

// Delegated click handler for data-open-drawer elements.
document.addEventListener('click', function(e) {
    var el = e.target.closest('[data-open-drawer]');
    if (!el) return;
    // Don't intercept if it's a button inside an element that also has data-open-drawer
    // (the button should take priority over the parent).
    var sessionId = el.getAttribute('data-open-drawer');
    var mode = el.getAttribute('data-open-drawer-mode') || el.getAttribute('data-drawer-mode') || 'findings';
    if (sessionId) {
        e.stopPropagation();
        openReviewDrawer(sessionId, mode);
    }
});

// Drawer close (backdrop, X button).
document.addEventListener('click', function (e) {
    if (e.target.closest('[data-drawer-close]')) closeReviewDrawer();
});

// Drawer prev/next nav.
document.addEventListener('click', function (e) {
    var btn = e.target.closest('[data-drawer-nav]');
    if (!btn || btn.disabled) return;
    if (btn.getAttribute('data-drawer-nav') === 'prev') drawerNavPrev();
    else drawerNavNext();
});

// Drawer skip / submit.
document.addEventListener('click', function (e) {
    if (e.target.closest('[data-drawer-skip]')) skipToNext();
});
document.addEventListener('click', function (e) {
    var btn = e.target.closest('[data-drawer-submit]');
    if (btn && !btn.disabled) submitAndNext();
});

// Step section collapse toggle (used in past-step history rows).
document.addEventListener('click', function (e) {
    var hdr = e.target.closest('[data-step-toggle]');
    if (hdr && hdr.parentElement) hdr.parentElement.classList.toggle('open');
});

// Triage buttons inside the drawer.
document.addEventListener('click', function (e) {
    var btn = e.target.closest('.df-tb[data-action]');
    if (btn) drawerTriage(btn);
});

// "Attach to Session" hero button in the drawer.
document.addEventListener('click', function (e) {
    var btn = e.target.closest('[data-attach-drawer-session]');
    if (btn) openTerminalFromDrawer(btn.getAttribute('data-attach-drawer-session'));
});

// ESC key closes drawer (in addition to management tray).
document.addEventListener('keydown', function(e) {
    if (e.key === 'Escape') {
        var container = document.getElementById('review-drawer-container');
        if (container && container.style.display !== 'none' && container.innerHTML) {
            closeReviewDrawer();
            return;
        }
    }
});
