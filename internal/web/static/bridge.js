/*
 * Artifex bridge (design §4.4). The server adds this script to every artifact
 * it serves; the file in the project never contains it. The artifact runs
 * with no origin of its own, so this script talks to the app only through
 * postMessage. It:
 *  - tells the app when the artifact has first drawn itself, so the app shows
 *    the frame only then, not empty and white;
 *  - reports the artifact's height, so the app sizes the iframe and the page,
 *    not the artifact, scrolls (unless the artifact has a height of its own:
 *    then the app keeps its frame that tall and the artifact scrolls itself);
 *  - turns clicks on links to other files of the project into "open" messages;
 *  - in comment mode, highlights the element under the pointer and reports the
 *    clicked element (CSS selector and text) and point;
 *  - draws the pins of threads, finding each pin's element again by selector
 *    and text, and falling back to its point (design decision 14).
 * Messages carry nothing secret, so they are posted to any origin; the app
 * only accepts them from its own iframes.
 */
(function () {
  'use strict';
  if (window.parent === window || window.__artifexBridge) return;
  window.__artifexBridge = true;

  var parts = location.pathname.split('/'); // "", "c", token, project, commit, ...
  var prefix = parts.slice(0, 5).join('/') + '/';
  var parent = window.parent;

  function send(type, data) {
    var m = data || {};
    m.type = 'artifex:' + type;
    try { parent.postMessage(m, '*'); } catch (e) { /* the app went away */ }
  }

  /* ---------- size ---------- */
  var lastHeight = 0, frame = 0;
  function measure() {
    frame = 0;
    var d = document.documentElement, b = document.body;
    if (!d) return;
    // The document is never shorter than the iframe, so measuring it could
    // only ever grow the iframe. The body follows the content (a page that
    // makes it fill the window keeps its height; app.js stops runaway growth).
    var h;
    if (b) {
      var cs = getComputedStyle(b);
      var margins = (parseFloat(cs.marginTop) || 0) + (parseFloat(cs.marginBottom) || 0);
      var bottom = b.getBoundingClientRect().bottom + window.scrollY + (parseFloat(cs.marginBottom) || 0);
      h = Math.ceil(Math.max(b.scrollHeight + margins, bottom));
    } else {
      h = d.scrollHeight;
    }
    var w = d.clientWidth;
    if (h !== lastHeight || w !== lastWidth) {
      lastHeight = h;
      lastWidth = w;
      send('size', { height: h, width: d.scrollWidth });
      if (pinData.length) placePins();
    }
  }
  var lastWidth = 0;
  function schedule() { if (!frame) frame = requestAnimationFrame(measure); }

  // The artifact scrolls itself when its frame is shorter than its content:
  // it has a height of its own (design decision 12).
  function scrollsItself() {
    var d = document.documentElement;
    return !!d && d.scrollHeight > window.innerHeight + 1;
  }

  function watchSize() {
    if (window.ResizeObserver) {
      var ro = new ResizeObserver(schedule);
      ro.observe(document.documentElement);
      if (document.body) ro.observe(document.body);
    }
    if (window.MutationObserver && document.body) {
      new MutationObserver(schedule).observe(document.body, { childList: true, subtree: true, attributes: true, characterData: true });
    }
    window.addEventListener('load', schedule);
    // Images and fonts that arrive late.
    var n = 0, t = setInterval(function () { schedule(); if (++n > 20) clearInterval(t); }, 500);
    schedule();
  }

  /* ---------- links ---------- */
  function onClick(e) {
    if (commentMode || e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    var a = e.target && e.target.closest ? e.target.closest('a[href]') : null;
    if (!a) return;
    var href = a.getAttribute('href');
    if (!href) return;
    if (href.charAt(0) === '#') {
      if (scrollsItself()) return; // the browser scrolls the artifact itself
      // The page scrolls, not the artifact: ask the app to scroll to the target.
      var id = decodeURIComponent(href.slice(1));
      var el = id ? document.getElementById(id) : document.body;
      if (el) {
        e.preventDefault();
        var r = el.getBoundingClientRect();
        send('scroll', { y: r.top + window.scrollY });
      }
      return;
    }
    if (/^(javascript|mailto|tel|sms):/i.test(href)) return;
    var u;
    try { u = new URL(href, location.href); } catch (err) { return; }
    if (u.origin === location.origin && u.pathname.indexOf(prefix) === 0) {
      e.preventDefault();
      var rel = u.pathname.slice(prefix.length);
      try { rel = decodeURIComponent(rel); } catch (err) { /* keep it encoded */ }
      send('open', { path: rel.replace(/\/$/, ''), hash: u.hash || '', newTab: a.target === '_blank' });
      return;
    }
    if (u.origin !== location.origin) {
      e.preventDefault();
      window.open(u.href, '_blank', 'noopener');
    }
  }

  /* ---------- comment mode ---------- */
  var commentMode = false, host = null, root = null, hl = null, pinLayer = null;

  function ensureOverlay() {
    if (host) return;
    host = document.createElement('artifex-overlay');
    host.setAttribute('style', 'all:initial;position:absolute;left:0;top:0;width:0;height:0;z-index:2147483647;pointer-events:none;');
    document.documentElement.appendChild(host);
    root = host.attachShadow({ mode: 'open' });
    root.innerHTML =
      '<style>' +
      '.hl{position:absolute;pointer-events:none;outline:2px solid #f59f00;background:rgba(245,159,0,.10);border-radius:2px;display:none;transition:all .04s}' +
      '.pin{position:absolute;pointer-events:auto;cursor:pointer;width:26px;height:26px;margin:-26px 0 0 -2px;border-radius:13px 13px 13px 2px;' +
      'background:#f59f00;color:#1c1300;font:700 12px/26px system-ui,sans-serif;text-align:center;box-shadow:0 2px 6px rgba(0,0,0,.35);border:2px solid #fff;box-sizing:border-box;padding:0}' +
      '.pin.resolved{background:#2b8a3e;color:#fff}.pin.temp{background:#3b5bdb;color:#fff}.pin.moved{border-style:dashed}' +
      '.pin.active{transform:scale(1.25);box-shadow:0 0 0 4px rgba(245,159,0,.45),0 2px 6px rgba(0,0,0,.35)}' +
      '</style><div class="hl"></div><div class="pins"></div>';
    hl = root.querySelector('.hl');
    pinLayer = root.querySelector('.pins');
  }

  function isOurs(el) { return el === host; }

  function onMove(e) {
    if (!commentMode) return;
    var el = document.elementFromPoint(e.clientX, e.clientY);
    if (!el || isOurs(el) || el === document.documentElement) { hl.style.display = 'none'; return; }
    var r = el.getBoundingClientRect();
    hl.style.display = 'block';
    hl.style.left = (r.left + window.scrollX) + 'px';
    hl.style.top = (r.top + window.scrollY) + 'px';
    hl.style.width = r.width + 'px';
    hl.style.height = r.height + 'px';
  }

  function block(e) {
    if (!commentMode) return;
    e.preventDefault();
    e.stopPropagation();
  }

  function onPick(e) {
    if (!commentMode) return;
    e.preventDefault();
    e.stopPropagation();
    var el = document.elementFromPoint(e.clientX, e.clientY);
    if (!el || isOurs(el)) return;
    if (el === document.documentElement && document.body) el = document.body;
    var r = el.getBoundingClientRect();
    var x = e.clientX + window.scrollX, y = e.clientY + window.scrollY;
    send('pick', {
      selector: selectorOf(el),
      text: textOf(el).slice(0, 160),
      x: Math.round(x), y: Math.round(y),
      ox: Math.round(x - (r.left + window.scrollX)), oy: Math.round(y - (r.top + window.scrollY)),
      width: document.documentElement.clientWidth
    });
  }

  function setMode(on) {
    commentMode = !!on;
    ensureOverlay();
    document.documentElement.style.cursor = commentMode ? 'crosshair' : '';
    if (!commentMode) hl.style.display = 'none';
  }

  function textOf(el) {
    return ((el.innerText || el.textContent || '') + '').replace(/\s+/g, ' ').trim();
  }

  function esc(s) { return window.CSS && CSS.escape ? CSS.escape(s) : s.replace(/[^a-zA-Z0-9_-]/g, '\\$&'); }

  function selectorOf(el) {
    var steps = [];
    while (el && el.nodeType === 1 && el !== document.documentElement) {
      if (el.id) {
        var byId = '#' + esc(el.id);
        try {
          if (document.querySelectorAll(byId).length === 1) { steps.unshift(byId); break; }
        } catch (e) { /* odd id */ }
      }
      var step = el.tagName.toLowerCase();
      var p = el.parentElement;
      if (p) {
        var same = 0, index = 0;
        for (var c = p.firstElementChild; c; c = c.nextElementSibling) {
          if (c.tagName === el.tagName) { same++; if (c === el) index = same; }
        }
        if (same > 1) step += ':nth-of-type(' + index + ')';
      }
      steps.unshift(step);
      el = p;
    }
    return steps.join(' > ');
  }

  /* ---------- pins ---------- */
  var pinData = [], activePin = 0, placing = false;

  function textMatches(el, want) {
    if (!want) return true;
    var have = textOf(el);
    var w = want.slice(0, 40);
    return have.slice(0, 40) === w || have.indexOf(want.slice(0, 24)) >= 0;
  }

  function placePins() {
    if (placing || !pinLayer) return;
    placing = true;
    pinLayer.innerHTML = '';
    var placed = [];
    for (var i = 0; i < pinData.length; i++) {
      var p = pinData[i], x = p.x, y = p.y, moved = false, el = null;
      if (p.selector) {
        try { el = document.querySelector(p.selector); } catch (e) { el = null; }
        if (el && !textMatches(el, p.text)) el = null;
      }
      if (el) {
        var r = el.getBoundingClientRect();
        var ox = p.ox == null ? 0 : Math.min(Math.max(p.ox, 0), r.width);
        var oy = p.oy == null ? 0 : Math.min(Math.max(p.oy, 0), r.height);
        x = r.left + window.scrollX + ox;
        y = r.top + window.scrollY + oy;
      } else if (p.selector || p.text) {
        moved = true;
      }
      if (x == null || y == null) { placed.push({ id: p.id, moved: moved, none: true }); continue; }
      var b = document.createElement('button');
      b.className = 'pin' + (p.state === 'resolved' ? ' resolved' : '') + (p.temp ? ' temp' : '') + (moved ? ' moved' : '') + (p.id === activePin ? ' active' : '');
      b.textContent = p.label || '';
      b.style.left = x + 'px';
      b.style.top = y + 'px';
      b.title = moved ? 'The element this comment was on has changed; the pin shows where it was.' : '';
      (function (id) {
        b.addEventListener('click', function (e) { e.preventDefault(); e.stopPropagation(); send('pin', { id: id }); });
      })(p.id);
      pinLayer.appendChild(b);
      // y in the frame: what the app scrolls its page by.
      placed.push({ id: p.id, x: x, y: y - window.scrollY, docY: y, moved: moved });
    }
    send('placed', { pins: placed });
    placing = false;
  }

  // In an artifact that scrolls itself, bring the active pin into view.
  function reveal() {
    if (!activePin || !pinLayer || !scrollsItself()) return;
    var pin = pinLayer.querySelector('.pin.active');
    if (!pin) return;
    var y = parseFloat(pin.style.top) || 0;
    if (y < window.scrollY + 30 || y > window.scrollY + window.innerHeight - 30) {
      window.scrollTo(0, Math.max(0, y - window.innerHeight / 3));
      placePins(); // report the pins' new place in the frame
    }
  }

  /* ---------- messages from the app ---------- */
  window.addEventListener('message', function (e) {
    if (e.source !== parent) return;
    var m = e.data;
    if (!m || typeof m.type !== 'string' || m.type.indexOf('artifex:') !== 0) return;
    switch (m.type) {
      case 'artifex:mode': setMode(m.comment); break;
      case 'artifex:pins': ensureOverlay(); pinData = m.pins || []; placePins(); break;
      case 'artifex:focus': activePin = m.id || 0; placePins(); reveal(); break;
      case 'artifex:measure': lastHeight = 0; schedule(); break;
      // The app may start listening after this artifact has loaded: it says
      // hello, and gets the messages it missed again.
      case 'artifex:hello': if (isReady) send('ready'); if (hasPainted) sendPainted(); break;
    }
  });

  document.addEventListener('mousemove', onMove, true);
  document.addEventListener('click', onPick, true);
  ['mousedown', 'mouseup', 'pointerdown', 'pointerup', 'dblclick', 'submit', 'contextmenu'].forEach(function (t) {
    document.addEventListener(t, block, true);
  });
  document.addEventListener('keydown', function (e) {
    if (commentMode && e.key === 'Escape') send('cancel');
  }, true);
  document.addEventListener('click', onClick, false);

  /* ---------- first paint ----------
   * The app keeps the frame hidden until the artifact has drawn itself, so a
   * reader never sees an empty white frame. Two animation frames after the
   * document is parsed, its stylesheets are in and it has been drawn; "load"
   * is the fallback, for a browser that doesn't animate a frame it hides. */
  var isReady = false, hasPainted = false;
  // The artifact's own background, so the app colours the frame with it: a
  // frame shown a moment before its content arrives is then the right colour.
  function background() {
    var els = [document.documentElement, document.body];
    for (var i = 0; i < els.length; i++) {
      if (!els[i]) continue;
      var c = getComputedStyle(els[i]).backgroundColor;
      if (c && c !== 'transparent' && !/^rgba\(.*,\s*0\)$/.test(c)) return c;
    }
    return '';
  }
  function sendPainted() { send('painted', { bg: background() }); }
  function onPainted() {
    if (hasPainted) return;
    hasPainted = true;
    sendPainted();
  }
  function onReady() {
    isReady = true;
    watchSize();
    send('ready');
    requestAnimationFrame(function () { requestAnimationFrame(onPainted); });
  }
  window.addEventListener('load', onPainted);
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', onReady);
  else onReady();
})();
