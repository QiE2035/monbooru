'use strict';

// Chord state for leader-key navigation. The next key within
// chordTimeoutMs resolves the chord; anything else cancels.
var _chordNode = null;
var _chordLabel = '';
var _chordTimer = null;
var _chordHint = null;
var chordTimeoutMs = 500;

// Chord tree. A leaf is a string URL (navigated to) or a function run with
// no args; a nested map is a sub-chord whose own keys resolve the next
// step. `g` leads navigation (`g c a` categories, `g c o` collections).
// `e` leads detail-page field edits: `s` opens the add-source dialog, `c`
// opens the add-to-collection dialog, and a digit edits that Nth
// collection. On pages without the matching control the chord no-ops.
var chordMap = {
  g: {
    g: '/',
    i: '/?q=inbox:true',
    c: {
      a: '/categories',
      o: '/collections',
    },
    t: '/tags',
    s: '/settings',
    h: '/help',
  },
  e: {
    s: function () { var b = document.querySelector('.btn-add-source'); if (b) b.click(); },
    c: function () { var b = document.querySelector('.btn-add-collection'); if (b) b.click(); },
  },
};

// editCollection opens the edit dialog for the Nth collection listed on the
// detail page (1-based, render order). Returns false when out of range.
function editCollection(n) {
  var btns = document.querySelectorAll('.btn-edit-collection');
  if (n < 1 || n > btns.length) return false;
  btns[n - 1].click();
  return true;
}

function clearChord() {
  _chordNode = null;
  _chordLabel = '';
  if (_chordTimer) { clearTimeout(_chordTimer); _chordTimer = null; }
  if (_chordHint && _chordHint.parentNode) _chordHint.parentNode.removeChild(_chordHint);
  _chordHint = null;
}

// enterChord arms a (sub-)chord: the next key is resolved against `node`,
// expiring after chordTimeoutMs. `label` is the keys pressed so far.
function enterChord(node, label) {
  _chordNode = node;
  _chordLabel = label;
  showChordHint(node, label);
  if (_chordTimer) clearTimeout(_chordTimer);
  _chordTimer = setTimeout(clearChord, chordTimeoutMs);
}

function showChordHint(node, label) {
  if (_chordHint && _chordHint.parentNode) _chordHint.parentNode.removeChild(_chordHint);
  _chordHint = document.createElement('div');
  _chordHint.className = 'chord-leader-hint';
  var keys = Object.keys(node || {});
  if (node === chordMap.e) {
    var nCol = document.querySelectorAll('.btn-edit-collection').length;
    for (var i = 1; i <= nCol && i <= 9; i++) keys.push(String(i));
  }
  keys.sort();
  _chordHint.textContent = label + ' → ' + keys.join(' / ');
  document.body.appendChild(_chordHint);
}

// pageScroller returns the nearest ancestor that actually scrolls. The
// layout pins body to overflow:hidden and lets <main> own the scrolling
// so the topbar / footer stay sticky. Falls back to document scrolling.
function pageScroller(el) {
  var node = el && el.parentElement;
  while (node) {
    var ov = getComputedStyle(node).overflowY;
    if ((ov === 'auto' || ov === 'scroll') && node.scrollHeight > node.clientHeight) {
      return node;
    }
    node = node.parentElement;
  }
  return document.scrollingElement || document.documentElement;
}

function scrollPageTo(el, top) {
  var s = pageScroller(el);
  if (s === document.documentElement || s === document.body || s === document.scrollingElement) {
    window.scrollTo({ top: top, behavior: 'smooth' });
  } else {
    s.scrollTo({ top: top, behavior: 'smooth' });
  }
}

function scrollToTop(el) { scrollPageTo(el, 0); }
function scrollToBottom(el) {
  var s = pageScroller(el);
  scrollPageTo(el, s.scrollHeight);
}

// setFocused moves a keyboard cursor: strip .focused off every element in
// `items`, mark items[idx], and keep it in view. `block` overrides the
// scrollIntoView alignment (default 'nearest').
function setFocused(items, idx, block) {
  items.forEach(function(el) { el.classList.remove('focused'); });
  items[idx].classList.add('focused');
  items[idx].scrollIntoView({ block: block || 'nearest' });
}

// Move-cursor / open-card helpers reused by both arrow keys and h/j/k/l.
// When dy != 0 and the would-be index walks off the top/bottom row, the
// scroll container scrolls to the top / bottom (search bar above,
// pagination below) instead of clamping silently. Horizontal walk-off
// keeps the existing clamp - side-scrolling has no useful target here.
function moveGridCursor(dx, dy) {
  var cards = Array.from(document.querySelectorAll('.thumb-card'));
  if (cards.length === 0) return false;
  var focused = document.querySelector('.thumb-card.focused');
  var idx = focused ? cards.indexOf(focused) : -1;
  if (idx < 0) {
    idx = 0;
  } else {
    var cols = 1;
    var grid = document.querySelector('.thumb-grid');
    if (grid && cards[0]) {
      var cardW = cards[0].offsetWidth;
      if (cardW > 0) cols = Math.max(1, Math.round(grid.offsetWidth / cardW));
    }
    var newIdx = idx + dx + dy * cols;
    if (dy < 0 && newIdx < 0) {
      scrollToTop(cards[idx]);
      return true;
    }
    if (dy > 0 && newIdx > cards.length - 1) {
      scrollToBottom(cards[idx]);
      return true;
    }
    idx = Math.max(0, Math.min(cards.length - 1, newIdx));
  }
  setFocused(cards, idx);
  return true;
}

function jumpGridCursor(target) {
  var cards = Array.from(document.querySelectorAll('.thumb-card'));
  if (cards.length === 0) return false;
  setFocused(cards, target === 'first' ? 0 : cards.length - 1);
  return true;
}

function clickPagination(needle) {
  var links = document.querySelectorAll('.pagination a');
  for (var i = 0; i < links.length; i++) {
    if (links[i].textContent.indexOf(needle) >= 0) { links[i].click(); return true; }
  }
  return false;
}

// handlePaginationKey maps [ ] G p onto the shared pagination controls
// (used verbatim by the tags page and the gallery). Returns true when the
// key was consumed - the matching control exists and was clicked.
function handlePaginationKey(e) {
  if (e.key === '[') return clickPagination('Prev');
  if (e.key === ']') return clickPagination('Next');
  if (e.key === 'G') return clickPagination('Last');
  if (e.key === 'p') {
    var jp = document.querySelector('.page-jump');
    if (jp) { jp.click(); return true; }
  }
  return false;
}

// detailRefBack is the history-back step of the back chain alone: true when
// the detail page carries data-ref (similar-click chain) and history has a
// predecessor. Shared with the .back-link click handler, which must leave
// the link's default navigation alive on cold loads (direct URL, bookmark).
function detailRefBack() {
  var detailPage = document.getElementById('detail-page');
  if (detailPage && detailPage.dataset.ref && history.length > 1) { history.back(); return true; }
  return false;
}

// detailBack walks the full back chain shared by Escape, Backspace, and the
// pages-grid Escape: unwind history one step on a data-ref page, else click
// the .back-link (its href carries the return query), else '/'. The pages
// grid has no #detail-page, so only the link walk runs there.
function detailBack() {
  if (detailRefBack()) return;
  var backLink = document.querySelector('.back-link');
  if (backLink) backLink.click();
  else window.location.href = '/';
}

// Detail-page prev/next image. Falls through when no nav arrow is present.
function navDetailImage(direction) {
  var sel = direction === 'prev'
    ? '.nav-arrow[title^="Previous image"]'
    : '.nav-arrow[title^="Next image"]';
  var a = document.querySelector(sel);
  if (a && a.href) { window.location.href = a.href; return true; }
  return false;
}

// Cycle the rating ceiling level. delta=+1 picks a stricter level (less
// permissive); delta=-1 picks a more permissive one. Fires the matching
// inactive footer-rating-link form.
function cycleRatingCeiling(delta) {
  var labels = document.querySelectorAll('.footer-rating-active, .footer-rating-link');
  // Active sits among the cluster as the [bracketed] span; siblings are
  // <button class="footer-rating-link">. Find the active index from the
  // active span's text and submit the neighbour's parent form.
  var active = document.querySelector('.footer-rating-active');
  if (!active) return false;
  var siblings = active.parentNode.parentNode.querySelectorAll('.footer-rating-active, .footer-rating-link');
  var ordered = Array.prototype.slice.call(siblings);
  var idx = ordered.indexOf(active);
  if (idx < 0) return false;
  var next = idx + delta;
  if (next < 0 || next >= ordered.length) return false;
  var target = ordered[next];
  if (target.tagName === 'BUTTON') {
    var f = target.closest('form');
    if (f) { f.requestSubmit ? f.requestSubmit() : f.submit(); return true; }
  }
  return false;
}

// Cycle the gallery sort select between newest -> filesize -> order -> random.
function cycleSort() {
  var sortEl = document.getElementById('search-sort');
  var form = document.getElementById('search-form');
  if (!sortEl || !form) return false;
  var order = ['newest', 'filesize', 'order', 'random'];
  var i = order.indexOf(sortEl.value);
  var next = order[(i + 1) % order.length];
  if (next === 'random') {
    applyRandomSort();
    return true;
  }
  var opt = sortEl.querySelector('option[value="' + next + '"]');
  if (!opt) {
    opt = document.createElement('option');
    opt.value = next;
    opt.textContent = next;
    sortEl.appendChild(opt);
  }
  sortEl.value = next;
  form.dispatchEvent(new Event('submit', { bubbles: true }));
  return true;
}

function flipSortDirection() {
  var orderEl = document.querySelector('#search-form select[name="order"]');
  var form = document.getElementById('search-form');
  if (!orderEl || !form) return false;
  orderEl.value = orderEl.value === 'asc' ? 'desc' : 'asc';
  form.dispatchEvent(new Event('submit', { bubbles: true }));
  return true;
}

function focusFirstSelector(selectors) {
  for (var i = 0; i < selectors.length; i++) {
    var el = document.querySelector(selectors[i]);
    if (el) { el.focus(); if (el.select) el.select(); return true; }
  }
  return false;
}

// Surface helpers; keep the keydown router readable.
function isGalleryPage()    { return !!document.querySelector('.thumb-grid, #gallery-grid'); }
function isDetailPage()     { return !!document.getElementById('detail-page'); }
function isTagsPage()       { return !!document.getElementById('tags-page'); }
function isCategoriesPage() { return !!document.getElementById('categories-page'); }
function isSettingsPage()   { return !!document.getElementById('settings-page'); }
function batchBarVisible() {
  var bar = document.getElementById('batch-bar');
  return !!(bar && bar.classList.contains('visible'));
}

// Pages-grid keymap. Arrow keys walk the cell focus across rows / columns
// (cols computed from the rendered card width like the gallery grid),
// Enter opens the focused page in the reader, Home / End jump.
function handlePagesGridKey(e) {
  var page = document.getElementById('pages-grid-page');
  if (!page) return false;
  if (e.target.tagName.toLowerCase() === 'input' || e.target.isContentEditable) return false;
  if (document.querySelector('dialog[open]')) return false;
  var cells = Array.from(page.querySelectorAll('.manga-page-cell'));
  if (cells.length === 0) return false;
  var focused = page.querySelector('.manga-page-cell.focused');
  var idx = focused ? cells.indexOf(focused) : -1;
  function moveTo(target) {
    target = Math.max(0, Math.min(cells.length - 1, target));
    setFocused(cells, target);
  }
  function cols() {
    if (cells.length === 0) return 1;
    // Count cells whose offsetTop matches the first cell's top - that's
    // the rendered column count regardless of grid gap. The simple
    // gridWidth/cellWidth heuristic overshoots when the gap is set
    // because cellWidth excludes the gap, leaving ArrowDown short by
    // one column on every row boundary.
    var firstTop = cells[0].offsetTop;
    var n = 0;
    for (var i = 0; i < cells.length; i++) {
      if (cells[i].offsetTop !== firstTop) break;
      n++;
    }
    return Math.max(1, n);
  }
  function tryVerticalScroll(target) {
    if (idx < 0) return false;
    if (target < 0) {
      scrollToTop(cells[idx]);
      return true;
    }
    if (target > cells.length - 1) {
      scrollToBottom(cells[idx]);
      return true;
    }
    return false;
  }
  switch (e.key) {
    case 'ArrowRight':
      e.preventDefault(); moveTo(idx < 0 ? 0 : idx + 1); return true;
    case 'ArrowLeft':
      e.preventDefault(); moveTo(idx < 0 ? 0 : idx - 1); return true;
    case 'ArrowDown':
      e.preventDefault();
      if (tryVerticalScroll(idx + cols())) return true;
      moveTo(idx < 0 ? 0 : idx + cols()); return true;
    case 'ArrowUp':
      e.preventDefault();
      if (tryVerticalScroll(idx - cols())) return true;
      moveTo(idx < 0 ? 0 : idx - cols()); return true;
    case 'Home':
      e.preventDefault(); moveTo(0); return true;
    case 'End':
      e.preventDefault(); moveTo(cells.length - 1); return true;
    case 'Enter':
      if (idx < 0) return false;
      var link = cells[idx].querySelector('a');
      if (link && link.href) { window.location.href = link.href; return true; }
      return false;
  }
  return false;
}

// Image viewer (lightbox). Triggered from the detail page by clicking
// the still-image, the .btn-view button, or pressing 'v'; from the
// manga reader by clicking a page or pressing 'v'. Uses a <dialog> so
// the browser handles Esc / scroll lock natively; the keymap below
// runs first in the global router so wheel-driven zoom and pan keys
// don't leak into detail-page bindings.
var lightbox = (function () {
  var dlg = null, img = null, stage = null, zoomLbl = null, openLink = null;
  var scale = 1, tx = 0, ty = 0;
  var panning = false, px0 = 0, py0 = 0, tx0 = 0, ty0 = 0;
  // pressMoved guards click-to-close so a drag-to-pan never dismisses the
  // viewer. Squared 4px threshold matches the browser's own click-vs-drag
  // tolerance, which is what users feel.
  var pressMoved = false;
  var bound = false;

  function clamp(v, lo, hi) { return Math.max(lo, Math.min(hi, v)); }
  function apply() {
    if (!img) return;
    img.style.transform = 'translate(' + tx + 'px,' + ty + 'px) scale(' + scale + ')';
    if (zoomLbl) zoomLbl.textContent = Math.round(scale * 100) + '%';
  }
  function reset() { scale = 1; tx = 0; ty = 0; apply(); }
  function zoomAt(cx, cy, factor) {
    var ns = clamp(scale * factor, 0.5, 4);
    var k = ns / scale;
    tx = cx - k * (cx - tx);
    ty = cy - k * (cy - ty);
    scale = ns;
    apply();
  }
  // The IMG element fills the stage (CSS width/height: 100%) so a small
  // image scales up; object-fit: contain letterboxes the natural-aspect
  // content inside that box. Click-to-close and the 1:1 zoom math both
  // need the visible content rect, not the element rect, otherwise the
  // letterbox counts as "on the image".
  function contentRect() {
    if (!img) return null;
    var r = img.getBoundingClientRect();
    if (!r.width || !r.height) return null;
    var iw = img.naturalWidth, ih = img.naturalHeight;
    if (!iw || !ih) return r;
    var ia = iw / ih, ba = r.width / r.height;
    var w, h;
    if (ia > ba) { w = r.width; h = r.width / ia; }
    else { h = r.height; w = r.height * ia; }
    var left = r.left + (r.width - w) / 2;
    var top = r.top + (r.height - h) / 2;
    return { left: left, top: top, right: left + w, bottom: top + h, width: w, height: h };
  }
  // 1:1 means one image pixel renders to one device pixel. The currently
  // displayed width is naturalWidth at scale=1 multiplied by the CSS fit,
  // so we invert through whatever scale is active right now to land on
  // naturalWidth without re-reading the un-transformed bounding box.
  function oneToOne() {
    if (!img || !img.naturalWidth) return;
    var r = contentRect();
    if (!r || !r.width) return;
    var ns = clamp(img.naturalWidth * scale / r.width, 0.5, 4);
    var k = ns / scale;
    tx = k * tx; ty = k * ty;
    scale = ns;
    apply();
  }
  function onWheel(e) {
    e.preventDefault();
    var sr = stage.getBoundingClientRect();
    var cx = e.clientX - (sr.left + sr.width / 2);
    var cy = e.clientY - (sr.top + sr.height / 2);
    zoomAt(cx, cy, e.deltaY < 0 ? 1.2 : 1 / 1.2);
  }
  function onPointerDown(e) {
    if (e.button !== 0) return;
    stage.setPointerCapture(e.pointerId);
    panning = true;
    pressMoved = false;
    px0 = e.clientX; py0 = e.clientY;
    tx0 = tx; ty0 = ty;
    stage.classList.add('grabbing');
  }
  function onPointerMove(e) {
    if (!panning) return;
    if (!pressMoved) {
      var dx = e.clientX - px0, dy = e.clientY - py0;
      if (dx * dx + dy * dy > 16) pressMoved = true;
    }
    tx = tx0 + (e.clientX - px0);
    ty = ty0 + (e.clientY - py0);
    apply();
  }
  function onPointerEnd(e) {
    if (!panning) return;
    panning = false;
    stage.releasePointerCapture(e.pointerId);
    stage.classList.remove('grabbing');
    // Click-to-close: release without panning, off the image itself.
    // Releases *on* the image fall through so dblclick still toggles
    // fit / 1:1 (the first click of a dblclick must not dismiss).
    if (pressMoved || !img) return;
    var r = contentRect();
    if (!r || !r.width || e.clientX < r.left || e.clientX > r.right || e.clientY < r.top || e.clientY > r.bottom) {
      dlg.close();
    }
  }
  function onDblClick() {
    if (Math.abs(scale - 1) < 0.01) oneToOne();
    else reset();
  }
  function ensure() {
    if (dlg) return true;
    dlg = document.getElementById('image-lightbox');
    if (!dlg) return false;
    img = document.getElementById('lightbox-img');
    stage = document.getElementById('lightbox-stage');
    zoomLbl = document.getElementById('lightbox-zoom');
    openLink = document.getElementById('lightbox-open');
    if (!bound && stage) {
      stage.addEventListener('wheel', onWheel, { passive: false });
      stage.addEventListener('pointerdown', onPointerDown);
      stage.addEventListener('pointermove', onPointerMove);
      stage.addEventListener('pointerup', onPointerEnd);
      stage.addEventListener('pointercancel', onPointerEnd);
      stage.addEventListener('dblclick', onDblClick);
      dlg.addEventListener('close', reset);
      var closeBtn = document.getElementById('lightbox-close');
      if (closeBtn) closeBtn.addEventListener('click', function () { dlg.close(); });
      bound = true;
    }
    return true;
  }
  return {
    open: function (e, src) {
      // Pass-through for modifier / middle clicks so target=_blank still
      // opens the raw file in a new tab. Returning truthy keeps the
      // anchor's default navigation alive.
      if (e && (e.button === 1 || e.ctrlKey || e.metaKey || e.shiftKey)) return true;
      if (!ensure()) return true;
      if (e && e.preventDefault) e.preventDefault();
      img.src = src;
      if (openLink) openLink.href = src;
      reset();
      if (!dlg.open) dlg.showModal();
      return false;
    },
    handleKey: function (e) {
      if (!dlg || !dlg.open) return false;
      var t = e.target.tagName.toLowerCase();
      if (t === 'input' || t === 'textarea' || e.target.isContentEditable) return false;
      switch (e.key) {
        case '+': case '=':
          e.preventDefault(); zoomAt(0, 0, 1.2); return true;
        case '-':
          e.preventDefault(); zoomAt(0, 0, 1 / 1.2); return true;
        case '0':
          e.preventDefault(); reset(); return true;
        case '1':
          e.preventDefault(); oneToOne(); return true;
      }
      return false;
    },
  };
})();

// Exposed for inline onclick on the still-image link and the
// .btn-view button in .detail-actions.
function openLightbox(e, src) { return lightbox.open(e, src); }

// openReaderJumpDialog opens the reader's page-jump dialog with the input
// focused and pre-selected. Shared by the p key and the counter click.
// Returns false when the dialog isn't on the page.
function openReaderJumpDialog() {
  var dlg = document.getElementById('reader-jump-dialog');
  if (!dlg) return false;
  dlg.showModal();
  var inp = document.getElementById('reader-jump-input');
  if (inp) { inp.focus(); inp.select(); }
  return true;
}

// Reader keymap. The reader is a separate <body class="reader-body"> that
// hides every gallery / detail control via CSS, so its keys must run
// before the global keymap and short-circuit it; otherwise the gallery's
// h/l/space bindings would steal the reader's prev/next.
function handleReaderKey(e) {
  var reader = document.getElementById('reader');
  if (!reader) return false;
  if (e.target.tagName.toLowerCase() === 'input') return false;
  if (document.querySelector('dialog[open]')) {
    if (e.key === 'Escape') {
      // Browser handles dialog Esc; let it through.
    }
    return false;
  }
  var page = parseInt(reader.dataset.page, 10) || 1;
  var total = parseInt(reader.dataset.total, 10) || 1;
  var imgID = reader.dataset.imageId;
  var detail = reader.dataset.detailUrl || ('/images/' + imgID);
  function go(p) {
    if (p < 1) p = 1; if (p > total) p = total;
    if (p === page) return;
    // Preserve the existing query (back_q / from=pages / etc) so a
    // keyboard page-flip stays in the same context as a click on the
    // template-emitted prev/next anchors. Without from=pages the
    // next Esc would land on the detail page instead of the pages grid.
    var url = new URL(window.location.href);
    url.searchParams.set('page', String(p));
    url.hash = '';
    window.location.href = url.pathname + url.search;
  }
  switch (e.key) {
    case 'ArrowRight': case 'l': case ' ':
      e.preventDefault(); go(page + 1); return true;
    case 'ArrowLeft': case 'h':
      e.preventDefault(); go(page - 1); return true;
    case 'Home':
      e.preventDefault(); go(1); return true;
    case 'End':
      e.preventDefault(); go(total); return true;
    case 'p':
      if (openReaderJumpDialog()) e.preventDefault();
      return true;
    case 'P':
      var pagesLink = document.querySelector('.reader-pages');
      if (pagesLink && pagesLink.href) {
        e.preventDefault();
        window.location.href = pagesLink.href;
        return true;
      }
      return false;
    case 'o':
      e.preventDefault();
      var openLink = document.querySelector('.reader-open');
      if (openLink) openLink.click();
      return true;
    case 'v':
      var pageLink = document.querySelector('.reader-page-link');
      if (pageLink) {
        e.preventDefault();
        openLightbox(null, pageLink.href);
        return true;
      }
      return false;
    case 'Escape': case 'Backspace':
      e.preventDefault();
      window.location.href = detail;
      return true;
  }
  return false;
}

// Keyboard navigation
document.addEventListener('keydown', function(e) {
  var tag = e.target.tagName.toLowerCase();
  var isInput = tag === 'input' || tag === 'textarea' || tag === 'select' || e.target.isContentEditable;

  if (lightbox.handleKey(e)) return;
  if (handleReaderKey(e)) return;
  if (handlePagesGridKey(e)) return;

  // 'Escape' → blur input first; once nothing is focused, walk the chain.
  if (e.key === 'Escape') {
    if (isInput) { e.target.blur(); return; }
    if (document.querySelector('dialog[open]')) return;
    if (batchBarVisible()) {
      e.preventDefault();
      clearSelection();
      return;
    }
    if (document.body.classList.contains('tag-focus') ||
        document.querySelector('#image-tags .tag-item.focused')) {
      e.preventDefault();
      exitTagFocusMode();
      return;
    }
    if (isDetailPage() || document.getElementById('pages-grid-page')) {
      e.preventDefault();
      detailBack();
      return;
    }
    // Back-navigation fallback: respect any page handler that already
    // claimed this Esc (e.g. the relations-session compare-slider close).
    // Without this guard the data-esc-back match below would navigate on
    // top of a slider dismissal.
    if (e.defaultPrevented) return;
    var escBackEl = document.querySelector('[data-esc-back]');
    if (escBackEl) {
      var dest = escBackEl.dataset.escBack;
      if (dest) {
        e.preventDefault();
        window.location.href = dest;
        return;
      }
    }
    return;
  }

  // While a dialog is open, swallow gallery / detail keyboard shortcuts so
  // arrow-key grid nav, page navigation, single-key actions, and the chord
  // state machine don't fire under the user's input. Inputs, selects, and
  // textareas inside the dialog still receive their own keystrokes because
  // isInput is true for those targets.
  if (!isInput && document.querySelector('dialog[open]')) {
    // ? closes the shortcuts overlay so the same key is a toggle.
    if (e.key === '?' && document.getElementById('shortcuts-help') && document.getElementById('shortcuts-help').open) {
      e.preventDefault();
      document.getElementById('shortcuts-help').close();
    }
    return;
  }

  if (isInput) return;

  // Chord in progress: this key is the next step. A nested map descends one
  // level (sub-chord); a string navigates; a function runs.
  if (_chordNode) {
    var node = _chordNode;
    var label = _chordLabel;
    var next = node[e.key];
    if (next && typeof next === 'object') {
      e.preventDefault();
      enterChord(next, label + ' ' + e.key);
      return;
    }
    clearChord();
    if (next !== undefined) {
      e.preventDefault();
      if (typeof next === 'string') window.location.href = next;
      else next();
      return;
    }
    // e + digit edits the Nth listed collection on the detail page.
    if (node === chordMap.e && /^[1-9]$/.test(e.key)) {
      if (editCollection(parseInt(e.key, 10))) { e.preventDefault(); return; }
    }
    // Unknown key: fall through so it still does its single-key job.
  }

  // ? overlay
  if (e.key === '?') {
    var helpDlg = document.getElementById('shortcuts-help');
    if (helpDlg) {
      e.preventDefault();
      if (helpDlg.open) helpDlg.close();
      else helpDlg.showModal();
    }
    return;
  }

  // Search focus (anywhere a search input exists).
  if (e.key === 's' || e.key === '/') {
    if (focusFirstSelector(['#search-input', '#sidebar-inner input[name="q"]'])) {
      e.preventDefault();
      return;
    }
  }

  // Y → click the topbar Sync button.
  if (e.key === 'Y') {
    var syncForm = document.querySelector('form[hx-post="/internal/sync"]');
    if (syncForm) { e.preventDefault(); syncForm.requestSubmit ? syncForm.requestSubmit() : syncForm.submit(); return; }
  }

  // , / .  rating-ceiling cycle.
  if (e.key === ',') { if (cycleRatingCeiling(-1)) { e.preventDefault(); return; } }
  if (e.key === '.') { if (cycleRatingCeiling(+1)) { e.preventDefault(); return; } }

  // \ switch gallery (only when more than one is configured).
  if (e.key === '\\') {
    var swDlg = document.getElementById('gallery-switch-dialog');
    if (swDlg) { e.preventDefault(); swDlg.showModal(); return; }
  }

  // Leader: g opens a navigation chord window.
  if (e.key === 'g' && !e.ctrlKey && !e.metaKey && !e.altKey) {
    e.preventDefault();
    enterChord(chordMap.g, 'g');
    return;
  }

  // Leader: e opens the detail-page edit chord. Gated on the detail page so
  // non-detail surfaces don't swallow a stray `e`.
  if (e.key === 'e' && !e.ctrlKey && !e.metaKey && !e.altKey && isDetailPage()) {
    e.preventDefault();
    enterChord(chordMap.e, 'e');
    return;
  }

  // Ctrl/Cmd+A → select every visible thumbnail. Gated on the gallery grid
  // existing so the browser's native select-all-text still works on pages
  // without a grid.
  if ((e.ctrlKey || e.metaKey) && (e.key === 'a' || e.key === 'A')) {
    if (!document.querySelector('.thumb-checkbox')) return;
    e.preventDefault();
    selectAll();
    return;
  }

  // Tags page
  if (isTagsPage()) {
    if (e.key === 'n') {
      var btnNew = document.getElementById('btn-create-tag');
      if (btnNew) { e.preventDefault(); btnNew.click(); return; }
    }
    if (e.key === 'N') {
      var btnAlias = document.getElementById('btn-create-alias');
      if (btnAlias) { e.preventDefault(); btnAlias.click(); return; }
    }
    if (handlePaginationKey(e)) { e.preventDefault(); return; }
  }

  // Categories page
  if (isCategoriesPage() && e.key === 'n') {
    if (focusFirstSelector(['.add-cat-form input[name="name"]'])) { e.preventDefault(); return; }
  }

  // Settings page: 1-9 jump to section anchors, in nav order.
  if (isSettingsPage() && /^[1-9]$/.test(e.key)) {
    var settingsAnchors = ['#general', '#galleries', '#monloader', '#tagger', '#relations', '#auth', '#maintenance', '#schedule', '#stats'];
    var sec = document.querySelector(settingsAnchors[parseInt(e.key, 10) - 1]);
    if (sec) { e.preventDefault(); sec.scrollIntoView({ behavior: 'smooth', block: 'start' }); return; }
  }

  // f / i toggles on the detail page (favorite / inbox archive).
  // Selection branch opens the batch-favorite dialog before falling through
  // so a selection-mode `f` doesn't also trip the detail-page handler.
  if (e.key === 'f') {
    if (batchBarVisible()) {
      if (typeof openBatchFavoriteDialog === 'function') {
        e.preventDefault(); openBatchFavoriteDialog('selection'); return;
      }
    }
    var favBtn = document.querySelector('.btn-fav');
    if (favBtn) { e.preventDefault(); favBtn.click(); return; }
  }

  // 'a' → context-dependent add-tag entry point. Detail tag-input focus
  // takes priority over the Actions chooser. Selection branch lives in the
  // same key with priority: selection > detail tag input > gallery chooser.
  if (e.key === 'a' && !e.ctrlKey && !e.metaKey) {
    if (batchBarVisible()) { e.preventDefault(); openTagSelectedDialog(); return; }
    var tagInput = document.getElementById('tag-input');
    if (tagInput) { e.preventDefault(); tagInput.focus(); return; }
    var actionsBtn = document.getElementById('actions-btn');
    if (actionsBtn) { e.preventDefault(); actionsBtn.click(); return; }
  }

  // 'r' → remove tags on selection / enter detail tag-focus mode.
  if (e.key === 'r') {
    if (batchBarVisible()) { e.preventDefault(); openStripSelectedDialog(); return; }
    if (isDetailPage()) { e.preventDefault(); enterTagFocusMode(); return; }
  }

  // Manga reader shortcuts. R opens the reader from the detail page or
  // the pages grid (where there's no tag-focus context to clobber).
  // P opens the all-pages grid from the detail page. Both gate on the
  // matching .btn-manga-action anchor being present so non-manga
  // detail pages don't fire on these uppercase keys.
  if (e.key === 'R') {
    var readBtn = document.querySelector('.btn-manga-action.btn-read');
    if (readBtn) { e.preventDefault(); window.location.href = readBtn.href; return; }
    // On /pages the visible Open-in-reader button was dropped as
    // redundant; the keyboard shortcut still has a job, so fall back
    // to the page's image-id data attribute.
    var pagesGrid = document.getElementById('pages-grid-page');
    if (pagesGrid && pagesGrid.dataset.imageId) {
      e.preventDefault();
      window.location.href = '/images/' + pagesGrid.dataset.imageId + '/read?page=1';
      return;
    }
  }
  if (e.key === 'P' && isDetailPage()) {
    var pagesBtn = document.querySelector('.btn-manga-action.btn-pages');
    if (pagesBtn) { e.preventDefault(); window.location.href = pagesBtn.href; return; }
  }

  // 'v' → open the image viewer. Gated on the still-image branch
  // (the .detail-img-link anchor only renders for non-video / non-manga
  // / not-missing rows), so the key no-ops on every other surface.
  if (e.key === 'v' && isDetailPage()) {
    var lbTrigger = document.querySelector('.detail-img-link');
    if (lbTrigger) { e.preventDefault(); openLightbox(null, lbTrigger.href); return; }
  }

  // 't' → auto-tag on selection / open detail auto-tag dialog.
  if (e.key === 't') {
    if (batchBarVisible()) {
      if (!document.querySelector('.btn-autotag')) return;
      e.preventDefault(); openBatchAutotagDialog('selection'); return;
    }
    if (isDetailPage()) {
      var btn = document.querySelector('.btn-autotag');
      if (btn) { e.preventDefault(); btn.click(); return; }
    }
  }

  // 'm' → move dialog (selection on the gallery; detail page).
  if (e.key === 'm') {
    if (batchBarVisible()) { e.preventDefault(); openBatchMoveDialog('selection'); return; }
    if (isDetailPage()) {
      var moveDlg = document.getElementById('move-image-dialog');
      if (moveDlg && typeof openMoveImageDialog === 'function') {
        e.preventDefault(); openMoveImageDialog(); return;
      }
    }
  }

  // 'i' → toggle inbox / archive (selection bulk dialog or detail toggle).
  if (e.key === 'i') {
    if (batchBarVisible()) {
      if (typeof openBatchInboxDialog === 'function') {
        e.preventDefault(); openBatchInboxDialog('selection'); return;
      }
    }
    if (isDetailPage()) {
      var inboxBtn = document.querySelector('.btn-inbox');
      if (inboxBtn) { e.preventDefault(); inboxBtn.click(); return; }
    }
  }

  // Gallery view-level toggles (Shift modifiers).
  if (isGalleryPage()) {
    if (e.key === 'F') {
      var fbtn = document.getElementById('fav-filter-btn');
      if (fbtn) { e.preventDefault(); fbtn.click(); return; }
    }
    if (e.key === 'R') {
      var rbtn = document.getElementById('random-sort-btn');
      if (rbtn) { e.preventDefault(); rbtn.click(); return; }
    }
    if (e.key === 'O') { if (cycleSort()) { e.preventDefault(); return; } }
    if (e.key === 'D') { if (flipSortDirection()) { e.preventDefault(); return; } }
    if (e.key === 'S' && !batchBarVisible()) {
      if (openSaveSearchDialog()) { e.preventDefault(); return; }
    }
    if (handlePaginationKey(e)) { e.preventDefault(); return; }
    if (e.key === 'Home') { if (jumpGridCursor('first')) { e.preventDefault(); return; } }
    if (e.key === 'End')  { if (jumpGridCursor('last'))  { e.preventDefault(); return; } }
  }

  // 'Delete' → delete current image (detail) or selection (gallery).
  if (e.key === 'Delete' || e.key === 'Del') {
    if (batchBarVisible()) {
      e.preventDefault();
      if (typeof batchDeleteSelected === 'function') batchDeleteSelected();
      return;
    }
    var delBtn = document.getElementById('delete-image-btn');
    if (delBtn) { e.preventDefault(); delBtn.click(); return; }
  }

  // Spacebar → play/pause the detail-page video, or toggle the focused
  // thumbnail's selection on the gallery.
  if (e.key === ' ') {
    var vid = document.querySelector('.detail-video');
    if (vid) {
      e.preventDefault();
      if (vid.paused) vid.play(); else vid.pause();
      return;
    }
    var focusedCard = document.querySelector('.thumb-card.focused');
    if (focusedCard) {
      var cb = focusedCard.querySelector('.thumb-checkbox');
      if (cb) {
        e.preventDefault();
        cb.checked = !cb.checked;
        updateBatchBar();
      }
    }
    return;
  }

  // Backspace: detail-page back. Mirrors Esc's back step.
  if (e.key === 'Backspace' && isDetailPage()) {
    e.preventDefault();
    detailBack();
    return;
  }

  // o → open focused card (gallery) or open original in new tab (detail).
  if (e.key === 'o') {
    if (isDetailPage()) {
      var dlA = document.querySelector('.detail-actions a[download]');
      if (dlA) { e.preventDefault(); window.open(dlA.href, '_blank'); return; }
    } else {
      var foc = document.querySelector('.thumb-card.focused a');
      if (foc) { e.preventDefault(); window.location.href = foc.href; return; }
    }
  }

  // h j k l: vim aliases.
  // Detail page: h/k = prev image, l/j = next image.
  // Gallery: grid-cursor moves (h ← l → k ↑ j ↓).
  if (e.key === 'h' || e.key === 'l' || e.key === 'j' || e.key === 'k') {
    if (document.querySelector('#image-tags .tag-item.focused')) {
      e.preventDefault();
      cycleTagFocus((e.key === 'l' || e.key === 'j') ? 1 : -1);
      return;
    }
    if (isDetailPage()) {
      var dir = (e.key === 'l' || e.key === 'j') ? 'next' : 'prev';
      if (navDetailImage(dir)) { e.preventDefault(); return; }
    } else if (isGalleryPage()) {
      var dx = 0, dy = 0;
      if (e.key === 'h') dx = -1;
      else if (e.key === 'l') dx = 1;
      else if (e.key === 'k') dy = -1;
      else if (e.key === 'j') dy = 1;
      if (moveGridCursor(dx, dy)) { e.preventDefault(); return; }
    }
  }

  // Arrow keys: tag-focus mode > detail prev/next > gallery grid moves.
  if (e.key === 'ArrowLeft' || e.key === 'ArrowRight' || e.key === 'ArrowUp' || e.key === 'ArrowDown') {
    if (document.querySelector('#image-tags .tag-item.focused')) {
      e.preventDefault();
      cycleTagFocus(e.key === 'ArrowRight' || e.key === 'ArrowDown' ? 1 : -1);
      return;
    }
    if (isDetailPage()) {
      if (e.key === 'ArrowLeft') { if (navDetailImage('prev')) { e.preventDefault(); } return; }
      if (e.key === 'ArrowRight') { if (navDetailImage('next')) { e.preventDefault(); } return; }
      return;
    }
    if (isGalleryPage()) {
      var ax = 0, ay = 0;
      if (e.key === 'ArrowLeft') ax = -1;
      else if (e.key === 'ArrowRight') ax = 1;
      else if (e.key === 'ArrowUp') ay = -1;
      else if (e.key === 'ArrowDown') ay = 1;
      if (moveGridCursor(ax, ay)) { e.preventDefault(); return; }
    }
  }

  // Enter: tag-focus mode remove > gallery open focused card.
  if (e.key === 'Enter') {
    var focusedTag = document.querySelector('#image-tags .tag-item.focused');
    if (focusedTag) {
      e.preventDefault();
      // Stash the focused tag's flat-list index so the htmx:afterSettle
      // swap below can land the cursor on the previous sibling instead
      // of dropping back to index 0 (the natural restart point with no
      // .focused element to anchor to).
      var allTags = Array.from(document.querySelectorAll('#image-tags .tag-item'));
      var idx = allTags.indexOf(focusedTag);
      if (idx >= 0) document.body.dataset.tagFocusIdx = String(idx);
      var tagBtn = focusedTag.querySelector('.tag-remove-btn');
      if (tagBtn) tagBtn.click();
      return;
    }
    var focusedCard = document.querySelector('.thumb-card.focused a');
    if (focusedCard) { window.location.href = focusedCard.href; return; }
  }
});

// On a similar-click detail page (marked by data-ref), the "← Previous image"
// link walks browser history instead of navigating to its href so chains of
// any depth unwind one page at a time - matching the Escape keybinding. The
// href stays wired for cold loads (direct URL, bookmarked tab) where history
// has no predecessor.
document.addEventListener('click', function(e) {
  var link = e.target.closest('.back-link');
  if (!link) return;
  if (detailRefBack()) e.preventDefault();
});

// Delete-from-ref walks history instead of server-redirecting so the chain
// (A -> B -> C -> delete) lands back on B's original URL (with its own
// ref=A intact), leaving Escape free to unwind the rest of the chain.
// HX-Redirect would push a fresh history entry and silently drop the ref
// chain. The event's detail.fallback carries the redirect URL the handler
// would otherwise have set; we fall back to it when history has no
// predecessor (direct link, fresh tab).
document.body.addEventListener('delete-go-back', function(e) {
  if (history.length > 1) { history.back(); return; }
  var fallback = e.detail && e.detail.fallback;
  if (fallback) window.location.href = fallback;
});

// Per-tagger config dialogs (thresholds, galleries) and the per-token
// privileges dialog close themselves on a successful save. The server fires
// `tagger-saved` / `token-saved` via HX-Trigger and names the dialog id to
// close; the parent flash and the row updates arrive as OOB swaps so the
// page state is already updated by the time this listener runs.
function closeDialogFromSaveEvent(e) {
  var id = e.detail && e.detail.dialog;
  if (!id) return;
  var dlg = document.getElementById(id);
  if (dlg && dlg.open) dlg.close();
}
document.body.addEventListener('tagger-saved', closeDialogFromSaveEvent);
document.body.addEventListener('token-saved', closeDialogFromSaveEvent);

// Per-tagger Galleries dialog helpers. taggerGalAllToggle disables and
// force-checks the per-gallery boxes when "All galleries" is on so the
// submitted state matches the rendered state. taggerGalSelect mass-sets
// the per-gallery boxes (used by "Select all" / "Select none").
function taggerGalAllToggle(cb) {
  var form = cb.closest('form');
  if (!form) return;
  form.querySelectorAll('input[name=gallery_names]').forEach(function(c) {
    c.disabled = cb.checked;
    if (cb.checked) c.checked = true;
  });
}

function taggerGalSelect(btn, on) {
  var form = btn.closest('form');
  if (!form) return;
  var allCb = form.querySelector('input[name=all]');
  if (allCb && allCb.checked) {
    allCb.checked = false;
    taggerGalAllToggle(allCb);
  }
  form.querySelectorAll('input[name=gallery_names]').forEach(function(c) {
    c.checked = !!on;
  });
}

// Per-tagger threshold dialog: the per-row Reset link only does something
// when the row differs from its catalog default, so hide it otherwise -
// a row already at its default would otherwise show a no-op control. "At
// default" means every number input equals its data-default (empty when
// the catalog seeds none) and the checkbox matches its data-default-checked
// state.
function threshRowAtDefault(row) {
  var atDefault = true;
  row.querySelectorAll('input[type=number]').forEach(function(inp) {
    if (inp.value !== (inp.dataset.default || '')) atDefault = false;
  });
  var cb = row.querySelector('input[type=checkbox]');
  if (cb && cb.checked !== (cb.dataset.defaultChecked === '1')) atDefault = false;
  return atDefault;
}

function syncThreshRow(row) {
  if (!row) return;
  var reset = row.querySelector('.reset-thresh');
  // Show the Reset only when the row differs from its catalog default
  // (any number off its default, or the category disabled). Toggle a
  // visibility class rather than the hidden attribute so the cell keeps
  // reserving the link's width and the column doesn't reflow.
  if (reset) reset.classList.toggle('reset-thresh-shown', !threshRowAtDefault(row));
}

// Reset one row to its catalog defaults, then re-sync so the link hides
// itself now that the row sits back at its default.
function resetThreshRow(link) {
  var row = link.closest('tr');
  if (!row) return;
  row.querySelectorAll('input[type=number]').forEach(function(inp) {
    inp.value = inp.dataset.default || '';
  });
  var cb = row.querySelector('input[type=checkbox]');
  if (cb) cb.checked = cb.dataset.defaultChecked === '1';
  syncThreshRow(row);
}

// Re-sync a row as the operator edits it (one body bound to both input and
// change so number typing and checkbox toggles are covered), and run an
// initial pass when the dialog body lazy-loads (or re-renders after "Reset
// to defaults"): htmx swaps the table into #tagger-thresh-<name>-body.
function syncThreshRowFromEvent(e) {
  var row = e.target.closest && e.target.closest('.tagger-thresh-table tbody tr');
  if (row) syncThreshRow(row);
}
document.body.addEventListener('input', syncThreshRowFromEvent);
document.body.addEventListener('change', syncThreshRowFromEvent);
document.body.addEventListener('htmx:afterSwap', function(e) {
  var t = e.detail && e.detail.target;
  if (!t) return;
  var table = t.querySelector && t.querySelector('.tagger-thresh-table');
  if (table) table.querySelectorAll('tbody tr').forEach(syncThreshRow);
});

// Returning from a detail page via a back-link with #img-N restores the
// arrow-key cursor on the matching thumbnail.
function restoreGalleryFocusFromHash() {
  var m = window.location.hash.match(/^#img-(\d+)$/);
  if (!m) return;
  var card = document.querySelector('.thumb-card[data-id="' + m[1] + '"]');
  if (!card) return;
  var cards = Array.from(document.querySelectorAll('.thumb-card'));
  setFocused(cards, cards.indexOf(card));
}
document.addEventListener('DOMContentLoaded', restoreGalleryFocusFromHash);

// Video hover preview swap (with error fallback to avoid "?" on hover fail)
document.addEventListener('mouseover', function(e) {
  const card = e.target.closest('.thumb-card');
  if (!card) return;
  const img = card.querySelector('.thumb-img');
  const hoverSrc = card.dataset.hover;
  if (!img || !hoverSrc || img.dataset.hovering) return;
  img.dataset.orig = img.src;
  img.dataset.hovering = '1';
  img.onerror = function() {
    img.src = img.dataset.orig || '';
    delete img.dataset.orig;
    delete img.dataset.hovering;
    img.onerror = null;
  };
  img.src = hoverSrc;
});

document.addEventListener('mouseout', function(e) {
  const card = e.target.closest('.thumb-card');
  if (!card) return;
  const img = card.querySelector('.thumb-img');
  if (!img || !img.dataset.orig) return;
  img.src = img.dataset.orig;
  delete img.dataset.orig;
  delete img.dataset.hovering;
  img.onerror = null;
});

// Tag-input (detail page) clears the invalid-tag flash as soon as the user
// starts fixing the input, so the error isn't stuck on-screen through the
// next submit.
document.addEventListener('input', function(e) {
  if (e.target.id !== 'tag-input') return;
  var tagsDiv = document.getElementById('image-tags');
  if (!tagsDiv) return;
  var err = tagsDiv.querySelector('.flash-err');
  if (err) err.remove();
});

// Sidebar tag-add-btn: toggle tag in/out of the current search query.
// The sidebar emits the term as "category:name"; the query may carry
// either that form or the bare name, so we drop both on remove.
document.addEventListener('click', function(e) {
  const btn = e.target.closest('.tag-add-btn');
  if (!btn) return;
  e.preventDefault();
  const tagName = btn.dataset.tag;
  if (!tagName) return;
  const si = document.getElementById('search-input');
  if (!si) return;
  const colon = tagName.indexOf(':');
  const bare = colon >= 0 ? tagName.slice(colon + 1) : tagName;
  const terms = si.value.trim().split(/\s+/).filter(Boolean);
  const filtered = terms.filter(t => t !== tagName && t !== bare);
  if (filtered.length === terms.length) filtered.push(tagName);
  si.value = filtered.join(' ');
  const form = document.getElementById('search-form');
  if (form && window.htmx) window.htmx.trigger(form, 'submit');
  else if (form) form.submit();
});

// Category collapse in sidebar. Listening on the whole header (not just
// the ▾) keeps the click target away from the sidebar scrollbar at the
// right edge.
document.addEventListener('click', function(e) {
  const header = e.target.closest('.tag-group-header');
  if (!header) return;
  const group = header.closest('.tag-group');
  if (!group) return;
  const list = group.querySelector('.tag-list-sidebar');
  if (!list) return;
  const indicator = group.querySelector('.cat-collapse');
  const collapsed = list.style.display === 'none';
  list.style.display = collapsed ? '' : 'none';
  if (indicator) indicator.textContent = collapsed ? '▾' : '▸';
});

// Batch selection: show/hide batch bar and keep checkboxes visible. The
// blur drops focus off the checkbox after the toggle so the keydown
// router's input-focused guard (`if (isInput) return`) doesn't swallow
// the selection shortcuts (a, f, r, t, m, i, Delete) on the next press.
document.addEventListener('change', function(e) {
  if (!e.target.classList.contains('thumb-checkbox')) return;
  updateBatchBar();
  e.target.blur();
});

// forEachClusterCheckbox walks forward through the grid's children after a
// cluster header, visiting every thumbnail checkbox until the next cluster
// header (or the end). Shared by the [Select]/[Unselect] click below and
// updateClusterButtons' visibility sync.
function forEachClusterCheckbox(header, fn) {
  var node = header.nextElementSibling;
  while (node && !node.classList.contains('thumb-cluster-header')) {
    var cb = node.querySelector ? node.querySelector('.thumb-checkbox') : null;
    if (cb) fn(cb);
    node = node.nextElementSibling;
  }
}

// Inbox cluster [Select] / [Unselect] buttons: tick or untick every
// thumbnail checkbox in the cluster; the only difference between the two
// buttons is the target checked state.
document.addEventListener('click', function(e) {
  var sel = e.target.closest('[data-cluster-select]');
  var uns = e.target.closest('[data-cluster-unselect]');
  if (!sel && !uns) return;
  e.preventDefault();
  var header = (sel || uns).closest('.thumb-cluster-header');
  if (!header) return;
  var target = !!sel;
  forEachClusterCheckbox(header, function(cb) { cb.checked = target; });
  updateBatchBar();
});

// While the batch bar is up, a plain left-click on a thumbnail toggles
// its checkbox instead of opening the detail page. Modifier-clicks
// (middle, ctrl/cmd, shift) keep the link's default so "open in tab"
// still works. Esc / Cancel clears the selection and the link goes back
// to navigating.
document.addEventListener('click', function(e) {
  if (e.button !== 0 || e.ctrlKey || e.metaKey || e.shiftKey || e.altKey) return;
  var grid = document.getElementById('gallery-grid');
  if (!grid || !grid.classList.contains('batch-active')) return;
  var link = e.target.closest('.thumb-link');
  if (!link) return;
  var card = link.closest('.thumb-card');
  if (!card) return;
  var cb = card.querySelector('.thumb-checkbox');
  if (!cb) return;
  e.preventDefault();
  cb.checked = !cb.checked;
  updateBatchBar();
});

// In the inbox, hide a cluster's [Unselect] when nothing in it is selected
// and [Select] when all of it is - the same sibling-walk as the click above.
function updateClusterButtons() {
  document.querySelectorAll('.thumb-cluster-header[data-cluster-start]').forEach(function(header) {
    var total = 0, checked = 0;
    forEachClusterCheckbox(header, function(cb) {
      total++;
      if (cb.checked) checked++;
    });
    var sel = header.querySelector('[data-cluster-select]');
    var uns = header.querySelector('[data-cluster-unselect]');
    if (sel) sel.hidden = total > 0 && checked === total;
    if (uns) uns.hidden = checked === 0;
  });
}

function updateBatchBar() {
  const checked = document.querySelectorAll('.thumb-checkbox:checked');
  const bar = document.getElementById('batch-bar');
  const grid = document.getElementById('gallery-grid');
  if (bar) bar.classList.toggle('visible', checked.length > 0);
  if (grid) grid.classList.toggle('batch-active', checked.length > 0);
  const countEl = document.getElementById('batch-count');
  if (countEl) countEl.textContent = checked.length + ' selected';
  // Hide the header's Actions chooser while a selection is active so users
  // aiming for the batch-bar's selection-scoped buttons can't misclick onto
  // the search-scoped variants.
  const actions = document.getElementById('actions-btn');
  if (actions) actions.hidden = checked.length > 0;
  updateClusterButtons();
}

function clearSelection() {
  document.querySelectorAll('.thumb-checkbox:checked').forEach(function(cb) { cb.checked = false; });
  updateBatchBar();
}

function selectAll() {
  document.querySelectorAll('.thumb-checkbox').forEach(function(cb) { cb.checked = true; });
  updateBatchBar();
}

// Detail-page tag-focus mode (entered with 'r'). The currently focused tag
// is marked by .tag-item.focused; arrow keys cycle, Enter triggers the
// matching .tag-remove-btn, Escape exits without removing.
function enterTagFocusMode() {
  var items = document.querySelectorAll('#image-tags .tag-item');
  if (!items.length) return;
  if (document.querySelector('#image-tags .tag-item.focused')) return;
  items[0].classList.add('focused');
  items[0].scrollIntoView({block: 'nearest'});
  // Mode flag survives an htmx swap that strips the .focused class on the
  // <li> - the Escape handler keys off body.tag-focus to know we're still
  // in mode.
  document.body.classList.add('tag-focus');
}

function exitTagFocusMode() {
  document.querySelectorAll('#image-tags .tag-item.focused').forEach(function(li) {
    li.classList.remove('focused');
  });
  document.body.classList.remove('tag-focus');
}

function cycleTagFocus(step) {
  var items = Array.from(document.querySelectorAll('#image-tags .tag-item'));
  if (!items.length) return;
  var current = document.querySelector('#image-tags .tag-item.focused');
  var idx = current ? items.indexOf(current) : 0;
  idx = Math.max(0, Math.min(items.length - 1, idx + step));
  setFocused(items, idx);
}

// Batch delete: opens the unified delete dialog scoped to the checked
// thumbs. The actual fetch + background-job wiring lives in
// confirmBatchDelete (gallery.html).
function batchDeleteSelected() {
  if (typeof openBatchDeleteDialog === 'function') openBatchDeleteDialog('selection');
}

// openSaveSearchDialog prefills the save-search dialog from the current
// search input, then opens it. Shared by the S-key shortcut and the
// Actions chooser's Save entry. Returns false when the dialog isn't on
// the page so the key handler can fall through.
function openSaveSearchDialog() {
  var dlg = document.getElementById('save-search-dialog');
  if (!dlg) return false;
  var si = document.getElementById('search-input');
  var sq = document.getElementById('save-search-query');
  var sp = document.getElementById('save-search-preview');
  if (si && sq) sq.value = si.value;
  if (si && sp) sp.textContent = si.value || '(empty)';
  // Snapshot the current URL's sort/order/seed so the saved entry
  // reopens at the same view. Defaults stay empty so the gallery
  // handler's defaults take over on reopen if nothing was set here.
  var url = new URL(window.location.href);
  var ss = document.getElementById('save-search-sort');
  var so = document.getElementById('save-search-order');
  var se = document.getElementById('save-search-seed');
  if (ss) ss.value = url.searchParams.get('sort') || '';
  if (so) so.value = url.searchParams.get('order') || '';
  if (se) se.value = url.searchParams.get('seed') || '';
  dlg.showModal();
  return true;
}

// refreshJobStatus forces the top-right job-status widget to re-fetch its
// state so a newly started background job becomes visible without waiting for
// the next 2s poll tick.
function refreshJobStatus() {
  var el = document.getElementById('job-status');
  if (!el || !window.htmx) return;
  el.setAttribute('hx-trigger', 'every 2s');
  window.htmx.process(el);
  window.htmx.ajax('GET', '/internal/job/status', {target: '#job-status', swap: 'outerHTML'});
}

// Close a dialog and, if it was opened from another dialog (data-return-to
// set by the opener), re-open the parent. Used by sub-dialog Cancel buttons
// reachable from the Actions chooser so cancel/escape pops one level
// instead of collapsing the whole stack.
function closeDialogAndRestoreParent(dialogId) {
  var d = document.getElementById(dialogId);
  if (!d) return;
  var parentId = d.dataset.returnTo;
  d.close();
  if (parentId) {
    var parent = document.getElementById(parentId);
    if (parent && typeof parent.showModal === 'function') parent.showModal();
  }
}

// Escape on a modal <dialog> raises the `cancel` event via the browser's
// close watcher, regardless of whether an input inside the dialog is
// focused. We listen in the capture phase because the cancel event
// doesn't bubble. When the dialog has data-return-to set (chooser opened
// it), preventDefault keeps the dialog open just long enough for
// closeDialogAndRestoreParent to swap the chooser back in. Without this
// hook, the keydown branch alone misses the input-focused case (the
// user's first Escape only blurs the input, then the browser's default
// close fires on the next tick before any JS can re-open the parent).
document.addEventListener('cancel', function(e) {
  var d = e.target;
  if (!d || !d.tagName || d.tagName !== 'DIALOG') return;
  if (!d.dataset.returnTo) return;
  e.preventDefault();
  closeDialogAndRestoreParent(d.id);
}, true);

// Shared confirmation dialog: replaces native confirm() and intercepts
// hx-confirm via the htmx:confirm event listener below. The triggering
// element may set data-confirm-danger for a red-tinted second warning line.
function showConfirm(message, onOk, danger, okLabel) {
  var dlg = document.getElementById('confirm-dialog');
  if (!dlg) { if (window.confirm(message)) onOk(); return; }
  document.getElementById('confirm-dialog-msg').textContent = message || '';
  document.getElementById('confirm-dialog-danger').textContent = danger || '';
  var okBtn = document.getElementById('confirm-dialog-ok');
  var cancelBtn = document.getElementById('confirm-dialog-cancel');
  okBtn.textContent = okLabel || 'OK';
  var close = function() { dlg.close(); okBtn.onclick = null; cancelBtn.onclick = null; };
  okBtn.onclick = function() { close(); onOk(); };
  cancelBtn.onclick = close;
  dlg.showModal();
  // Land focus on the safer button so a keyboard-only operator can
  // commit safe prompts with a single Enter while destructive prompts
  // (those carrying a danger line) require an explicit Tab to OK.
  if (danger) cancelBtn.focus(); else okBtn.focus();
}

document.body.addEventListener('htmx:confirm', function(e) {
  if (!e.detail || !e.detail.question) return;
  e.preventDefault();
  var ds = e.detail.elt && e.detail.elt.dataset ? e.detail.elt.dataset : {};
  showConfirm(e.detail.question, function() { e.detail.issueRequest(true); }, ds.confirmDanger, ds.confirmOk);
});

// Page jump: dialog that sets ?page= on the current URL. Works for both
// the HTMX gallery and the full-page tags pagination.
document.addEventListener('click', function(e) {
  var btn = e.target.closest('.page-jump');
  if (!btn) return;
  e.preventDefault();
  var dlg = document.getElementById('page-jump-dialog');
  var input = document.getElementById('page-jump-input');
  var totalSpan = document.getElementById('page-jump-total');
  if (!dlg || !input) return;
  var current = btn.dataset.current || '1';
  var total = btn.dataset.total || '1';
  input.value = current;
  // data-max, not the HTML max attribute: max would trigger HTML5
  // constraint validation and block submit, so the clamp never runs.
  input.dataset.max = total;
  if (totalSpan) totalSpan.textContent = total;
  dlg.showModal();
  setTimeout(function() { input.focus(); input.select(); }, 0);
});

// "Add a relation..." chip in the detail-page Relations panel opens
// the manual relation-add dialog with the current image pre-filled.
// Each open resets the form so a chain of adds doesn't keep the prior
// child id or relation-type radio in place after the dialog closes.
document.addEventListener('click', function(e) {
  var btn = e.target.closest('[data-relations-add]');
  if (!btn) return;
  e.preventDefault();
  var dlg = document.getElementById('relation-edit-dialog');
  if (!dlg) return;
  var parentInput = document.getElementById('relation-parent-id');
  var otherInput = document.getElementById('relation-other-id');
  var self = btn.getAttribute('data-relations-add');
  if (parentInput) { parentInput.value = self; parentInput.readOnly = true; }
  if (otherInput) { otherInput.value = ''; otherInput.readOnly = false; }
  var otherThumb = document.getElementById('relation-edit-thumb-other');
  if (otherThumb) { otherThumb.hidden = true; otherThumb.removeAttribute('src'); otherThumb.alt = ''; }
  var dupRadio = dlg.querySelector('input[name="type"][value="duplicate"]');
  if (dupRadio) dupRadio.checked = true;
  var err = document.getElementById('relation-edit-error');
  if (err) err.innerHTML = '';
  var overwrite = document.getElementById('relation-edit-overwrite-btn');
  if (overwrite) {
    overwrite.hidden = true;
    overwrite.textContent = 'Overwrite existing relation';
  }
  dlg.showModal();
});

// Handles the response of the detail-page external-field dialogs
// (Source / URL / Collection / Order). On 204 the server sends back
// HX-Refresh so the page reloads with the new value; the post-reload
// flash is delivered via the shared monbooru:flash HX-Trigger / picker.
// Validation errors land 200 OK with a flash-err body the dialog already
// targets, so the dialog stays open and the operator can correct the
// input.
//
// HTMX events bubble: the input inside the form fires hx-get against
// the collection / source suggest endpoints, and those return 204 when
// no matches exist. Without the FORM gate below, every empty suggest
// response would slip past the status check and close the dialog
// mid-typing.
function onExternalEditResponse(event, dialogID) {
  if (!event || !event.detail || !event.detail.xhr) return;
  if (!event.detail.elt || event.detail.elt.tagName !== 'FORM') return;
  if (event.detail.xhr.status !== 204) return;
  var dlg = document.getElementById(dialogID);
  if (dlg) dlg.close();
}

// toggleAnnotations shows/hides the note-box overlay on the detail image.
function toggleAnnotations(btn) {
  var media = btn.closest('.detail-media');
  if (!media) return;
  var hidden = media.classList.toggle('notes-hidden');
  btn.textContent = hidden ? '[show notes]' : '[hide notes]';
}

// actionFlashSlots is the ordered list of per-page slot ids the shared
// flash helpers fall through. Each page that wants action feedback ships
// one of these slots in its template; the page only ever has one of them.
var actionFlashSlots = ['gallery-flash', 'detail-flash', 'tag-flash', 'cat-flash', 'collection-flash', 'flash-tagger'];

function findActionFlashSlot() {
  for (var i = 0; i < actionFlashSlots.length; i++) {
    var el = document.getElementById(actionFlashSlots[i]);
    if (el) return el;
  }
  return null;
}

// showActionFlash writes html into the first available flash slot and
// auto-clears after 5 s. Stale content is replaced on every call so
// back-to-back actions don't pile up. kind picks the flash-ok / flash-err
// palette; html is inserted as-is when wrapped in a .flash element,
// otherwise wrapped.
function showActionFlash(html, kind) {
  var slot = findActionFlashSlot();
  if (!slot || !html) return;
  var cls = kind === 'err' ? 'flash-err' : 'flash-ok';
  if (/class\s*=\s*"[^"]*flash\b/.test(html)) {
    slot.innerHTML = html;
  } else {
    slot.innerHTML = '<div class="flash ' + cls + '">' + html + '</div>';
  }
  var token = String(Date.now()) + Math.random();
  slot.dataset.token = token;
  setTimeout(function() {
    if (slot.dataset.token === token) slot.innerHTML = '';
  }, 5000);
}

// stashActionFlash queues a flash for the next page load - used when an
// action triggers a full reload / redirect and the in-place showActionFlash
// would be wiped before the user could read it. The timestamp lets the
// picker reject stashes older than the staleness window so a non-navigating
// action's stash doesn't poison a later, unrelated navigation.
function stashActionFlash(html, kind) {
  if (!html) return;
  try {
    sessionStorage.setItem('monbooru_action_flash',
      JSON.stringify({h: html, k: kind || 'ok', t: Date.now()}));
  } catch (e) {}
}

var showGalleryFlash = showActionFlash;
var stashGalleryFlash = stashActionFlash;

// stashStalenessMs caps how long a stashed flash survives before the picker
// drops it. Long enough to cover a normal action+navigation round-trip,
// short enough that a stash left behind by a non-navigating action doesn't
// pop on the next page the user visits five minutes later.
var stashStalenessMs = 10000;

document.addEventListener('DOMContentLoaded', function() {
  if (!findActionFlashSlot()) return;
  var raw;
  try { raw = sessionStorage.getItem('monbooru_action_flash'); } catch (e) { return; }
  if (!raw) return;
  try { sessionStorage.removeItem('monbooru_action_flash'); } catch (e) {}
  var stash;
  try { stash = JSON.parse(raw); } catch (e) { return; }
  if (!stash || !stash.h) return;
  if (stash.t && Date.now() - stash.t > stashStalenessMs) return;
  showActionFlash(stash.h, stash.k);
});

// HX-Trigger bridge: every handler that wants a success flash emits
// `{"monbooru:flash": {"text": "...", "kind": "ok"}}` on the response.
// Dual-mode: show on the current page (covers non-navigating actions like
// relation-add) and stash for the next page (covers HX-Redirect / HX-Refresh
// flows like delete / move / categories / tags / external-edit). The
// stash carries a timestamp so a non-consumed stash doesn't outlive the
// staleness window.
document.body.addEventListener('monbooru:flash', function(e) {
  if (!e || !e.detail) return;
  var text = e.detail.text || '';
  var kind = e.detail.kind || 'ok';
  showActionFlash(text, kind);
  stashActionFlash(text, kind);
});

// Handles the dialog response for /relations/add. The success-side flash
// rides the shared monbooru:flash HX-Trigger so it lands in the page
// slot via the common listener; this handler only owns the conflict
// affordance (Overwrite button label / visibility) and the dialog-close
// + related-entries refresh on success.
function onRelationEditResponse(event) {
  var src = document.getElementById('relation-edit-error');
  if (!src) return;
  var overwrite = document.getElementById('relation-edit-overwrite-btn');
  var success = src.querySelector('.flash-ok');
  if (!success) {
    // Conflict: surface the Overwrite affordance so the operator can
    // replace the existing relation in one click. The button label
    // tracks which conflict the server reported so the operator reads
    // exactly what they're about to drop:
    //   - "already has a different relation" -> Overwrite existing relation
    //     (pair-shaped conflict between THIS pair)
    //   - "already has a version edge" -> Replace existing version edge
    //     (one side is already on a version chain with a third image)
    //   - "already has a source" -> Replace existing source
    //     (the chosen derivative already points at a different source)
    var err = src.querySelector('.flash-err');
    if (overwrite) {
      var msg = err ? (err.textContent || '') : '';
      if (/already has a different relation/i.test(msg)) {
        overwrite.textContent = 'Overwrite existing relation';
        overwrite.hidden = false;
      } else if (/already has a version edge/i.test(msg)) {
        overwrite.textContent = 'Replace existing version edge';
        overwrite.hidden = false;
      } else if (/already has a source/i.test(msg)) {
        overwrite.textContent = 'Replace existing source';
        overwrite.hidden = false;
      } else {
        overwrite.hidden = true;
      }
    }
    return;
  }
  if (overwrite) {
    overwrite.hidden = true;
    overwrite.textContent = 'Overwrite existing relation';
  }
  src.innerHTML = '';
  var dlg = document.getElementById('relation-edit-dialog');
  if (dlg) dlg.close();
  if (window.htmx) {
    var panel = document.getElementById('related-entries-panel');
    if (panel) window.htmx.trigger(panel, 'relations-changed');
  }
}

function submitPageJump() {
  var input = document.getElementById('page-jump-input');
  if (!input) return;
  var p = parseInt(input.value, 10);
  if (!p || p < 1) p = 1;
  var max = parseInt(input.dataset.max, 10);
  if (max && p > max) p = max;
  var u = new URL(window.location.href);
  u.searchParams.set('page', String(p));
  window.location.href = u.toString();
}

// Sidebar toggle (narrow viewports)
document.addEventListener('click', function(e) {
  if (!e.target.id || e.target.id !== 'sidebar-toggle') return;
  const sidebar = document.getElementById('sidebar');
  if (sidebar) sidebar.classList.toggle('open');
});

// Folder tree: expand/collapse with cookie persistence
function getFolderCookie() {
  var m = document.cookie.match(/monbooru_folders=([^;]*)/);
  if (!m) return new Set();
  try { return new Set(decodeURIComponent(m[1]).split(',').filter(Boolean)); }
  catch (err) { return new Set(); }
}

function setFolderCookie(set) {
  document.cookie = 'monbooru_folders=' + encodeURIComponent(Array.from(set).join(',')) + '; path=/; max-age=31536000';
}

function toggleFolderItem(btn, targetId, path) {
  var list = document.getElementById(targetId);
  if (!list) return;
  var state = getFolderCookie();
  var isCollapsed = list.style.display === 'none';
  list.style.display = isCollapsed ? '' : 'none';
  btn.textContent = isCollapsed ? '▼' : '▶';
  if (isCollapsed) state.add(path || targetId);
  else state.delete(path || targetId);
  setFolderCookie(state);
}

// The cookie records sections toggled off their default: an opened
// default-collapsed section, or a collapsed default-open one (Tags).
function initSectionToggle(toggleId, listId, cookieKey, forceOpen, defaultOpen) {
  var toggle = document.getElementById(toggleId);
  var list = document.getElementById(listId);
  if (!toggle || !list) return;
  var offDefault = getFolderCookie().has(cookieKey);
  if (defaultOpen) {
    if (offDefault) { list.style.display = 'none'; toggle.textContent = '▶'; }
  } else if (forceOpen || offDefault) {
    list.style.display = '';
    toggle.textContent = '▼';
  }
  var clickToggle = function() {
    var state = getFolderCookie();
    var isCollapsed = list.style.display === 'none';
    list.style.display = isCollapsed ? '' : 'none';
    toggle.textContent = isCollapsed ? '▼' : '▶';
    var nowOffDefault = defaultOpen ? !isCollapsed : isCollapsed;
    if (nowOffDefault) state.add(cookieKey);
    else state.delete(cookieKey);
    setFolderCookie(state);
  };
  toggle.onclick = clickToggle;
  var header = toggle.closest('.sidebar-section-header');
  var title = header && header.querySelector('.sidebar-section-title');
  if (title) title.onclick = clickToggle;
}

// _lastInitedQuery latches the URL the sidebar's auto-expand logic
// last responded to. HTMX OOB swaps replace #sidebar-inner verbatim on
// every gallery refresh - including the watcher-driven refreshes the
// job-status poll fires without a URL change - so any `firstInit` marker
// stored on a DOM element gets wiped between renders. Comparing the
// captured query against window.location.search is the only signal
// that distinguishes a real navigation from a same-view re-render, and
// without it the URL-driven force-open undoes the user's explicit
// section collapse a few seconds after they click it.
var _lastInitedQuery = null;

function initFolderTree() {
  // The sidebar is lazy-loaded on every page that owns one; DOMContentLoaded
  // fires before the placeholder's hx-get settles. Bail when no toggle
  // buttons are present so the empty pass doesn't latch the URL and
  // suppress the next real settle's navigation-driven force-open.
  if (!document.querySelector('.folder-toggle-btn')) return;

  var expanded = getFolderCookie();
  var currentQuery = window.location.search;
  var urlChanged = currentQuery !== _lastInitedQuery;
  _lastInitedQuery = currentQuery;

  // Determine current folder from URL query. Both folder:PATH (recursive)
  // and folderonly:PATH (exact) drive the same sidebar auto-expand path.
  var currentFolder = '';
  var urlParams = new URLSearchParams(currentQuery);
  var q = urlParams.get('q') || '';
  var folderMatch = q.match(/(?:^|\s)folder(?:only)?:(?:"([^"]+)"|([^\s]*))/);
  if (folderMatch) {
    currentFolder = folderMatch[1] || folderMatch[2];
  }

  // Tags section: open by default, collapse persists (inverse of the others).
  initSectionToggle('tags-toggle', 'tag-groups', '__tags__', false, true);
  // Folder tree main toggle (show/hide whole tree).
  // Use onclick assignment (not addEventListener) to prevent duplicate handlers
  // from multiple calls (e.g. HTMX partial swaps fire htmx:afterSettle repeatedly).
  initSectionToggle('folder-tree-toggle', 'folder-tree-list', '__tree__', false);
  // Sources panel: auto-open when the current query targets a specific
  // source label, so the matching row is visible on landing.
  var sourceLabelMatch = q.match(/(?:^|\s)source:(?:"([^"]+)"|([^\s]+))/);
  var sourceLabelOpen = urlChanged && !!sourceLabelMatch;
  initSectionToggle('source-labels-toggle', 'source-labels-list', '__sources__', sourceLabelOpen);
  var treeToggle = document.getElementById('folder-tree-toggle');
  var treeList = document.getElementById('folder-tree-list');

  // Subfolder toggles - use onclick to avoid duplicate listeners
  document.querySelectorAll('.folder-toggle-btn[data-path]').forEach(function(btn) {
    var path = btn.dataset.path;
    var targetId = btn.dataset.target || ('fc-' + path);
    var list = document.getElementById(targetId);
    if (!list) return;

    // urlDriven: this expansion comes from navigation context (the current
    // folder). Gated by urlChanged so a same-view sidebar rebuild (watcher
    // refresh, job progress) leaves the user's collapse alone - only a real
    // URL change re-asserts the navigation-driven open.
    var urlDriven = currentFolder && (currentFolder === path || currentFolder.startsWith(path + '/'));
    var shouldExpand = expanded.has(path) || (urlChanged && urlDriven);

    if (shouldExpand) {
      list.style.display = '';
      btn.textContent = '▼';
      // Force the parent tree open only when navigation drives this
      // expansion. The parent's open/close otherwise stays under the
      // user's control via initSectionToggle's own cookie key.
      if (urlChanged && urlDriven && treeList) {
        treeList.style.display = '';
        if (treeToggle) treeToggle.textContent = '▼';
      }
    }

    btn.onclick = function(e) {
      e.stopPropagation();
      toggleFolderItem(btn, targetId, path);
    };
  });
}

document.addEventListener('DOMContentLoaded', initFolderTree);
// Re-run on HTMX settle to restore auto-expand state after URL changes,
// but since we use btn.onclick (not addEventListener) no duplicate handlers accumulate.
document.addEventListener('htmx:afterSettle', initFolderTree);

// Tag suggest (detail page + merge dialog): apply selected suggestion
function applyTagSuggest(btn) {
  var tagName = btn.dataset.tagName;
  if (!tagName) return;
  var dd = btn.closest('.suggest-dropdown');
  if (!dd) return;
  dd.innerHTML = '';
  var container = dd.parentElement;
  if (!container) return;
  var input = container.querySelector('input[type="text"]');
  if (!input) return;
  // Multi-tag inputs (e.g. upload form) keep previous tokens: replace the last
  // whitespace-separated word and append a trailing space for the next tag.
  if (input.dataset.multiTags) {
    var words = input.value.split(/(\s+)/);
    var lastIdx = -1;
    for (var i = words.length - 1; i >= 0; i--) {
      if (words[i].trim() !== '') { lastIdx = i; break; }
    }
    if (lastIdx >= 0) words[lastIdx] = tagName;
    else words.push(tagName);
    input.value = words.join('') + ' ';
    input.focus();
    return;
  }
  input.value = tagName;
  input.focus();
  // Submit the tag-add form if present; merge dialog just fills the input
  var form = input.closest('form') || container.querySelector('form');
  if (form && form.id === 'add-tag-form') {
    form.requestSubmit();
  }
}

// Label suggest (move-image/move-selected folder dialogs; detail and batch
// collection/source dialogs): copies the picked value into the dropdown's
// nearest text input and keeps focus so the user can keep typing. key names
// the dataset field carrying the value ('folderPath' or 'series').
function applyLabelSuggest(btn, key) {
  var label = btn.dataset[key];
  if (label == null) return;
  var dd = btn.closest('.suggest-dropdown');
  if (!dd) return;
  var container = dd.parentElement;
  if (!container) return;
  var input = container.querySelector('input[type="text"]');
  if (!input) return;
  dd.innerHTML = '';
  input.value = label;
  input.focus();
}

// Search suggest: apply selected suggestion to search input
function applySearchSuggest(tagName) {
  var si = document.getElementById('search-input');
  if (!si) return;
  var words = si.value.split(/(\s+)/);
  // Find last non-whitespace word
  var lastWordIdx = -1;
  for (var i = words.length - 1; i >= 0; i--) {
    if (words[i].trim() !== '') { lastWordIdx = i; break; }
  }
  if (lastWordIdx >= 0) {
    var last = words[lastWordIdx];
    var prefix = last.startsWith('-') ? '-' : '';
    words[lastWordIdx] = prefix + tagName;
  } else {
    words.push(tagName);
  }
  // system: cheat-sheet rows end on a colon (`date:`) or a comparison
  // operator (`date:>`, `width:>=`, `date:..`); the user's next keystroke
  // is the value, so don't push the cursor onto a new term with a space.
  var keepCursor = /[:<>=]$|\.\.$/.test(tagName);
  si.value = words.join('') + (keepCursor ? '' : ' ');
  var dd = document.getElementById('search-suggest');
  if (dd) dd.innerHTML = '';
  si.focus();
  // After picking a colon- or operator-terminated row the cursor stays
  // parked next to the prefix (`fav:`, `date:>`, `character:`); fire an
  // input event so htmx's `input changed` trigger refreshes the
  // dropdown with the level-2 hint - operators or value lists for
  // filter keywords, live tags for category prefixes - without forcing
  // the user to type a throwaway character to wake autocomplete back up.
  if (keepCursor) {
    si.dispatchEvent(new Event('input', { bubbles: true }));
  }
}

// Copy `text` to the clipboard with a clipboard-API path and a
// document.execCommand fallback so the helper still works under the
// LAN-HTTP deployments where the secure-context API is unavailable.
// `flashEl`, when provided, is briefly unhidden as a "copied" badge so
// the click has visible feedback instead of failing silently.
function copyToClipboard(text, flashEl) {
  var done = function () {
    if (!flashEl) return;
    flashEl.hidden = false;
    setTimeout(function () { flashEl.hidden = true; }, 1500);
  };
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(text).then(done, function () {
      copyToClipboardLegacy(text, done);
    });
    return;
  }
  copyToClipboardLegacy(text, done);
}

function copyToClipboardLegacy(text, done) {
  var ta = document.createElement('textarea');
  ta.value = text;
  ta.setAttribute('readonly', '');
  ta.style.position = 'fixed';
  ta.style.opacity = '0';
  document.body.appendChild(ta);
  ta.select();
  try { document.execCommand('copy'); } catch (e) { /* fall through */ }
  document.body.removeChild(ta);
  done();
}

document.addEventListener('click', function (e) {
  var btn = e.target.closest('[data-copy]');
  if (!btn) return;
  e.preventDefault();
  var flash = btn.parentElement && btn.parentElement.querySelector('.copy-flash');
  copyToClipboard(btn.dataset.copy, flash);
});

document.addEventListener('click', function (e) {
  var btn = e.target.closest('.btn-tagger-cmd');
  if (!btn) return;
  var dlg = document.getElementById('tagger-cmd-dialog');
  if (!dlg) return;
  var host = btn.dataset.hostCmd || '';
  var docker = btn.dataset.dockerCmd || '';
  document.getElementById('tagger-cmd-name').textContent = btn.dataset.taggerName || '';
  document.getElementById('tagger-cmd-desc').textContent = btn.dataset.taggerDesc || '';
  document.getElementById('tagger-cmd-host').textContent = host;
  document.getElementById('tagger-cmd-docker').textContent = docker;
  document.getElementById('tagger-cmd-host-copy').dataset.copy = host;
  document.getElementById('tagger-cmd-docker-copy').dataset.copy = docker;
  dlg.showModal();
});

// Auto-reload gallery/tags after job completes; auto-clear status after 30s
var _jobAutoClearTimer = null;
// FinishedAt the current auto-clear timer was armed against; re-armed on newer
// surface events so rolling watcher activity doesn't trip the dismiss mid-batch
// (which would strip hx-trigger and silence the widget until page reload).
var _jobAutoClearFinishedAt = '';
// Track the FinishedAt timestamp of the last reloaded event so each new
// watcher/job completion triggers exactly one reload.
var _lastReloadedFinishedAt = '';
// The first job-status settle after page load reports whatever job last
// finished on the server, even when that job completed before the page was
// rendered. Reloading the grid/tags on that first poll is wasted work (and
// races user keystrokes) because the server-rendered DOM already reflects
// the post-job state. Skip exactly one reload, then resume normal behaviour
// so subsequent watcher/job events still surface live.
var _firstJobStatusSettle = true;
// Processed cursor for the currently-running job. The gallery grid is
// refreshed whenever the worker bumps this so changes show up without a
// manual reload, mirroring the watcher's per-event surface path. Applies
// to progress-emitting job types listed in the handler below.
var _lastJobProcessed = -1;
// Watcher-event counter. Bumped by the server whenever the filesystem
// watcher surfaces an ingest/remove while a job is running; gives the grid
// a refresh signal for job types that don't themselves change the image
// list (autotag) or whose progress cursor is scoped to existing rows.
var _lastWatcherNotices = -1;
// One-shot latch set by user-initiated deletes. When the delete job finishes,
// the htmx.ajax reload path intermittently fails to settle the gallery-grid
// swap, leaving the view stale. Force a full reload instead - only for the
// delete case; other job completions still use the incremental swap.
var _pendingGalleryReload = false;

document.body.addEventListener('htmx:afterSettle', function(e) {
  var el = e.detail.elt;

  // When the gallery grid is swapped (pagination, search, or job reload),
  // reset batch selection state to match the fresh (all-unchecked) checkboxes.
  // The swap wipes any .focused class; reapply it from the URL hash so the
  // arrow-key cursor doesn't vanish when a post-job refresh races the user.
  if (el && el.id === 'gallery-grid') {
    clearSelection();
    restoreGalleryFocusFromHash();
    initInboxUpload();
    return;
  }

  // After a tag-remove swap in tag-focus mode, restore .focused on the
  // tag-item one position before the one just deleted. Without this the
  // mode flag survives but the .focused class is wiped, so cycleTagFocus's
  // `current ? indexOf : 0` fallback drops the cursor to the first tag.
  if (el && el.id === 'image-tags' && document.body.classList.contains('tag-focus')) {
    var stash = document.body.dataset.tagFocusIdx;
    if (stash !== undefined) {
      delete document.body.dataset.tagFocusIdx;
      var items = document.querySelectorAll('#image-tags .tag-item');
      if (items.length === 0) {
        document.body.classList.remove('tag-focus');
      } else {
        var prev = Math.max(0, parseInt(stash, 10) - 1);
        if (prev >= items.length) prev = items.length - 1;
        items[prev].classList.add('focused');
        items[prev].scrollIntoView({block: 'nearest'});
      }
    }
  }

  if (!el || el.id !== 'job-status') return;

  // Latch first-settle once per page load. Used below to suppress the
  // initial done-state reload when the server-rendered DOM already reflects
  // the just-finished job.
  var firstSettle = _firstJobStatusSettle;
  _firstJobStatusSettle = false;

  var isDone = !!el.querySelector('.job-done');
  var isErr  = !!el.querySelector('.job-error');
  // `job-running` lives on #job-status itself, not a descendant, so use
  // classList instead of querySelector (which would always miss it).
  var isRunning = el.classList.contains('job-running');
  var finishedAt = el.dataset.finishedAt || '';

  // Running progress-emitting jobs: refresh the gallery grid whenever the
  // worker's Processed cursor advances so new ingests, deletions, and
  // re-extractions show up without a manual reload. The listed types call
  // jobs.Update(processed,...) inside their worker loops; others either
  // finish quickly (watcher events surface via FinishedAt) or don't
  // visibly alter the gallery during the run. Sits above the isIdle bail
  // so the running state reaches this branch.
  //
  // WatcherNotices covers the other half: the filesystem watcher may ingest
  // or remove files while any job runs, and autotag's own cursor doesn't
  // reflect those. A bump triggers the same grid refresh regardless of job
  // type (sync drops watcher events upstream, so its counter stays at 0).
  var jobType = el.dataset.jobType || '';
  var processed = parseInt(el.dataset.processed || '0', 10);
  var watcherNotices = parseInt(el.dataset.watcherNotices || '0', 10);
  var refreshDuringRun = jobType === 'sync' || jobType === 'delete' || jobType === 're-extract';
  if (!isRunning) {
    _lastJobProcessed = -1;
    _lastWatcherNotices = -1;
  } else {
    var needRefresh = false;
    if (refreshDuringRun && processed > 0 && processed !== _lastJobProcessed) {
      _lastJobProcessed = processed;
      needRefresh = true;
    }
    if (watcherNotices > 0 && watcherNotices !== _lastWatcherNotices) {
      _lastWatcherNotices = watcherNotices;
      needRefresh = true;
    }
    if (needRefresh) {
      var runningGrid = document.getElementById('gallery-grid');
      if (runningGrid && window.htmx) {
        var runURL = new URL(window.location.href);
        window.htmx.ajax('GET', runURL.pathname + runURL.search, {target: '#gallery-grid', swap: 'innerHTML'});
      }
    }
  }

  var isIdle = !isDone && !isErr && !isRunning;

  // Reset auto-clear flag when job-status goes idle (dismissed)
  if (isIdle) {
    _jobAutoClearFinishedAt = '';
    if (_jobAutoClearTimer) { clearTimeout(_jobAutoClearTimer); _jobAutoClearTimer = null; }
    return;
  }

  // Auto-clear 30s after the last surface event. Re-arm whenever FinishedAt
  // advances so rolling watcher events during a batch keep the widget alive.
  if ((isDone || isErr) && finishedAt && finishedAt !== _jobAutoClearFinishedAt) {
    _jobAutoClearFinishedAt = finishedAt;
    if (_jobAutoClearTimer) clearTimeout(_jobAutoClearTimer);
    _jobAutoClearTimer = setTimeout(function() {
      _jobAutoClearFinishedAt = '';
      dismissJobStatus();
    }, 30000);
  }

  // Post-delete: full reload guarantees the gallery / tags table
  // reflects the deletions once the background job finishes.
  if (isDone && _pendingGalleryReload) {
    _pendingGalleryReload = false;
    if (finishedAt) _lastReloadedFinishedAt = finishedAt;
    if (document.getElementById('gallery-grid') || document.getElementById('tags-page') || document.getElementById('collections-page')) {
      var pendingDone = el.querySelector('.job-done');
      if (pendingDone) stashGalleryFlash(pendingDone.textContent || '', 'ok');
      window.location.reload();
      return;
    }
  }

  // Reload gallery grid or detail tags once per completion event. The
  // data-finished-at attribute changes whenever the server records a new
  // event (e.g. sync complete, watcher add/remove), so reloads no longer
  // latch on a single flag.
  if (isDone && finishedAt && finishedAt !== _lastReloadedFinishedAt) {
    _lastReloadedFinishedAt = finishedAt;
    if (firstSettle) return;

    // Gallery page: reload grid + lift the job summary into the inline
    // flash slot so the user sees the result without having to scan the
    // top-right job-status widget.
    var grid = document.getElementById('gallery-grid');
    if (grid) {
      var doneEl = el.querySelector('.job-done');
      if (doneEl) showGalleryFlash(doneEl.textContent || '', 'ok');
      var url = new URL(window.location.href);
      if (window.htmx) {
        window.htmx.ajax('GET', url.pathname + url.search, {target: '#gallery-grid', swap: 'innerHTML'});
      }
    }

    // Detail page: reload the tag list so freshly added auto-tags show up.
    // The "Auto-tagger started..." / completion flash rides the shared
    // monbooru:flash slot which self-dismisses after 5 s.
    var imageTags = document.getElementById('image-tags');
    if (imageTags) {
      var imageId = imageTags.dataset.imageId;
      if (imageId && window.htmx) {
        window.htmx.ajax('GET', '/images/' + imageId + '/tags', {target: '#image-tags', swap: 'outerHTML'});
      }
    }
  }
});

function getCSRFToken() {
  var meta = document.querySelector('meta[name="csrf-token"]');
  if (meta) return meta.content;
  var input = document.querySelector('input[name="_csrf"]');
  return input ? input.value : '';
}

// scopeCount populates the dialog's count + noun span pair from the
// current search-result span (search scope) or the checked-thumb count
// (selection scope). Returns the resolved count so callers can early-
// return on an empty selection.
function scopeCount(scope, countEl, nounEl) {
  var n = 0;
  if (scope === 'selection') {
    n = document.querySelectorAll('.thumb-checkbox:checked').length;
  } else {
    var rcEl = document.querySelector('.result-count');
    if (rcEl) {
      var m = rcEl.textContent.match(/(\d+)/);
      if (m) n = parseInt(m[1], 10);
    }
  }
  if (countEl) countEl.textContent = n;
  if (nounEl) {
    var suffix = n === 1 ? 'image' : 'images';
    nounEl.textContent = scope === 'selection' ? 'selected ' + suffix : suffix + ' in current search';
  }
  return n;
}

// searchScopeParts returns the query/sort/order body fragment used by
// every search-scoped batch endpoint.
function searchScopeParts() {
  var si = document.getElementById('search-input');
  var sortEl = document.getElementById('search-sort');
  var orderEl = document.querySelector('#search-form select[name="order"]');
  return ['q=' + encodeURIComponent(si ? si.value : ''),
          'sort=' + encodeURIComponent(sortEl ? sortEl.value : 'newest'),
          'order=' + encodeURIComponent(orderEl ? orderEl.value : 'desc')];
}

// selectionScopeIds returns the checked-thumb id parts. Returns null
// when nothing is checked (caller writes the flash and aborts).
function selectionScopeIds() {
  var checked = document.querySelectorAll('.thumb-checkbox:checked');
  if (checked.length === 0) return null;
  return Array.prototype.map.call(checked, function(cb) {
    return 'ids=' + encodeURIComponent(cb.value);
  });
}

// runBatchOp fires the named endpoint, closing the dialog and refreshing
// job-status on success. opts: endpoint, scope, params (array of
// already-encoded "k=v" parts not including _csrf or scope), dialogId,
// flashId, failMsg, clearOnOk (defaults to scope === 'selection').
function runBatchOp(opts) {
  var flash = document.getElementById(opts.flashId);
  if (flash) flash.innerHTML = '';
  var csrfEl = document.querySelector('input[name="_csrf"]');
  var csrf = csrfEl ? csrfEl.value : '';
  var parts = ['_csrf=' + encodeURIComponent(csrf),
               'scope=' + encodeURIComponent(opts.scope)];
  if (opts.params) parts = parts.concat(opts.params);
  var clearOnOk = opts.clearOnOk;
  if (clearOnOk === undefined) clearOnOk = opts.scope === 'selection';
  fetch(opts.endpoint, {
    method: 'POST',
    headers: {'Content-Type': 'application/x-www-form-urlencoded', 'X-CSRF-Token': csrf},
    body: parts.join('&')
  }).then(function(res) {
    if (res.ok) {
      var dlg = document.getElementById(opts.dialogId);
      if (dlg) dlg.close();
      if (clearOnOk) clearSelection();
      _pendingGalleryReload = true;
      refreshJobStatus();
    } else {
      res.text().then(function(t) {
        if (flash) flash.innerHTML = t || '<div class="flash flash-err">' + (opts.failMsg || 'Action failed.') + '</div>';
      });
    }
  }).catch(function() {
    if (flash) flash.innerHTML = '<div class="flash flash-err">Request failed.</div>';
  });
}

// openBatchDialog runs the shared open skeleton for a batch dialog whose
// elements share an id prefix (#<prefix>-dialog/-scope/-count/-noun/-flash):
// guard an empty selection, fill the count + noun pair, stamp the scope,
// reset the per-dialog extras, clear the flash, showModal. opts:
//   countId       - count element id when it isn't <prefix>-count
//   requireMatches - also abort a search-scope open that matches 0 images
//   radio         - selector of the radio re-checked to its default
//   clearIds      - element ids emptied on open (inputs get value='',
//                   suggest dropdowns innerHTML='')
//   clearReturnTo - drop any stale chooser back-link from a prior open;
//                   pickBatchAction re-applies it on the chooser-open path
//                   after this runs (shared batch-bar/chooser dialogs only)
//   beforeShow    - hook run after the resets, before showModal
//   focusId       - input focused after showModal
function openBatchDialog(prefix, scope, opts) {
  opts = opts || {};
  if (scope === 'selection' && document.querySelectorAll('.thumb-checkbox:checked').length === 0) return;
  var n = scopeCount(scope, document.getElementById(opts.countId || prefix + '-count'),
                     document.getElementById(prefix + '-noun'));
  if (opts.requireMatches && scope === 'search' && n === 0) return;
  document.getElementById(prefix + '-scope').value = scope;
  if (opts.radio) {
    var dflt = document.querySelector(opts.radio);
    if (dflt) dflt.checked = true;
  }
  (opts.clearIds || []).forEach(function(id) {
    var el = document.getElementById(id);
    if (!el) return;
    if (el.tagName === 'INPUT') el.value = ''; else el.innerHTML = '';
  });
  var flash = document.getElementById(prefix + '-flash');
  if (flash) flash.innerHTML = '';
  var dlg = document.getElementById(prefix + '-dialog');
  if (opts.clearReturnTo && dlg) delete dlg.dataset.returnTo;
  if (opts.beforeShow) opts.beforeShow();
  dlg.showModal();
  var focusEl = opts.focusId ? document.getElementById(opts.focusId) : null;
  if (focusEl) focusEl.focus();
}

// clearSlots empties each listed element by id, skipping ids absent from
// the page: the shared "reset error/suggest/flash slots" step of dialog-open.
function clearSlots() {
  for (var i = 0; i < arguments.length; i++) {
    var el = document.getElementById(arguments[i]);
    if (el) el.innerHTML = '';
  }
}

// openHxRewriteDialog repoints a form's htmx attribute (attr is 'hx-post'
// or 'hx-patch') at url, re-processes the form so htmx picks up the new
// target, and opens the dialog. Callers prefill before, focus after.
function openHxRewriteDialog(formId, attr, url, dialogId) {
  var form = document.getElementById(formId);
  form.setAttribute(attr, url);
  if (window.htmx) window.htmx.process(form);
  document.getElementById(dialogId).showModal();
}

// confirmBatchSimple runs the shared confirm shape: resolve the scope the
// open path stashed in #<prefix>-scope into query or id params (flashing
// on an empty selection), then post through runBatchOp. extraParams are
// already-encoded "k=v" parts placed before the scope params.
function confirmBatchSimple(prefix, endpoint, extraParams, failMsg) {
  var scope = document.getElementById(prefix + '-scope').value;
  var params = extraParams || [];
  if (scope === 'search') {
    params = params.concat(searchScopeParts());
  } else {
    var ids = selectionScopeIds();
    if (!ids) {
      document.getElementById(prefix + '-flash').innerHTML = '<div class="flash flash-err">No images selected.</div>';
      return;
    }
    params = params.concat(ids);
  }
  runBatchOp({endpoint: endpoint, scope: scope, params: params,
              dialogId: prefix + '-dialog', flashId: prefix + '-flash', failMsg: failMsg});
}

// postForm sends a form-encoded request with the page CSRF token in both
// the body and the X-CSRF-Token header, then applies runBatchOp's
// ok-close / err-flash shape. params is a URLSearchParams or plain object
// (or null); _csrf is appended here. opts:
//   method   - HTTP verb, default 'POST'
//   hx       - also send the HX-Request header (relations bulk endpoints)
//   okStatus - success only on this exact status; default is res.ok
//   dialogId - dialog closed on success
//   onOK     - called with the response after the dialog close
//   flashId  - element that receives the error body / failure flashes
//   failMsg  - fallback error text when the error body is empty
//   onErr    - called with the error body text instead of the flash write
//   catchMsg - network-failure flash text (default 'Request failed.');
//              pass null to keep a converted site's original no-catch shape
function postForm(url, params, opts) {
  opts = opts || {};
  var csrf = getCSRFToken();
  var body = params instanceof URLSearchParams ? params : new URLSearchParams(params || {});
  body.append('_csrf', csrf);
  var headers = {'Content-Type': 'application/x-www-form-urlencoded', 'X-CSRF-Token': csrf};
  if (opts.hx) headers['HX-Request'] = 'true';
  var flash = opts.flashId ? document.getElementById(opts.flashId) : null;
  var p = fetch(url, {
    method: opts.method || 'POST',
    headers: headers,
    body: body.toString()
  }).then(function(res) {
    var ok = opts.okStatus ? res.status === opts.okStatus : res.ok;
    if (ok) {
      var dlg = opts.dialogId ? document.getElementById(opts.dialogId) : null;
      if (dlg) dlg.close();
      if (opts.onOK) opts.onOK(res);
    } else {
      res.text().then(function(t) {
        if (opts.onErr) { opts.onErr(t); return; }
        if (flash) flash.innerHTML = t || (opts.failMsg ? '<div class="flash flash-err">' + opts.failMsg + '</div>' : '');
      });
    }
  });
  if (opts.catchMsg !== null) {
    p.catch(function() {
      if (flash) flash.innerHTML = '<div class="flash flash-err">' + (opts.catchMsg || 'Request failed.') + '</div>';
    });
  }
}

function dismissJobStatus() {
  _lastReloadedFinishedAt = '';
  _lastJobProcessed = -1;
  _lastWatcherNotices = -1;
  _jobAutoClearFinishedAt = '';
  if (_jobAutoClearTimer) { clearTimeout(_jobAutoClearTimer); _jobAutoClearTimer = null; }
  postForm('/internal/job/dismiss');
  var js = document.getElementById('job-status');
  if (js) {
    js.innerHTML = '';
    js.removeAttribute('hx-trigger');
    if (window.htmx) window.htmx.process(js);
  }
}

// cancelJobStatus interrupts the running auto-tagging job. The worker observes
// ctx.Done() and wraps up via Complete; the 30s auto-dismiss takes over from
// there so the user still sees a "cancelled" summary on the status bar.
function cancelJobStatus() {
  postForm('/internal/job/cancel');
}

// Shared suggest-dropdown keyboard navigation (search, tag input, merge)
function handleSuggestKey(e, dropdownId, inputId) {
  var dd = document.getElementById(dropdownId);
  if (!dd) return;
  var items = Array.from(dd.querySelectorAll('.suggest-item'));
  var focused = dd.querySelector('.suggest-item.kbd-focused');
  var idx = focused ? items.indexOf(focused) : -1;
  if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
    e.preventDefault();
    items.forEach(function(i){ i.classList.remove('kbd-focused'); });
    idx = e.key === 'ArrowDown' ? Math.min(idx + 1, items.length - 1) : Math.max(idx - 1, 0);
    if (idx >= 0) {
      items[idx].classList.add('kbd-focused');
      items[idx].scrollIntoView({ block: 'nearest' });
    }
  } else if (e.key === 'Enter' && focused) {
    e.preventDefault();
    focused.click();
  } else if (e.key === 'Escape') {
    dd.innerHTML = '';
  }
}

// Close suggest dropdown when clicking outside, or when the user submits
// the enclosing form (Enter or the Search button). Without the submit
// hook the dropdown stays visible on top of the result page until the
// next outside click.
//
// opts.blurOnSubmit: also blur the input on submit. Used for the search
// form so page-level arrow-key gallery navigation kicks in after Enter
// without an extra Escape press; the tag/folder dropdowns are designed
// for repeated entry and keep focus.
function initSuggestDismiss(dropdownId, inputId, opts) {
  document.addEventListener('click', function(e) {
    var dd = document.getElementById(dropdownId);
    if (dd && !dd.contains(e.target) && e.target.id !== inputId) {
      dd.innerHTML = '';
    }
  });
  var input = document.getElementById(inputId);
  var form = input && input.form;
  if (form) {
    form.addEventListener('submit', function() {
      var dd = document.getElementById(dropdownId);
      if (dd) dd.innerHTML = '';
      if (opts && opts.blurOnSubmit && input) input.blur();
    });
  }
  // A pending suggest request (debounced 200ms by hx-trigger) can land
  // after the user submits or moves focus elsewhere; drop the swap if
  // the input no longer holds focus so the dropdown doesn't get refilled
  // behind the user's back.
  //
  // Also tag the dropdown as `suggest-fresh` after every swap so the CSS
  // can suppress the :hover highlight until the user actually moves the
  // mouse over it - otherwise an item happening to land under the cursor's
  // previous position appears "selected" without the user picking it.
  var dd0 = document.getElementById(dropdownId);
  if (dd0) {
    dd0.addEventListener('htmx:afterSwap', function() {
      if (document.activeElement !== input) { dd0.innerHTML = ''; return; }
      dd0.classList.add('suggest-fresh');
    });
    dd0.addEventListener('mousemove', function() {
      dd0.classList.remove('suggest-fresh');
    });
  }
}

initSuggestDismiss('search-suggest', 'search-input', {blurOnSubmit: true});
initSuggestDismiss('tag-suggest-dropdown', 'tag-input');
initSuggestDismiss('merge-suggest', 'merge-canon-input');
initSuggestDismiss('batch-move-suggest', 'batch-move-folder');
initSuggestDismiss('move-image-suggest', 'move-image-folder');
initSuggestDismiss('batch-tag-suggest', 'batch-tag-input');
initSuggestDismiss('batch-strip-suggest', 'batch-strip-input');
initSuggestDismiss('source-suggest', 'source-site-input');
initSuggestDismiss('batch-series-search-suggest', 'batch-series-search-input');
initSuggestDismiss('batch-series-selected-suggest', 'batch-series-selected-input');

// Detail page: tags added in the current session are split into a "just-added"
// list and reset on full page reload.
var _initialTagIDs = null;
var _addedTagOrder = [];

function captureInitialTags(container) {
  if (_initialTagIDs !== null) return;
  _initialTagIDs = new Set();
  container.querySelectorAll('.tag-list > li.tag-item[data-tag-id]').forEach(function(li) {
    _initialTagIDs.add(li.dataset.tagId);
  });
}

// Always keep auto-tagged items in their tagger subcategory; never treat
// them as "just added" even when an auto-tag run happens mid-session.
function registerInitialAutoTags(container) {
  if (_initialTagIDs === null) return;
  container.querySelectorAll('.tag-list > li.tag-item[data-source="auto"][data-tag-id]').forEach(function(li) {
    _initialTagIDs.add(li.dataset.tagId);
  });
}

function separateNewTags(container) {
  if (!container) return;
  if (_initialTagIDs === null) { captureInitialTags(container); return; }
  // Auto-tag runs that happened after page load expose new auto tags in their
  // tagger subcategory; pin them into the initial set so they don't drift into
  // "Just added" (which is reserved for tags the user typed in this session).
  registerInitialAutoTags(container);

  var added = container.querySelector('.tag-list-added');
  var divider = container.querySelector('.tag-list-divider');
  var title = container.querySelector('.tag-list-added-title');
  if (!added) return;

  // Scan every user-source tag-item across all subcategory lists for this image.
  var nodesById = {};
  container.querySelectorAll('.tag-list:not(.tag-list-added) li.tag-item[data-source="user"][data-tag-id]').forEach(function(li) {
    var id = li.dataset.tagId;
    if (_initialTagIDs.has(id)) return;
    nodesById[id] = li;
    if (_addedTagOrder.indexOf(id) === -1) _addedTagOrder.push(id);
  });
  _addedTagOrder = _addedTagOrder.filter(function(id) { return nodesById[id] !== undefined; });

  // appendChild on an existing node moves it; insertion order is preserved.
  _addedTagOrder.forEach(function(id) { added.appendChild(nodesById[id]); });

  // Hide subcategory groups (e.g. "Tags added by the user") whose tag list
  // just became empty after we moved items to "Just added". Without this the
  // header stays visible above an empty list on first add.
  container.querySelectorAll('.tag-source-group').forEach(function(group) {
    var list = group.querySelector('.tag-list:not(.tag-list-added)');
    group.hidden = !!list && list.children.length === 0;
  });

  var hasAdded = added.children.length > 0;
  if (divider) divider.hidden = !hasAdded;
  if (title) title.hidden = !hasAdded;
}

document.body.addEventListener('htmx:afterSettle', function(e) {
  var el = e.detail ? e.detail.elt : null;
  if (!el || el.id !== 'image-tags') return;
  separateNewTags(el);
  // Re-anchor tag-focus after the swap. The deleted-row's <li> takes the
  // .focused class with it, so without this the next ArrowRight / Enter
  // has no cursor to act on and the user has to press r again to keep
  // deleting.
  if (document.body.classList.contains('tag-focus') &&
      !el.querySelector('.tag-item.focused')) {
    var first = el.querySelector('.tag-item');
    if (first) {
      first.classList.add('focused');
      first.scrollIntoView({block: 'nearest'});
    } else {
      // Image has no tags left - leave the mode rather than holding it
      // open against an empty list.
      document.body.classList.remove('tag-focus');
    }
  }
});

document.addEventListener('DOMContentLoaded', function() {
  var el = document.getElementById('image-tags');
  if (el) captureInitialTags(el);
});


// Mobile sidebar drawer: at narrow widths the topbar wraps to several
// rows (nav + topbar-right + logo), so the drawer's hard-coded top:42px
// would cover the wrapped nav links. Mirror the real topbar height into
// a --topbar-h custom property so the drawer always slots in below it.
(function () {
  var root = document.documentElement;
  function sync() {
    var tb = document.getElementById('topbar');
    if (!tb) return;
    root.style.setProperty('--topbar-h', Math.round(tb.getBoundingClientRect().height) + 'px');
  }
  document.addEventListener('DOMContentLoaded', sync);
  window.addEventListener('resize', sync);
  window.addEventListener('load', sync);
})();

// Reader: clicking the page counter opens the page-jump dialog.
document.addEventListener('DOMContentLoaded', function() {
  var counter = document.getElementById('reader-counter');
  if (counter && document.getElementById('reader-jump-dialog')) {
    counter.addEventListener('click', function(e) {
      e.preventDefault();
      openReaderJumpDialog();
    });
  }
});

// Pages grid: when the URL carries a #page-N fragment (set by the
// reader's Back / Pages link so the operator returns to the cell they
// were just reading), focus that cell so the keyboard nav picks up
// from the right spot and the cell is visually anchored.
document.addEventListener('DOMContentLoaded', function() {
  if (!document.getElementById('pages-grid-page')) return;
  var hash = window.location.hash;
  if (!hash || hash.indexOf('#page-') !== 0) return;
  var target = document.querySelector(hash + '.manga-page-cell');
  if (!target) return;
  var cells = Array.from(document.querySelectorAll('.manga-page-cell'));
  // block: 'center' - the return-from-reader landing wants the cell
  // anchored mid-viewport, not just nudged into view.
  setFocused(cells, cells.indexOf(target), 'center');
});

// initInboxUpload wires the inline drop zone the gallery renders at the
// top of the grid whenever the query positively asserts inbox:true.
// Idempotent via a data-wired latch on the drop zone so repeat calls
// (htmx grid swaps) don't pile up listeners. The drop zone reuses the
// upload page's CSS hooks; the post-submit reload uses htmx.ajax against
// the current URL so the new inbox rows show up without a full reload.
function initInboxUpload() {
  var dz = document.getElementById('inbox-upload-drop');
  var input = document.getElementById('inbox-upload-file-input');
  var list = document.getElementById('inbox-upload-file-list');
  var pickBtn = document.getElementById('inbox-upload-pick-btn');
  var form = document.getElementById('inbox-upload-form');
  var submitBtn = document.getElementById('inbox-upload-submit-btn');
  var resetBtn = document.getElementById('inbox-upload-reset-btn');
  var result = document.getElementById('inbox-upload-result');
  if (!dz || !input || !list || !form || dz.dataset.wired === '1') return;
  dz.dataset.wired = '1';

  // _pending owns the staged FileList. The browser's native file
  // picker overwrites input.files on every change, so we mirror the
  // accumulating set here and copy it back onto input before submit.
  var _pending = new DataTransfer();

  function renderList() {
    list.innerHTML = '';
    var files = _pending.files;
    for (var i = 0; i < files.length; i++) {
      var li = document.createElement('li');
      var kib = Math.round(files[i].size / 1024);
      var sizeText = kib > 0 ? (kib + ' KiB') : '<1 KiB';
      li.textContent = files[i].name + ' (' + sizeText + ')';
      list.appendChild(li);
    }
    input.files = _pending.files;
    var empty = files.length === 0;
    if (submitBtn) submitBtn.disabled = empty;
    if (resetBtn) resetBtn.disabled = empty;
  }

  function appendFiles(incoming) {
    for (var i = 0; i < incoming.length; i++) _pending.items.add(incoming[i]);
    renderList();
  }

  function clearPending() {
    _pending = new DataTransfer();
    renderList();
    if (result) result.innerHTML = '';
  }

  input.addEventListener('change', function() {
    // Browser picker returned a fresh selection; append onto the
    // pending set rather than replace it. The renderList sync at the
    // end of appendFiles writes _pending.files back over input.files
    // so the form submit carries the full accumulated set.
    if (input.files && input.files.length) appendFiles(input.files);
  });
  if (pickBtn) {
    pickBtn.addEventListener('click', function(e) { e.preventDefault(); input.click(); });
  }
  if (resetBtn) {
    resetBtn.addEventListener('click', function(e) { e.preventDefault(); clearPending(); });
  }
  // Initial empty state disables submit / reset until the user picks
  // or drops a file. An empty submit posts /upload as a no-op (server
  // 200, no rows added) and an empty reset does nothing visible;
  // surfacing the disabled state up front is honest.
  renderList();

  ['dragenter', 'dragover'].forEach(function(ev) {
    dz.addEventListener(ev, function(e) { e.preventDefault(); dz.classList.add('drag-over'); });
  });
  ['dragleave', 'drop'].forEach(function(ev) {
    dz.addEventListener(ev, function(e) { e.preventDefault(); dz.classList.remove('drag-over'); });
  });
  dz.addEventListener('drop', function(e) {
    if (!e.dataTransfer || !e.dataTransfer.files) return;
    appendFiles(e.dataTransfer.files);
  });

  form.addEventListener('htmx:beforeRequest', function() {
    if (submitBtn) submitBtn.disabled = true;
    if (result) result.innerHTML = '<div class="field-hint">Uploading...</div>';
  });
  form.addEventListener('htmx:afterRequest', function(e) {
    if (submitBtn) submitBtn.disabled = false;
    if (!e.detail.successful) return;
    // The upload handler renders the summary flash into #inbox-upload-result;
    // that slot lives inside #gallery-grid and the upcoming refresh would
    // wipe it. Lift the text into the gallery-level slot before the swap.
    if (result && result.innerHTML.trim() !== '') {
      showGalleryFlash(result.innerHTML, /flash-err/.test(result.innerHTML) ? 'err' : 'ok');
    }
    clearPending();
    // Refresh the grid so the freshly ingested rows surface at the top
    // of the inbox. Same idiom the job-status auto-refresh uses.
    var grid = document.getElementById('gallery-grid');
    if (grid && window.htmx) {
      var u = new URL(window.location.href);
      window.htmx.ajax('GET', u.pathname + u.search, {target: '#gallery-grid', swap: 'innerHTML'});
    }
  });
}

document.addEventListener('DOMContentLoaded', initInboxUpload);
