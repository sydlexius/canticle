// Phone-width sidebar toggle (#1094). Progressive enhancement: without this
// script the sidebar stays a static block above the content (see input.css).
// Plain ES, no inline anything: CSP is script-src 'self'.
(function () {
  'use strict';

  var button = document.querySelector('.mx-nav-toggle');
  var nav = document.getElementById('mx-sidebar');
  if (!button || !nav) {
    console.error('shell.js: .mx-nav-toggle or #mx-sidebar missing; sidebar toggle not wired');
    return;
  }

  // Switches the stylesheet from the no-JS static block to the off-canvas drawer.
  document.documentElement.classList.add('mx-js');

  function isOpen() {
    return button.getAttribute('aria-expanded') === 'true';
  }

  function setOpen(open, restoreFocus) {
    button.setAttribute('aria-expanded', open ? 'true' : 'false');
    if (open) {
      nav.setAttribute('data-open', 'true');
      nav.focus();
    } else {
      nav.removeAttribute('data-open');
      if (restoreFocus) {
        button.focus();
      }
    }
  }

  button.addEventListener('click', function () {
    setOpen(!isOpen(), true);
  });

  document.addEventListener('keydown', function (ev) {
    if (ev.key === 'Escape' && isOpen()) {
      setOpen(false, true);
    }
  });

  nav.addEventListener('click', function (ev) {
    var target = ev.target;
    if (target && target.closest && target.closest('a') && isOpen()) {
      setOpen(false, false);
    }
  });
})();
