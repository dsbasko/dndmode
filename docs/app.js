/* dndmode site — page logic.
   A dependency-free port of the design's page script and of the three
   design-system components the page uses (LangSwitch, ThemeToggle, Terminal):
   language switch, theme toggle, "Copy" buttons and the matrix rain that
   mirrors internal/macos/cocoa/matrixview_darwin.m. No network calls. */
(function () {
  'use strict';

  var root = document.documentElement;

  /* ── Language ──────────────────────────────────────────────────────── */
  var LANG_KEY = 'ds-lang';
  var TITLES = {
    ru: 'dndmode: заблокируй оставленный Mac, не останавливая работу',
    en: 'dndmode: lock your unattended Mac without killing the work'
  };

  function readLang() {
    var stored = null;
    try { stored = window.localStorage.getItem(LANG_KEY); } catch (e) { /* private mode */ }
    return stored === 'en' || stored === 'ru' ? stored : 'ru';
  }

  function applyLang(lang) {
    root.setAttribute('lang', lang);
    try { window.localStorage.setItem(LANG_KEY, lang); } catch (e) { /* the choice just does not persist */ }
    document.title = TITLES[lang];
    var segs = document.querySelectorAll('[data-lang-pick]');
    for (var i = 0; i < segs.length; i++) {
      var on = segs[i].getAttribute('data-lang-pick') === lang;
      segs[i].setAttribute('aria-checked', on ? 'true' : 'false');
      segs[i].classList.toggle('is-active', on);
    }
  }

  /* ── Theme: light · dark · system ──────────────────────────────────── */
  var THEME_KEY = 'ds-theme';

  function readTheme() {
    try {
      var v = window.localStorage.getItem(THEME_KEY);
      return v === 'light' || v === 'dark' ? v : 'system';
    } catch (e) {
      return 'system';
    }
  }

  function applyTheme(mode) {
    if (mode === 'system') root.removeAttribute('data-theme');
    else root.setAttribute('data-theme', mode);
    try {
      if (mode === 'system') window.localStorage.removeItem(THEME_KEY);
      else window.localStorage.setItem(THEME_KEY, mode);
    } catch (e) { /* the choice just does not persist */ }
    var segs = document.querySelectorAll('[data-theme-pick]');
    for (var i = 0; i < segs.length; i++) {
      var on = segs[i].getAttribute('data-theme-pick') === mode;
      segs[i].setAttribute('aria-checked', on ? 'true' : 'false');
      segs[i].classList.toggle('is-active', on);
    }
  }

  /* ── Terminal "Copy" buttons ───────────────────────────────────────── */
  function bindCopy(btn) {
    btn.addEventListener('click', function () {
      var text = btn.getAttribute('data-copy') || '';
      if (!(navigator.clipboard && navigator.clipboard.writeText)) return;
      navigator.clipboard.writeText(text).then(function () {
        btn.classList.add('is-done');
        btn.textContent = 'Copied';
        window.setTimeout(function () {
          btn.classList.remove('is-done');
          btn.textContent = 'Copy';
        }, 1500);
      }, function () { /* clipboard refused — stay quiet */ });
    });
  }

  /* ── Matrix rain ───────────────────────────────────────────────────────
     Port of internal/macos/cocoa/matrixview_darwin.m: fixed grid of stable
     glyphs, a continuous head per column, brightness as a smooth function of
     distance to the head. Sizes/cadence/palette are the file's constants. */
  function createRain(canvas, scale) {
    var SIZES = [24, 34, 46].map(function (s) { return s * scale; });
    var CELL_H = 1.18;
    var CELL_W = 0.92;
    var PALETTE = [[0, 1, 0.25], [0.45, 1, 0.2], [0, 0.9, 0.45], [0.25, 0.85, 0.3]];
    var FRAME = 1000 / 30;
    var ctx = canvas.getContext('2d');
    var animated = !(window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches);
    var cols = [];
    var w = 0;
    var h = 0;
    var raf = 0;
    var last = 0;

    function glyph() {
      return Math.random() < 0.25
        ? String.fromCharCode(48 + Math.floor(Math.random() * 10))
        : String.fromCharCode(0xff66 + Math.floor(Math.random() * (0xff9d - 0xff66 + 1)));
    }

    function respawn(col) {
      col.trail = 8 + Math.floor(Math.random() * 13);
      col.speed = 0.12 + Math.floor(Math.random() * 29) / 100;
      col.head = -Math.floor(Math.random() * (col.rows + col.trail + 1));
      col.glyphs = Array.from({ length: col.rows }, glyph);
    }

    function build() {
      var r = canvas.getBoundingClientRect();
      if (!r.width || !r.height) return false;
      var dpr = Math.min(window.devicePixelRatio || 1, 2);
      w = r.width; h = r.height;
      canvas.width = Math.round(w * dpr);
      canvas.height = Math.round(h * dpr);
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
      ctx.textBaseline = 'top';
      cols = [];
      var x = 0;
      while (x < w) {
        var size = SIZES[Math.floor(Math.random() * SIZES.length)];
        var cellH = size * CELL_H;
        var col = {
          x: x,
          size: size,
          cellH: cellH,
          font: '500 ' + size.toFixed(1) + 'px ui-monospace, SFMono-Regular, Menlo, monospace',
          hue: PALETTE[Math.floor(Math.random() * PALETTE.length)],
          alpha: 0.45 + Math.floor(Math.random() * 56) / 100,
          rows: Math.floor(h / cellH) + 1
        };
        respawn(col);
        cols.push(col);
        x += size * CELL_W;
      }
      return true;
    }

    function step() {
      for (var i = 0; i < cols.length; i++) {
        var col = cols[i];
        var prev = Math.floor(col.head);
        col.head += col.speed;
        if (col.head - col.trail > col.rows) { respawn(col); continue; }
        var cur = Math.floor(col.head);
        for (var r = prev + 1; r <= cur; r++) if (r >= 0 && r < col.rows) col.glyphs[r] = glyph();
      }
    }

    function draw() {
      ctx.fillStyle = '#000';
      ctx.fillRect(0, 0, w, h);
      for (var i = 0; i < cols.length; i++) {
        var col = cols[i];
        ctx.font = col.font;
        var lo = Math.max(0, Math.floor(col.head - col.trail));
        var hi = Math.min(col.rows - 1, Math.floor(col.head));
        for (var r = lo; r <= hi; r++) {
          var d = col.head - r;
          if (d < 0 || d >= col.trail) continue;
          var bright = (1 - d / col.trail) * col.alpha;
          var wh = d < 1.5 ? ((1.5 - d) / 1.5) * 0.85 : 0;
          var c = function (v, add) { return Math.round(Math.min(1, col.hue[v] + add) * bright * 255); };
          ctx.fillStyle = 'rgb(' + c(0, wh) + ',' + c(1, wh * 0.3) + ',' + c(2, wh) + ')';
          ctx.fillText(col.glyphs[r], col.x, r * col.cellH);
        }
      }
    }

    function tick(t) {
      raf = window.requestAnimationFrame(tick);
      if (t - last < FRAME) return;
      last = t;
      step();
      draw();
    }

    var builtW = 0;
    var builtH = 0;
    function start() {
      var r = canvas.getBoundingClientRect();
      if (!r.width || !r.height) return;
      builtW = r.width;
      builtH = r.height;
      if (!build()) return;
      for (var i = 0; i < 150; i++) step(); // pre-roll so the first painted frame is already raining
      draw();
      window.cancelAnimationFrame(raf);
      if (animated) raf = window.requestAnimationFrame(tick);
    }

    start();
    if (document.fonts && document.fonts.ready) document.fonts.ready.then(start);
    var ro = window.ResizeObserver ? new ResizeObserver(function () { start(); }) : null;
    if (ro) ro.observe(canvas);
    // Layout can settle after stylesheets land, and in a hidden document neither
    // rAF nor ResizeObserver delivers — so poll the box against what was built.
    var watch = 0;
    var guard = window.setInterval(function () {
      var r = canvas.getBoundingClientRect();
      if (r.width && r.height && (Math.abs(r.width - builtW) > 0.5 || Math.abs(r.height - builtH) > 0.5)) start();
      if (++watch > 40) window.clearInterval(guard);
    }, 250);
  }

  /* ── Wire-up ───────────────────────────────────────────────────────── */
  function init() {
    applyLang(readLang());
    applyTheme(readTheme());

    var i;
    var langSegs = document.querySelectorAll('[data-lang-pick]');
    for (i = 0; i < langSegs.length; i++) {
      langSegs[i].addEventListener('click', function () { applyLang(this.getAttribute('data-lang-pick')); });
    }
    var themeSegs = document.querySelectorAll('[data-theme-pick]');
    for (i = 0; i < themeSegs.length; i++) {
      themeSegs[i].addEventListener('click', function () { applyTheme(this.getAttribute('data-theme-pick')); });
    }
    var copies = document.querySelectorAll('[data-copy]');
    for (i = 0; i < copies.length; i++) bindCopy(copies[i]);

    var RAIN_SCALE = { hero: 0.46, card: 0.24 };
    var rains = document.querySelectorAll('canvas[data-rain]');
    for (i = 0; i < rains.length; i++) {
      var scale = RAIN_SCALE[rains[i].getAttribute('data-rain')];
      if (scale) createRain(rains[i], scale);
    }
  }

  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', init);
  else init();
})();
