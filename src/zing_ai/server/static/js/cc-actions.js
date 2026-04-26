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
