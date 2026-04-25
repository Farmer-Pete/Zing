// Kebab menu toggling
function toggleMenu(kebab) {
    var menu = kebab.nextElementSibling;
    if (!menu || !menu.classList.contains('strip-menu')) return;
    // Close all other open menus
    document.querySelectorAll('.strip-menu.open').forEach(function(m) {
        if (m !== menu) {
            m.classList.remove('open');
            m.closest('.card').classList.remove('menu-open');
        }
    });
    menu.classList.toggle('open');
    var card = menu.closest('.card');
    var isOpen = menu.classList.contains('open');
    card.classList.toggle('menu-open', isOpen);
    kebab.setAttribute('aria-expanded', isOpen);
    // Auto-focus search if present
    if (isOpen) {
        var search = menu.querySelector('.menu-search');
        if (search) { search.value = ''; search.focus(); filterMenu(search); }
    }
}

function closeAllMenus() {
    document.querySelectorAll('.strip-menu.open').forEach(function(m) {
        m.classList.remove('open');
        m.closest('.card').classList.remove('menu-open');
        var kebab = m.previousElementSibling;
        if (kebab && kebab.classList.contains('strip-kebab')) {
            kebab.setAttribute('aria-expanded', 'false');
        }
    });
}

document.addEventListener('click', function(e) {
    if (!e.target.closest('.strip-kebab') && !e.target.closest('.strip-menu')) {
        closeAllMenus();
    }
});

// Search filtering for complex kebab menus
function filterMenu(input) {
    var q = input.value.toLowerCase();
    var menu = input.closest('.strip-menu');
    menu.querySelectorAll('.menu-row').forEach(function(row) {
        var main = row.querySelector('.menu-row-main');
        var text = (main.getAttribute('data-s') || main.textContent).toLowerCase();
        row.style.display = text.includes(q) ? '' : 'none';
    });
    // Hide empty section labels
    menu.querySelectorAll('.menu-section-label').forEach(function(label) {
        var next = label.nextElementSibling;
        var hasVisible = false;
        while (next && !next.classList.contains('menu-section-label') && !next.classList.contains('strip-menu-divider')) {
            if (next.style.display !== 'none') hasVisible = true;
            next = next.nextElementSibling;
        }
        label.style.display = hasVisible ? '' : 'none';
    });
}

// Kept on window: tests/test_ui/test_kebab_menus.py invokes these via page.evaluate.
window.toggleMenu = toggleMenu;
window.filterMenu = filterMenu;

// Delegated kebab open/close. Fragments use [data-kebab-toggle] on the button.
document.addEventListener('click', function (e) {
    var btn = e.target.closest('[data-kebab-toggle]');
    if (btn) toggleMenu(btn);
});

// Delegated search filter for kebab menus that include a [data-kebab-search] input.
document.addEventListener('input', function (e) {
    var input = e.target.closest('[data-kebab-search]');
    if (input) filterMenu(input);
});
