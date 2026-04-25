// Notification opt-in: show the button only when permission is still default,
// and request permission when the user clicks it.
(function () {
    if (!('Notification' in window) || Notification.permission !== 'default') return;
    var btn = document.querySelector('[data-notif-opt-in]');
    if (!btn) return;
    btn.style.display = 'inline-block';
    btn.addEventListener('click', function () {
        Notification.requestPermission().then(function (perm) {
            if (perm !== 'default') btn.style.display = 'none';
        });
    });
})();
