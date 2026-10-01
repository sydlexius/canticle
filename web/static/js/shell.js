// Phone-width sidebar toggle (#1094). Progressive enhancement: without this
// script the sidebar stays a static block above the content (see input.css).
// Plain ES, no inline anything: CSP is script-src 'self'.
//
// Every listener is DELEGATED from `document` and looks the toggle and the
// sidebar up at event time. htmx 2 restores history (browser Back/Forward) by
// replacing the body's contents, so a listener bound once to the original nodes
// would be left on detached elements and the Menu button would go dead until a
// full reload. `document` and <html> survive a restore; nothing else is assumed to.
(function () {
  'use strict';

  var TOGGLE = '.mx-nav-toggle';
  var SIDEBAR_ID = 'mx-sidebar';
  // Mirrors the input.css breakpoint that turns the sidebar into a drawer.
  var phone = window.matchMedia('(max-width: 767.98px)');

  // Switches the stylesheet from the no-JS static block to the off-canvas drawer.
  document.documentElement.classList.add('mx-js');

  function parts() {
    var button = document.querySelector(TOGGLE);
    var nav = document.getElementById(SIDEBAR_ID);
    if (!button || !nav) {
      console.error('shell.js: ' + TOGGLE + ' or #' + SIDEBAR_ID + ' missing; sidebar toggle not wired');
      return null;
    }
    return { button: button, nav: nav };
  }

  function isOpen(p) {
    return p.button.getAttribute('aria-expanded') === 'true';
  }

  function setOpen(p, open, restoreFocus) {
    p.button.setAttribute('aria-expanded', open ? 'true' : 'false');
    if (open) {
      p.nav.setAttribute('data-open', 'true');
      // Focusable only while open, so a desktop click on empty sidebar space
      // never focuses (and outlines) the nav.
      p.nav.setAttribute('tabindex', '-1');
      p.nav.focus();
      return;
    }
    p.nav.removeAttribute('data-open');
    p.nav.removeAttribute('tabindex');
    if (restoreFocus && phone.matches) {
      p.button.focus();
    }
  }

  // close forces the closed state on whatever nodes are current. Used after a
  // history restore or swap, where the restored markup may carry a stale open
  // state or the button and sidebar may disagree.
  function close() {
    var p = parts();
    if (p) {
      setOpen(p, false, false);
    }
  }

  document.addEventListener('click', function (ev) {
    var target = ev.target;
    if (!(target instanceof Element)) {
      return;
    }
    if (target.closest(TOGGLE)) {
      var t = parts();
      if (t) {
        setOpen(t, !isOpen(t), true);
      }
      return;
    }
    var p = parts();
    if (!p || !isOpen(p)) {
      return;
    }
    if (p.nav.contains(target)) {
      // Picking a link closes the drawer; a click on its empty space does not.
      if (target.closest('a')) {
        setOpen(p, false, false);
      }
      return;
    }
    // A click on the content while the drawer is open dismisses it (the touch
    // path that has no Escape key). The click itself still goes through.
    setOpen(p, false, false);
  });

  document.addEventListener('keydown', function (ev) {
    if (ev.key !== 'Escape') {
      return;
    }
    var p = parts();
    if (p && isOpen(p)) {
      setOpen(p, false, true);
    }
  });

  // Tabbing past the last link (or Shift+Tab past the first into the page)
  // closes the drawer instead of leaving it open with focus behind it. A null
  // relatedTarget (window blur, click on non-focusable space) is left to the
  // click handler.
  document.addEventListener('focusout', function (ev) {
    var next = ev.relatedTarget;
    if (!(next instanceof Element)) {
      return;
    }
    var p = parts();
    if (!p || !isOpen(p)) {
      return;
    }
    if (!p.nav.contains(next) && !p.button.contains(next)) {
      setOpen(p, false, false);
    }
  });

  // Leaving phone width with the drawer open would strand aria-expanded="true"
  // on a hidden button; normalize it.
  phone.addEventListener('change', function (ev) {
    if (!ev.matches) {
      close();
    }
  });

  // History: snapshot closed, and normalize whatever a restore brings back.
  document.addEventListener('htmx:beforeHistorySave', close);
  document.addEventListener('htmx:historyRestore', close);
  // A swap that replaced the toggle or the sidebar (but not both) can leave
  // them disagreeing; the server always renders them closed, so close both.
  document.addEventListener('htmx:afterSwap', function () {
    var p = parts();
    if (p && isOpen(p) !== p.nav.hasAttribute('data-open')) {
      setOpen(p, false, false);
    }
  });
})();
