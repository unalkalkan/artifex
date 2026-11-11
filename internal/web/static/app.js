/* Artifex pages. No framework: each page with a script part is chosen by
 * <body data-page>. Artifacts run in iframes with no origin and talk to this
 * script only through postMessage (see bridge.js). */
(function () {
  'use strict';
  var body = document.body;
  var csrf = body.dataset.csrf || '';
  var page = body.dataset.page;

  /* ---------- helpers ---------- */
  function $(sel, el) { return (el || document).querySelector(sel); }
  function $all(sel, el) { return Array.prototype.slice.call((el || document).querySelectorAll(sel)); }
  function h(tag, attrs, kids) {
    var el = document.createElement(tag);
    if (attrs) Object.keys(attrs).forEach(function (k) {
      if (k === 'class') el.className = attrs[k];
      else if (k === 'text') el.textContent = attrs[k];
      else if (k.indexOf('on') === 0) el.addEventListener(k.slice(2), attrs[k]);
      else if (attrs[k] !== undefined && attrs[k] !== null && attrs[k] !== false) el.setAttribute(k, attrs[k]);
    });
    (kids || []).forEach(function (k) { if (k) el.appendChild(typeof k === 'string' ? document.createTextNode(k) : k); });
    return el;
  }
  function api(method, url, data) {
    return fetch(url, {
      method: method, credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf },
      body: data ? JSON.stringify(data) : undefined
    }).then(function (r) {
      return r.json().catch(function () { return {}; }).then(function (j) {
        if (!r.ok) throw new Error(j.error || r.statusText);
        return j;
      });
    });
  }
  function toast(msg) {
    var t = $('#toast');
    if (!t) return;
    t.textContent = msg;
    t.hidden = false;
    clearTimeout(toast.timer);
    toast.timer = setTimeout(function () { t.hidden = true; }, 3500);
  }
  function short(id) { return (id || '').slice(0, 10); }
  function store(key, val) {
    try { if (val === undefined) return localStorage.getItem(key); localStorage.setItem(key, val); } catch (e) { return null; }
  }

  /* ---------- times, in the reader's zone ---------- */
  var rtf = window.Intl && Intl.RelativeTimeFormat ? new Intl.RelativeTimeFormat(undefined, { numeric: 'auto' }) : null;
  function rel(d) {
    var s = (d.getTime() - Date.now()) / 1000, a = Math.abs(s);
    if (!rtf) return d.toLocaleString();
    if (a < 45) return rtf.format(Math.round(s), 'second');
    if (a < 2700) return rtf.format(Math.round(s / 60), 'minute');
    if (a < 79200) return rtf.format(Math.round(s / 3600), 'hour');
    if (a < 2592000) return rtf.format(Math.round(s / 86400), 'day');
    return d.toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' });
  }
  function fmtTimes(scope) {
    $all('time[datetime]', scope).forEach(function (t) {
      var d = new Date(t.getAttribute('datetime'));
      if (isNaN(d)) return;
      t.textContent = rel(d);
      t.title = d.toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' });
    });
  }
  fmtTimes(document);
  setInterval(function () { fmtTimes(document); }, 60000);

  /* ---------- forms ----------
   * A form with data-confirm asks first, in Tabler's dialog (layout.html).
   * data-confirm-title, -cta and -variant ("danger" unless set) word it. */
  var confirmBox = $('#confirm-modal');
  var confirmModal = confirmBox && window.bootstrap && window.bootstrap.Modal ? new window.bootstrap.Modal(confirmBox) : null;
  var confirming = null;
  document.addEventListener('submit', function (e) {
    var f = e.target;
    if (!f.dataset || !f.dataset.confirm) return;
    if (f.dataset.confirmed === 'yes') { delete f.dataset.confirmed; return; }
    e.preventDefault();
    if (!confirmModal) {
      if (window.confirm(f.dataset.confirm)) { f.dataset.confirmed = 'yes'; f.requestSubmit ? f.requestSubmit() : f.submit(); }
      return;
    }
    confirming = f;
    var variant = f.dataset.confirmVariant || 'danger';
    $('#confirm-title').textContent = f.dataset.confirmTitle || 'Are you sure?';
    $('[data-confirm-body]', confirmBox).textContent = f.dataset.confirm;
    var cta = $('[data-confirm-cta]', confirmBox);
    cta.textContent = f.dataset.confirmCta || 'Continue';
    cta.className = 'btn w-100 btn-' + variant;
    $('.modal-status', confirmBox).className = 'modal-status bg-' + variant;
    $('.modal-body > div', confirmBox).className = 'mb-2 text-' + variant;
    confirmModal.show();
  });
  if (confirmBox) {
    $('[data-confirm-cta]', confirmBox).addEventListener('click', function () {
      var f = confirming;
      confirming = null;
      if (confirmModal) confirmModal.hide();
      if (!f) return;
      f.dataset.confirmed = 'yes';
      if (f.requestSubmit) f.requestSubmit(); else f.submit();
    });
    confirmBox.addEventListener('hidden.bs.modal', function () { confirming = null; });
  }
  $all('select[data-autosubmit]').forEach(function (s) {
    s.addEventListener('change', function () { s.form.submit(); });
  });
  $all('pre.copyable').forEach(function (pre) {
    if (!navigator.clipboard) return;
    var b = h('button', { class: 'btn btn-sm copy', type: 'button', text: 'Copy' });
    b.addEventListener('click', function () {
      navigator.clipboard.writeText(pre.textContent.replace(/Copy$/, '').trim()).then(function () {
        b.textContent = 'Copied';
        setTimeout(function () { b.textContent = 'Copy'; }, 1500);
      });
    });
    pre.appendChild(b);
  });

  /* ---------- frames ----------
   * A Frame is one artifact iframe. An artifact with its own height (design
   * decision 12) is shown exactly that tall, and its content scrolls inside
   * it; others are sized from the bridge's reports. Fixed-width artifacts are
   * scaled to the zoom; others fill the box. */
  var frames = [];
  function Frame(iframe, box, width, opts) {
    this.iframe = iframe;
    this.box = box;
    this.width = width || 0;
    this.height = 0;
    this.scale = 1;
    this.opts = opts || {};
    this.growth = [];
    this.locked = false;
    this.fixed = this.opts.height || 0;
    frames.push(this);
    if (this.width) iframe.style.width = this.width + 'px';
    else iframe.style.width = '100%';
    iframe.style.height = (this.fixed || this.opts.minHeight || 600) + 'px';
    // Until the artifact has drawn itself, its box shows a spinner instead of
    // an empty white frame (the pages draw it loading). The bridge says
    // "painted"; the iframe's load only starts a fallback, as it comes before
    // the artifact's first frame is on screen. An artifact that loaded before
    // this script ran answers the hello with what it already sent.
    var self = this;
    box.classList.add('loading');
    iframe.addEventListener('load', function () {
      clearTimeout(self.fallback);
      self.fallback = setTimeout(function () { self.shown(); }, 500);
    });
    this.post('hello');
  }
  Frame.prototype.painted = function (bg) {
    // Only a plain colour from the computed style: an artifact can't make the
    // app load anything through it.
    if (typeof bg === 'string' && /^rgba?\([\d.,\s%]+\)$/.test(bg)) {
      this.iframe.style.background = bg;
      this.box.style.background = bg;
    }
    // Two frames more, so the artifact's frame is on screen when it shows.
    var self = this;
    requestAnimationFrame(function () { requestAnimationFrame(function () { self.shown(); }); });
  };
  Frame.prototype.shown = function () {
    clearTimeout(this.fallback);
    this.box.classList.remove('loading');
  };
  Frame.prototype.post = function (type, data) {
    var m = data || {};
    m.type = 'artifex:' + type;
    if (this.iframe.contentWindow) this.iframe.contentWindow.postMessage(m, '*');
  };
  Frame.prototype.setScale = function (s) {
    this.scale = s;
    this.layout();
  };
  Frame.prototype.layout = function () {
    var hgt = this.fixed || this.height || (this.opts.minHeight || 600);
    this.iframe.style.height = hgt + 'px';
    if (this.width) {
      this.iframe.style.transform = this.scale === 1 ? '' : 'scale(' + this.scale + ')';
      this.box.style.width = Math.round(this.width * this.scale) + 'px';
      this.box.style.height = Math.round(hgt * this.scale) + 'px';
    } else {
      this.iframe.style.transform = '';
      this.box.style.width = '100%';
      this.box.style.height = hgt + 'px';
    }
  };
  // Some pages size themselves to the window (100vh). Growing the iframe to
  // fit them makes them grow again, without end: stop once the growth repeats.
  Frame.prototype.onSize = function (height) {
    if (this.fixed) { this.layout(); if (this.opts.onSize) this.opts.onSize(this); return; }
    if (this.locked && height > this.height) return;
    var now = Date.now(), d = height - this.height;
    if (this.height && d > 0) {
      this.growth.push({ d: d, t: now });
      this.growth = this.growth.filter(function (g) { return now - g.t < 2000; });
      if (this.growth.length >= 6) {
        this.locked = true;
        height = Math.max(this.height, (this.opts.lockHeight || window.innerHeight));
      }
    }
    this.height = Math.max(50, Math.min(height, 40000));
    this.layout();
    if (this.opts.onSize) this.opts.onSize(this);
  };
  Frame.prototype.reload = function (url) {
    this.locked = false;
    this.growth = [];
    this.box.classList.add('loading');
    this.iframe.src = url;
  };

  window.addEventListener('message', function (e) {
    var m = e.data;
    if (!m || typeof m.type !== 'string' || m.type.indexOf('artifex:') !== 0) return;
    for (var i = 0; i < frames.length; i++) {
      var f = frames[i];
      if (e.source !== f.iframe.contentWindow) continue;
      var t = m.type.slice(8);
      if (t === 'size') f.onSize(m.height);
      if (t === 'painted') f.painted(m.bg);
      if (f.opts.on && f.opts.on[t]) f.opts.on[t](m, f);
      return;
    }
  });

  function zoomControl(sel, key, apply) {
    var z = $(sel);
    if (!z) return function () { return 'fit'; };
    var saved = store(key);
    if (saved && $all('option', z).some(function (o) { return o.value === saved; })) z.value = saved;
    z.addEventListener('change', function () { store(key, z.value); apply(); });
    return function () { return z.value; };
  }

  function appPath(project, path, version, latest) {
    var u = '/p/' + encodeURIComponent(project) + '/' + path.split('/').map(encodeURIComponent).join('/');
    if (!latest && version) u += '?v=' + version;
    return u;
  }

  // An artifact asks to open another file of its project. Anything else it
  // could name (other app pages, "..", query strings) is refused: the
  // sandbox exists so an artifact can't steer the app.
  function safeOpenPath(p) {
    if (typeof p !== 'string' || !p || p.length > 1024) return null;
    var segs = p.split('/');
    for (var i = 0; i < segs.length; i++) {
      if (!segs[i] || segs[i] === '.' || segs[i] === '..' || (i === 0 && segs[i] === '-')) return null;
    }
    return p;
  }
  // The request must also follow a click or key press inside that artifact:
  // the reader is in it (it has focus), and acted just now (HTML's user
  // activation, which a click in a frame gives the page too, and which a
  // script can't fake). Otherwise an artifact could move the reader away the
  // moment it loads, and on a canvas any one artifact could. Links still
  // work: the bridge sends them from the click itself.
  function readerActedIn(frame) {
    if (document.activeElement !== frame.iframe) return false;
    var ua = navigator.userActivation;
    return !ua || ua.isActive;
  }
  function openFromArtifact(cfg) {
    return function (m, frame) {
      if (!readerActedIn(frame)) return;
      var p = safeOpenPath(m.path);
      if (!p) return;
      var hash = typeof m.hash === 'string' && /^#[\w-]*$/.test(m.hash) ? m.hash : '';
      var u = appPath(cfg.project, p, cfg.version, cfg.latest) + hash;
      if (m.newTab) window.open(u, '_blank', 'noopener'); else location.href = u;
    };
  }

  /* ---------- artifact page ---------- */
  function initArtifact() {
    var cfg = JSON.parse($('#frame-data').textContent);
    var stage = $('#stage'), box = $('#frame-box'), iframe = $('#frame');
    var list = $('#thread-list'), composer = $('#composer');
    var toggle = $('#comment-toggle'), hint = $('#comment-hint');
    var threads = [], pending = null, commenting = false, active = 0, positions = {};
    if (!cfg.width) stage.classList.add('fill');

    var frame = new Frame(iframe, box, cfg.width, {
      height: cfg.height || 0,
      minHeight: Math.max(400, stage.clientHeight - 48),
      lockHeight: Math.max(400, stage.clientHeight - 48),
      on: {
        ready: function () { sendPins(); if (commenting) frame.post('mode', { comment: true }); },
        pick: function (m) { startComposer(m); },
        cancel: function () { setCommenting(false); closeComposer(); },
        pin: function (m) { focusThread(m.id, true); },
        placed: function (m) {
          positions = {};
          (m.pins || []).forEach(function (p) { positions[p.id] = p; });
          markMoved();
        },
        open: openFromArtifact(cfg),
        scroll: function (m) { stage.scrollTo({ top: box.offsetTop + m.y * frame.scale - 20, behavior: 'smooth' }); }
      }
    });
    var zoom = zoomControl('#zoom', 'artifex.zoom', fit);
    function fit() {
      if (!cfg.width) { frame.setScale(1); return; }
      var z = zoom();
      var avail = stage.clientWidth - 48;
      frame.setScale(z === 'fit' ? Math.min(1, avail / cfg.width) : parseFloat(z));
    }
    window.addEventListener('resize', fit);
    fit();

    function setCommenting(on) {
      commenting = on;
      toggle.setAttribute('aria-pressed', on ? 'true' : 'false');
      stage.classList.toggle('commenting', on);
      hint.hidden = !on;
      frame.post('mode', { comment: on });
    }
    toggle.addEventListener('click', function () { setCommenting(!commenting); });
    document.addEventListener('keydown', function (e) {
      if (e.target.closest && e.target.closest('input, textarea, select')) return;
      if (e.key === 'c' && !e.metaKey && !e.ctrlKey && !e.altKey) { e.preventDefault(); setCommenting(!commenting); }
      if (e.key === 'Escape' && commenting) { setCommenting(false); closeComposer(); }
    });

    function startComposer(m) {
      pending = m;
      setCommenting(false);
      composer.hidden = false;
      $('#composer-quote').textContent = m.text ? '“' + m.text + '”' : 'A spot on the artifact';
      $('#composer-body').value = '';
      $('#composer-body').focus();
      sendPins();
    }
    function closeComposer() {
      pending = null;
      composer.hidden = true;
      sendPins();
    }
    $('#composer-cancel').addEventListener('click', closeComposer);
    composer.addEventListener('submit', function (e) {
      e.preventDefault();
      var bodyText = $('#composer-body').value.trim();
      if (!bodyText || !pending) return;
      var btn = $('button.btn-primary', composer);
      btn.disabled = true;
      api('POST', '/api/projects/' + encodeURIComponent(cfg.project) + '/threads', {
        path: cfg.path, version: cfg.version, body: bodyText,
        anchor: { selector: pending.selector, text: pending.text, x: pending.x, y: pending.y, ox: pending.ox, oy: pending.oy, width: pending.width }
      }).then(function (t) {
        pending = null;
        composer.hidden = true;
        return loadThreads().then(function () { focusThread(t.id, true); });
      }).catch(function (err) { toast(err.message); }).then(function () { btn.disabled = false; });
    });

    // Open threads get numbers in the order they were started.
    function numbered() {
      var open = threads.filter(function (t) { return t.state === 'open'; }).sort(function (a, b) { return a.id - b.id; });
      var nums = {};
      open.forEach(function (t, i) { nums[t.id] = i + 1; });
      return nums;
    }
    function sendPins() {
      var nums = numbered(), pins = [];
      threads.forEach(function (t) {
        if (t.state !== 'open' && t.id !== active) return;
        var a = t.anchor || {};
        if (a.x == null && !a.selector) return;
        pins.push({ id: t.id, label: nums[t.id] ? String(nums[t.id]) : '✓', state: t.state, selector: a.selector, text: a.text, x: a.x, y: a.y, ox: a.ox, oy: a.oy });
      });
      if (pending) pins.push({ id: -1, label: '+', temp: true, selector: pending.selector, text: pending.text, x: pending.x, y: pending.y, ox: pending.ox, oy: pending.oy });
      frame.post('pins', { pins: pins });
    }

    function loadThreads() {
      return api('GET', '/api/projects/' + encodeURIComponent(cfg.project) + '/threads?path=' + encodeURIComponent(cfg.path)).then(function (res) {
        threads = res.threads || [];
        render();
        sendPins();
      }).catch(function (err) { list.textContent = err.message; });
    }

    function render() {
      var nums = numbered();
      var open = threads.filter(function (t) { return t.state === 'open'; }).sort(function (a, b) { return a.id - b.id; });
      var done = threads.filter(function (t) { return t.state !== 'open'; });
      var count = $('#thread-count');
      count.textContent = open.length + ' open';
      count.hidden = !open.length;
      list.innerHTML = '';
      if (!threads.length) {
        list.appendChild(h('p', { class: 'text-secondary', text: 'No comments yet. Press Comment, then click the spot you want to talk about.' }));
        return;
      }
      open.forEach(function (t) { list.appendChild(card(t, nums[t.id])); });
      if (done.length) {
        var det = h('details', { class: 'resolved-toggle' }, [h('summary', { class: 'text-secondary', text: done.length + ' resolved' })]);
        done.forEach(function (t) { det.appendChild(card(t, 0)); });
        if (done.some(function (t) { return t.id === active; })) det.open = true;
        list.appendChild(det);
      }
      fmtTimes(list);
      markMoved();
    }

    function card(t, n) {
      var el = h('div', { class: 'thread' + (t.state !== 'open' ? ' resolved' : '') + (t.id === active ? ' active' : ''), id: 't' + t.id, 'data-id': t.id });
      var first = t.entries[0] || {};
      var head = h('div', { class: 'head' }, [h('span', { class: 'num', text: n ? String(n) : '✓' }), (t.anchor && t.anchor.text) ? h('span', { class: 'quote', text: '“' + t.anchor.text + '”' }) : h('span', { class: 'quote', text: 'The artifact' })]);
      head.addEventListener('click', function () { focusThread(t.id, false); });
      el.appendChild(head);
      t.entries.forEach(function (e) {
        if (e.kind === 'comment') {
          el.appendChild(h('div', { class: 'entry' }, [
            h('div', null, [h('span', { class: 'who', text: e.author }), e.agent ? h('span', { class: 'badge bg-azure-lt agent', text: 'Agent' }) : null, h('time', { class: 'when', datetime: e.created_at })]),
            h('div', { class: 'text', text: e.body })
          ]));
        } else {
          var label = e.kind === 'resolved' ? 'resolved this' : 'reopened this';
          var kids = [h('span', { class: 'who', text: e.author }), ' ' + label + ' ', h('time', { datetime: e.created_at })];
          if (e.kind === 'resolved' && e.version) {
            kids.push(' · ');
            kids.push(h('a', { href: '/p/' + encodeURIComponent(cfg.project) + '/-/compare?from=' + t.version + '&to=' + e.version + '&path=' + encodeURIComponent(t.path), text: 'see the change' }));
          }
          el.appendChild(h('div', { class: 'entry event' }, kids));
          if (e.body) el.appendChild(h('div', { class: 'entry' }, [h('div', { class: 'text', text: e.body })]));
        }
      });
      if (t.version !== cfg.version && cfg.latest) {
        el.appendChild(h('p', { class: 'note', text: 'Made on an earlier version (' + short(t.version) + ').' }));
      }
      if (t.original_path) el.appendChild(h('p', { class: 'note', text: 'Made when this artifact was ' + t.original_path + '.' }));
      el.appendChild(h('p', { class: 'note moved-note', hidden: 'hidden', text: 'The element it points at has changed; the pin shows where it was.' }));
      var ta = h('textarea', { class: 'form-control', placeholder: 'Reply…', 'aria-label': 'Reply' });
      var reply = h('button', { class: 'btn btn-sm btn-primary', type: 'button', text: 'Reply' });
      var other = h('button', { class: 'btn btn-sm btn-ghost-secondary', type: 'button', text: t.state === 'open' ? 'Resolve' : 'Reopen' });
      reply.addEventListener('click', function () {
        var b = ta.value.trim();
        if (!b) return;
        act(t.id, 'reply', { body: b });
      });
      other.addEventListener('click', function () {
        act(t.id, t.state === 'open' ? 'resolve' : 'reopen', { body: ta.value.trim(), version: t.state === 'open' && cfg.latest ? cfg.version : '' });
      });
      el.appendChild(h('div', { class: 'reply' }, [ta, h('div', { class: 'reply-actions' }, [reply, other])]));
      return el;
    }

    function act(id, action, data) {
      api('POST', '/api/threads/' + id + '/' + action, data).then(function () { return loadThreads(); }).catch(function (err) { toast(err.message); });
    }

    function markMoved() {
      $all('.thread', list).forEach(function (el) {
        var p = positions[el.dataset.id];
        var note = $('.moved-note', el);
        if (note) note.hidden = !(p && p.moved);
      });
    }

    function focusThread(id, fromPin) {
      active = id;
      $all('.thread', list).forEach(function (el) { el.classList.toggle('active', Number(el.dataset.id) === id); });
      var t = threads.find(function (x) { return x.id === id; });
      if (t && t.state !== 'open') { render(); sendPins(); }
      frame.post('focus', { id: id });
      var card = $('#t' + id);
      if (card) {
        var det = card.closest('details');
        if (det) det.open = true;
        card.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
      }
      if (!fromPin) {
        setTimeout(function () {
          var p = positions[id];
          if (p && p.y != null) stage.scrollTo({ top: box.offsetTop + p.y * frame.scale - stage.clientHeight / 3, behavior: 'smooth' });
        }, 80);
      }
    }

    loadThreads().then(function () {
      var m = /^#t(\d+)$/.exec(location.hash);
      if (m) setTimeout(function () { focusThread(Number(m[1]), false); }, 600);
    });

    // Live updates (design decision 21).
    live(cfg.project, {
      version: function (ev) {
        if (!cfg.latest) { toast('There is a newer version of this project.'); return; }
        var mine = (ev.paths || []).indexOf(cfg.path) >= 0;
        var assets = (ev.paths || []).some(function (p) { return !/\.html?$/i.test(p); });
        if (!mine && !assets) return;
        api('GET', '/api/projects/' + encodeURIComponent(cfg.project) + '/frame?path=' + encodeURIComponent(cfg.path)).then(function (f) {
          cfg.version = f.version;
          frame.reload(f.url);
          $('#open-alone').href = f.url;
          toast('Updated to version ' + short(f.version));
          loadThreads();
        }).catch(function () {
          toast('This artifact was moved or deleted in version ' + short(ev.version) + '.');
          loadThreads();
        });
      },
      thread: function (ev) { if (!ev.path || ev.path === cfg.path) loadThreads(); else loadThreads(); }
    });
  }

  /* ---------- canvas page ---------- */
  function initCanvas() {
    var cfgEl = $('#canvas-data');
    if (!cfgEl) return;
    var cfg = JSON.parse(cfgEl.textContent);
    var canvas = $('#canvas');
    if (!canvas) return;
    var list = $all('.board', canvas).map(function (sec) {
      var w = parseInt(sec.dataset.width, 10) || 1440;
      var hgt = parseInt(sec.dataset.height, 10) || 0;
      return new Frame($('iframe', sec), $('.board-box', sec), w, { height: hgt, minHeight: 900, lockHeight: 1200, on: { open: openFromArtifact(cfg) } });
    });
    var zoom = zoomControl('#zoom', 'artifex.canvasZoom', fit);
    function fit() {
      var z = zoom(), avail = canvas.clientWidth - 48;
      list.forEach(function (f) { f.setScale(z === 'fit' ? Math.min(1, avail / f.width) : parseFloat(z)); });
    }
    window.addEventListener('resize', fit);
    fit();
    live(cfg.project, {
      version: function (ev) {
        if (!cfg.latest) { toast('There is a newer version of this project.'); return; }
        var paths = ev.paths || [];
        var shown = list.map(function (f) { return f.iframe.dataset.path; });
        var dir = cfg.dir ? cfg.dir + '/' : '';
        var added = paths.some(function (p) { return /\.html?$/i.test(p) && p.indexOf(dir) === 0 && p.slice(dir.length).indexOf('/') < 0 && shown.indexOf(p) < 0; });
        if (added || paths.some(function (p) { return /folder\.yaml$/.test(p); })) { location.reload(); return; }
        var assets = paths.some(function (p) { return !/\.html?$/i.test(p); });
        list.forEach(function (f) {
          var p = f.iframe.dataset.path;
          if (!assets && paths.indexOf(p) < 0) return;
          api('GET', '/api/projects/' + encodeURIComponent(cfg.project) + '/frame?path=' + encodeURIComponent(p)).then(function (r) {
            f.reload(r.url);
          }).catch(function () { location.reload(); });
        });
        toast('Updated to version ' + short(ev.version));
      }
    });
  }

  /* ---------- compare page ---------- */
  function initCompare() {
    var c = $('#compare');
    if (!c) return;
    var w = parseInt(c.dataset.width, 10) || 0;
    var hgt = parseInt(c.dataset.height, 10) || 0;
    var list = $all('figure', c).map(function (fig) {
      var iframe = $('iframe', fig);
      if (!iframe) return null;
      return new Frame(iframe, $('.board-box', fig), w || 1280, { height: hgt, minHeight: 600, lockHeight: 900 });
    }).filter(Boolean);
    function fit() {
      list.forEach(function (f) { f.setScale(Math.min(1, f.box.parentElement.clientWidth / f.width)); });
    }
    window.addEventListener('resize', fit);
    fit();
  }

  /* ---------- live events ---------- */
  function live(project, handlers) {
    if (!window.EventSource) return;
    var es = new EventSource('/api/projects/' + encodeURIComponent(project) + '/events');
    Object.keys(handlers).forEach(function (type) {
      es.addEventListener(type, function (e) {
        try { handlers[type](JSON.parse(e.data)); } catch (err) { /* ignore a bad event */ }
      });
    });
  }

  if (page === 'artifact') initArtifact();
  if (page === 'canvas') initCanvas();
  if (page === 'compare') initCompare();
})();
