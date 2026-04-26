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
