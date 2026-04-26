// Toast notification helper
function showToast(message, type) {
    var container = document.getElementById('cc-toast-container');
    var toast = document.createElement('div');
    toast.className = 'cc-toast cc-toast-' + (type || 'error');
    toast.textContent = message;
    container.appendChild(toast);
    // Trigger reflow then add visible class for animation
    toast.offsetHeight;
    toast.classList.add('visible');
    setTimeout(function() {
        toast.classList.remove('visible');
        setTimeout(function() { toast.remove(); }, 300);
    }, 5000);
}

// Toolbar Standup button — fetch standup data, then hand off to the modal.
document.addEventListener('click', function (e) {
    var btn = e.target.closest('[data-cc-standup]');
    if (!btn || btn.disabled) return;
    btn.disabled = true;
    btn.textContent = 'Loading\u2026';
    fetch('/command-center/standup')
        .then(function (r) { return r.json(); })
        .then(function (data) {
            window.openStandup(data);
            btn.innerHTML = '&#x270D; Standup';
            btn.disabled = false;
        })
        .catch(function () {
            btn.textContent = 'Failed';
            setTimeout(function () {
                btn.innerHTML = '&#x270D; Standup';
                btn.disabled = false;
            }, 1500);
        });
});

// Open external URLs from data-open-url attributes (replaces inline onclick)
document.addEventListener('click', function(e) {
    var btn = e.target.closest('[data-open-url]');
    if (!btn) return;
    window.open(btn.getAttribute('data-open-url'), '_blank');
    closeAllMenus();
});

// Copy-to-clipboard for kebab menu copy buttons
document.addEventListener('click', function(e) {
    var btn = e.target.closest('[data-copy-cmd]');
    if (!btn) return;
    var cmd = btn.getAttribute('data-copy-cmd');
    var original = btn.innerHTML;
    navigator.clipboard.writeText(cmd).then(function() {
        btn.innerHTML = '&#10003;';
        btn.classList.add('copied');
        setTimeout(function() {
            btn.innerHTML = original;
            btn.classList.remove('copied');
        }, 1500);
    });
    closeAllMenus();
});

// Repo chooser: shows a popup when the server can't determine which repo to use.
// After the user picks, retries the original request with the chosen repo.
function handleRepoChoice(data, retryFn) {
    var modal = document.getElementById('repo-chooser-modal');
    var backdrop = document.getElementById('repo-chooser-backdrop');
    var list = document.getElementById('repo-chooser-list');
    list.innerHTML = '';
    data.repos.forEach(function(repo) {
        var btn = document.createElement('button');
        btn.className = 'repo-chooser-option';
        btn.textContent = repo;
        btn.addEventListener('click', function() {
            modal.style.display = 'none';
            backdrop.style.display = 'none';
            retryFn(repo);
        });
        list.appendChild(btn);
    });
    modal.style.display = '';
    backdrop.style.display = '';
}

// Background launch for data-launch-bg buttons
document.addEventListener('click', function(e) {
    var btn = e.target.closest('[data-launch-bg]');
    if (!btn || btn.disabled) return;
    var cardKey = btn.getAttribute('data-launch-bg');
    var label = btn.getAttribute('data-launch-bg-label');
    var skill = btn.getAttribute('data-launch-bg-skill');
    var prNumber = btn.getAttribute('data-launch-bg-pr');

    function doLaunch(repo) {
        btn.disabled = true;
        btn.innerHTML = '&#9203; Launching\u2026';
        var payload = {card_key: cardKey};
        if (repo) payload.repo = repo;
        if (skill) payload.skill = skill;
        if (prNumber) payload.pr_number = parseInt(prNumber, 10);

        fetch('/command-center/launch-background', {
            method: 'POST',
            headers: {'Content-Type': 'application/json'},
            body: JSON.stringify(payload)
        }).then(function(resp) {
            return resp.json().then(function(data) {
                if (data.status === 'choose_repo') {
                    btn.innerHTML = label; btn.disabled = false;
                    handleRepoChoice(data, doLaunch);
                } else if (!resp.ok) {
                    showToast(data.error || ('Launch failed (HTTP ' + resp.status + ')'), 'error');
                    btn.innerHTML = '&#10007; Failed';
                    setTimeout(function() { btn.innerHTML = label; btn.disabled = false; }, 3000);
                } else {
                    showToast('Session launched for ' + cardKey, 'success');
                    btn.innerHTML = '&#10003; Launched!';
                    setTimeout(function() { btn.innerHTML = label; btn.disabled = false; }, 2000);
                }
            });
        }).catch(function(err) {
            showToast('Launch failed: ' + err.message, 'error');
            btn.innerHTML = '&#10007; Failed';
            setTimeout(function() { btn.innerHTML = label; btn.disabled = false; }, 3000);
        });
    }
    doLaunch(null);
});

// Attach session (browser via Zellij)
document.addEventListener('click', function(e) {
    var btn = e.target.closest('[data-attach-session]');
    if (!btn || btn.disabled) return;
    var terminalSession = btn.getAttribute('data-attach-session');

    btn.disabled = true;
    btn.innerHTML = '&#9203; Attaching\u2026';

    fetch('/command-center/attach-session', {
        method: 'POST',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({terminal_session: terminalSession})
    }).then(function(resp) {
        return resp.json().then(function(data) {
            if (!resp.ok) {
                showToast(data.error || ('Attach failed (HTTP ' + resp.status + ')'), 'error');
                btn.innerHTML = '&#10007; Failed';
                setTimeout(function() { btn.innerHTML = 'Attach'; btn.disabled = false; }, 3000);
            } else if (data.url) {
                openTerminal(data.url, terminalSession);
                btn.innerHTML = '&#10003; Opened!';
                setTimeout(function() { btn.innerHTML = 'Attach'; btn.disabled = false; }, 2000);
            } else {
                btn.innerHTML = '&#10003; Attached!';
                setTimeout(function() { btn.innerHTML = 'Attach'; btn.disabled = false; }, 2000);
            }
        });
    }).catch(function(err) {
        showToast('Attach failed: ' + err.message, 'error');
        btn.innerHTML = '&#10007; Failed';
        setTimeout(function() { btn.innerHTML = 'Attach'; btn.disabled = false; }, 3000);
    });
});

// Start ticket (move to in-progress)
document.addEventListener('click', function(e) {
    var btn = e.target.closest('[data-start-ticket]');
    if (!btn || btn.disabled) return;
    var ticketId = btn.getAttribute('data-start-ticket');
    var label = btn.getAttribute('data-start-ticket-label');

    btn.disabled = true;
    btn.innerHTML = '&#9203; Starting\u2026';

    fetch('/command-center/start-ticket', {
        method: 'POST',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({ticket_id: ticketId})
    }).then(function(resp) {
        return resp.json().then(function(data) {
            if (!resp.ok) {
                showToast(data.error || ('Start failed (HTTP ' + resp.status + ')'), 'error');
                btn.innerHTML = '&#10007; Failed';
                setTimeout(function() { btn.innerHTML = label; btn.disabled = false; }, 3000);
            } else {
                showToast(ticketId + ' moved to In Progress', 'success');
                btn.innerHTML = '&#10003; Started!';
            }
        });
    }).catch(function(err) {
        showToast('Start failed: ' + err.message, 'error');
        btn.innerHTML = '&#10007; Failed';
        setTimeout(function() { btn.innerHTML = label; btn.disabled = false; }, 3000);
    });
});

// Management tray FAB toggles the slide-up panel and hides the FAB while open.
document.addEventListener('click', function (e) {
    var fab = e.target.closest('[data-mgmt-toggle]');
    if (!fab) return;
    var panel = document.getElementById('mgmt-panel');
    if (!panel) return;
    panel.classList.toggle('open');
    fab.classList.toggle('hidden');
});

// Management tray close button.
document.addEventListener('click', function (e) {
    if (!e.target.closest('[data-mgmt-close]')) return;
    var panel = document.getElementById('mgmt-panel');
    var fab = document.getElementById('mgmt-fab');
    if (panel) panel.classList.remove('open');
    if (fab) fab.classList.remove('hidden');
});

// ESC to close management tray
document.addEventListener('keydown', function(e) {
    if (e.key === 'Escape') {
        var panel = document.getElementById('mgmt-panel');
        if (panel && panel.classList.contains('open')) {
            panel.classList.remove('open');
            document.getElementById('mgmt-fab').classList.remove('hidden');
        }
    }
});

// Kill running session
document.addEventListener('click', function(e) {
    var btn = e.target.closest('[data-kill-session]');
    if (!btn) return;
    var sessionId = btn.getAttribute('data-kill-session');
    fetch('/command-center/kill-session', {
        method: 'POST',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({session_id: sessionId})
    }).then(function(resp) {
        return resp.json().then(function(data) {
            if (!resp.ok) showToast(data.error || 'Kill failed', 'error');
            else showToast('Session killed', 'success');
        });
    }).catch(function(err) {
        showToast('Kill failed: ' + err.message, 'error');
    });
    closeAllMenus();
});

// Cleanup/discard stopped session
document.addEventListener('click', function(e) {
    var btn = e.target.closest('[data-cleanup-session]');
    if (!btn) return;
    var sessionId = btn.getAttribute('data-cleanup-session');
    fetch('/command-center/cleanup-worktree', {
        method: 'POST',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({session_id: sessionId})
    }).then(function(resp) {
        return resp.json().then(function(data) {
            if (!resp.ok) showToast(data.error || 'Cleanup failed', 'error');
            else showToast('Session discarded', 'success');
        });
    }).catch(function(err) {
        showToast('Cleanup failed: ' + err.message, 'error');
    });
    closeAllMenus();
});

// Resume stopped session
document.addEventListener('click', function(e) {
    var btn = e.target.closest('[data-resume-session]');
    if (!btn || btn.disabled) return;
    var sessionId = btn.getAttribute('data-resume-session');
    var ticketId = btn.getAttribute('data-resume-ticket');

    btn.disabled = true;
    btn.innerHTML = '&#9203; Resuming\u2026';

    var payload = {card_key: ticketId, skill: 'resume'};

    fetch('/command-center/launch-background', {
        method: 'POST',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify(payload)
    }).then(function(resp) {
        return resp.json().then(function(data) {
            if (!resp.ok) {
                showToast(data.error || 'Resume failed', 'error');
                btn.innerHTML = '&#10007; Failed';
                setTimeout(function() { btn.innerHTML = 'Resume'; btn.disabled = false; }, 3000);
            } else {
                showToast('Session resumed', 'success');
                btn.innerHTML = '&#10003; Resumed!';
                setTimeout(function() { btn.innerHTML = 'Resume'; btn.disabled = false; }, 2000);
            }
        });
    }).catch(function(err) {
        showToast('Resume failed: ' + err.message, 'error');
        btn.innerHTML = '&#10007; Failed';
        setTimeout(function() { btn.innerHTML = 'Resume'; btn.disabled = false; }, 3000);
    });
});
