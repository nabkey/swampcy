// swampcy web: the headless desktop in the browser (docs/web.md).
//
// The picture is a WebRTC video track (AV1 where this browser decodes it, H.264
// otherwise) drawn by a <video>; the signalling is JSON on /ws; taps, clicks and keys
// go back as small binary messages on the data channel "input".

const $ = (id) => document.getElementById(id);
const app = $('app');
const stage = $('stage');
const view = $('view');
const video = $('screen');
const fx = $('fx');
const overlay = $('overlay');
const bar = $('bar');
const dot = $('dot');
const statsEl = $('stats');
const fitBtn = $('fit');
const keysEl = $('keys');
const kbd = $('kbd');
const btn = {
  kbd: $('b-kbd'), fit: $('b-fit'), drag: $('b-drag'), keys: $('b-keys'),
  stats: $('b-stats'), more: $('b-more'), paste: $('b-paste'), desk: $('b-desk'), wins: $('b-wins'), apps: $('b-apps'), zoom: $('b-zoom'),
};
const chooser = $('chooser');
const winsEl = $('wins');
const appsEl = $('apps');
// The page's own panels and buttons over the stage, whose input is not the desktop's.
const onUI = (e) => !!e.target.closest?.('#fit, #chooser, #wins, #apps');

const coarse = matchMedia('(pointer: coarse)').matches;
const clamp = (v, lo, hi) => Math.min(hi, Math.max(lo, v));

function load(key, fallback) {
  try {
    const v = localStorage.getItem('swampcy.' + key);
    return v === null ? fallback : JSON.parse(v);
  } catch {
    return fallback;
  }
}
function save(key, v) {
  try {
    localStorage.setItem('swampcy.' + key, JSON.stringify(v));
  } catch {
    /* private mode: preferences last for the page */
  }
}

// ---- Keys: KeyboardEvent.code -> Linux evdev keycode (linux/input-event-codes.h) ----

const KEYCODES = {
  Escape: 1, Digit1: 2, Digit2: 3, Digit3: 4, Digit4: 5, Digit5: 6, Digit6: 7, Digit7: 8,
  Digit8: 9, Digit9: 10, Digit0: 11, Minus: 12, Equal: 13, Backspace: 14, Tab: 15,
  KeyQ: 16, KeyW: 17, KeyE: 18, KeyR: 19, KeyT: 20, KeyY: 21, KeyU: 22, KeyI: 23, KeyO: 24,
  KeyP: 25, BracketLeft: 26, BracketRight: 27, Enter: 28, ControlLeft: 29, KeyA: 30,
  KeyS: 31, KeyD: 32, KeyF: 33, KeyG: 34, KeyH: 35, KeyJ: 36, KeyK: 37, KeyL: 38,
  Semicolon: 39, Quote: 40, Backquote: 41, ShiftLeft: 42, Backslash: 43, KeyZ: 44,
  KeyX: 45, KeyC: 46, KeyV: 47, KeyB: 48, KeyN: 49, KeyM: 50, Comma: 51, Period: 52,
  Slash: 53, ShiftRight: 54, NumpadMultiply: 55, AltLeft: 56, Space: 57, CapsLock: 58,
  F1: 59, F2: 60, F3: 61, F4: 62, F5: 63, F6: 64, F7: 65, F8: 66, F9: 67, F10: 68,
  NumLock: 69, ScrollLock: 70, Numpad7: 71, Numpad8: 72, Numpad9: 73, NumpadSubtract: 74,
  Numpad4: 75, Numpad5: 76, Numpad6: 77, NumpadAdd: 78, Numpad1: 79, Numpad2: 80,
  Numpad3: 81, Numpad0: 82, NumpadDecimal: 83, IntlBackslash: 86, F11: 87, F12: 88,
  IntlRo: 89, Convert: 92, KanaMode: 93, NonConvert: 94, NumpadEnter: 96,
  ControlRight: 97, NumpadDivide: 98, PrintScreen: 99, AltRight: 100, Home: 102,
  ArrowUp: 103, PageUp: 104, ArrowLeft: 105, ArrowRight: 106, End: 107, ArrowDown: 108,
  PageDown: 109, Insert: 110, Delete: 111, AudioVolumeMute: 113, AudioVolumeDown: 114,
  AudioVolumeUp: 115, Power: 116, NumpadEqual: 117, Pause: 119, NumpadComma: 121,
  Lang1: 122, Lang2: 123, IntlYen: 124, MetaLeft: 125, MetaRight: 126, OSLeft: 125,
  OSRight: 126, ContextMenu: 127, BrowserStop: 128, Again: 129, Props: 130, Undo: 131,
  Select: 132, Copy: 133, Open: 134, Paste: 135, Find: 136, Cut: 137, Help: 138,
  LaunchApp2: 140, Sleep: 142, WakeUp: 143, LaunchApp1: 144, LaunchMail: 155,
  BrowserFavorites: 156, BrowserBack: 158, BrowserForward: 159, Eject: 161,
  MediaTrackNext: 163, MediaPlayPause: 164, MediaTrackPrevious: 165, MediaStop: 166,
  BrowserHome: 172, BrowserRefresh: 173, NumpadParenLeft: 179, NumpadParenRight: 180,
  F13: 183, F14: 184, F15: 185, F16: 186, F17: 187, F18: 188, F19: 189, F20: 190,
  F21: 191, F22: 192, F23: 193, F24: 194, MediaSelect: 226, BrowserSearch: 217,
};
const KEY = { esc: 1, backspace: 14, tab: 15, enter: 28, delete: 111 };
const MODKEY = { ctrl: 29, shift: 42, alt: 56, super: 125 };
const MODIFIER_CODES = new Set([
  'ControlLeft', 'ControlRight', 'ShiftLeft', 'ShiftRight', 'AltLeft', 'AltRight',
  'MetaLeft', 'MetaRight', 'OSLeft', 'OSRight',
]);

// A printable ASCII character on the US layout: [keycode, shift]. Used only when a
// sticky modifier turns a typed character into a shortcut (Ctrl, then "c").
const US = {};
{
  const rows = [
    ['`1234567890-=', '~!@#$%^&*()_+', [41, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13]],
    ['qwertyuiop[]\\', 'QWERTYUIOP{}|', [16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 43]],
    ["asdfghjkl;'", 'ASDFGHJKL:"', [30, 31, 32, 33, 34, 35, 36, 37, 38, 39, 40]],
    ['zxcvbnm,./', 'ZXCVBNM<>?', [44, 45, 46, 47, 48, 49, 50, 51, 52, 53]],
  ];
  for (const [plain, shifted, codes] of rows) {
    [...plain].forEach((c, i) => (US[c] = [codes[i], false]));
    [...shifted].forEach((c, i) => (US[c] = [codes[i], true]));
  }
  US[' '] = [57, false];
}

// ---- Input messages (spec: Input) -------------------------------------------------

const enc = new TextEncoder();
let dc = null; // the "input" data channel of the current session
let W = 0; // the screen, in pixels, from the answer
let H = 0;

function send(buf) {
  if (dc && dc.readyState === 'open') dc.send(buf);
}
function msg(kind, size) {
  const b = new ArrayBuffer(size);
  const v = new DataView(b);
  v.setUint8(0, kind);
  return [b, v];
}
function sendMove([x, y]) {
  const [b, v] = msg(0x01, 5);
  v.setUint16(1, x, true);
  v.setUint16(3, y, true);
  send(b);
}
function sendButton(button, down, [x, y]) {
  cancelMove();
  const [b, v] = msg(0x02, 7);
  v.setUint8(1, button);
  v.setUint8(2, down ? 1 : 0);
  v.setUint16(3, x, true);
  v.setUint16(5, y, true);
  send(b);
}
function sendWheel(dx, dy, [x, y]) {
  cancelMove();
  while (dx || dy) {
    const sx = clamp(dx, -127, 127);
    const sy = clamp(dy, -127, 127);
    const [b, v] = msg(0x03, 7);
    v.setInt8(1, sx);
    v.setInt8(2, sy);
    v.setUint16(3, x, true);
    v.setUint16(5, y, true);
    send(b);
    dx -= sx;
    dy -= sy;
  }
}
function sendKey(code, down) {
  const [b, v] = msg(0x04, 4);
  v.setUint16(1, code, true);
  v.setUint8(3, down ? 1 : 0);
  send(b);
}
function sendText(s) {
  const t = enc.encode(s);
  const b = new Uint8Array(t.length + 1);
  b[0] = 0x05;
  b.set(t, 1);
  send(b.buffer);
}
// The viewer's clipboard, pasted on the desktop as a clipboard: the server
// makes it the selection of the seat the keys go to and presses Ctrl+V there, so the
// app pastes it whole, newlines included. One message, so at most what the data
// channel carries.
function sendPaste(s) {
  if (!s) return;
  const t = enc.encode(s);
  const max = (pc?.sctp?.maxMessageSize || 65535) - 1;
  if (t.length > max) {
    notice(`too long to paste: ${Math.ceil(t.length / 1024)} KB, and the most is ${Math.floor(max / 1024)} KB`);
    return;
  }
  const b = new Uint8Array(t.length + 1);
  b[0] = 0x07;
  b.set(t, 1);
  send(b.buffer);
}
function sendRelease() {
  send(new Uint8Array([0x06]).buffer);
}

// Pointer moves are coalesced to one per animation frame.
let pendingMove = null;
let moveRaf = 0;
function queueMove(p) {
  pendingMove = p;
  if (!moveRaf) moveRaf = requestAnimationFrame(flushMove);
}
function flushMove() {
  moveRaf = 0;
  if (pendingMove) sendMove(pendingMove);
  pendingMove = null;
}
function cancelMove() {
  pendingMove = null;
}

// A click from a tap: press now, release a moment later, since an app may ignore a
// press and release that arrive together.
function click(button, p) {
  sendButton(button, true, p);
  setTimeout(() => sendButton(button, false, p), 50);
}

// ---- Layout: the frame letterboxed in the stage, and a zoom on top -----------------

const viewT = { s: 1, x: 0, y: 0 };

function layout() {
  const sw = stage.clientWidth;
  const sh = stage.clientHeight;
  const fw = W || video.videoWidth || 16;
  const fh = H || video.videoHeight || 10;
  const k = Math.min(sw / fw, sh / fh);
  const w = Math.round(fw * k);
  const h = Math.round(fh * k);
  video.style.width = `${w}px`;
  video.style.height = `${h}px`;
  video.style.left = `${Math.round((sw - w) / 2)}px`;
  video.style.top = `${Math.round((sh - h) / 2)}px`;
  clampView();
  applyView();
}
function clampView() {
  const sw = stage.clientWidth;
  const sh = stage.clientHeight;
  viewT.s = clamp(viewT.s, 1, 5);
  viewT.x = clamp(viewT.x, sw * (1 - viewT.s), 0);
  viewT.y = clamp(viewT.y, sh * (1 - viewT.s), 0);
}
function applyView() {
  view.style.transform = `translate(${viewT.x}px, ${viewT.y}px) scale(${viewT.s})`;
  fitBtn.hidden = viewT.s <= 1.001;
}
function resetView() {
  viewT.s = 1;
  viewT.x = 0;
  viewT.y = 0;
  applyView();
}
new ResizeObserver(layout).observe(stage);
video.addEventListener('resize', layout); // the decoded size, before the answer's arrives

// A client point to a frame pixel, through the letterbox and the zoom (the video's
// bounding box already includes both). null when outside the frame unless clamped.
function toFrame(cx, cy, clampToFrame = false) {
  const fw = W || video.videoWidth;
  const fh = H || video.videoHeight;
  if (!fw || !fh) return null;
  const r = video.getBoundingClientRect();
  if (!r.width || !r.height) return null;
  let x = ((cx - r.left) / r.width) * fw;
  let y = ((cy - r.top) / r.height) * fh;
  if (!clampToFrame && (x < 0 || y < 0 || x >= fw || y >= fh)) return null;
  x = clamp(Math.floor(x), 0, fw - 1);
  y = clamp(Math.floor(y), 0, fh - 1);
  return [x, y];
}

// iOS lays the keyboard over the page. Size the app to the visible part instead, so the
// key bar sits on top of the keyboard and the desktop refits above it.
if (window.visualViewport) {
  const vv = window.visualViewport;
  const place = () => {
    app.style.height = `${vv.height}px`;
    app.style.transform = `translateY(${vv.offsetTop}px)`;
  };
  vv.addEventListener('resize', place);
  vv.addEventListener('scroll', place);
  place();
}

// ---- Touch --------------------------------------------------------------------------
//
// One finger: tap clicks; a long press right-clicks; a drag pans the view when zoomed in,
// and drags with the left button at 1x (or always, in drag mode). Two fingers: a pinch
// zooms the view; moving them together scrolls.

let dragMode = load('dragMode', false);
// How far a finger may drift, in CSS pixels, and still tap: a fingertip rolls
// further than 10 as it lifts, which turned taps into drags, or pans when zoomed.
const TAP_SLOP = 16;
const T = { mode: 'idle', start: null, last: null, t0: 0, timer: 0, two: null };

const dist = (a, b) => Math.hypot(a.clientX - b.clientX, a.clientY - b.clientY);
const mid = (a, b) => ({ x: (a.clientX + b.clientX) / 2, y: (a.clientY + b.clientY) / 2 });

function pulse(p) {
  const r = stage.getBoundingClientRect();
  const el = document.createElement('div');
  el.className = 'pulse';
  el.style.left = `${p.x - r.left}px`;
  el.style.top = `${p.y - r.top}px`;
  el.addEventListener('animationend', () => el.remove());
  fx.append(el);
}

function longPress() {
  if (T.mode !== 'pending') return;
  T.mode = 'done';
  const f = toFrame(T.start.x, T.start.y);
  if (!f) return;
  click(1, f);
  pulse(T.start);
  navigator.vibrate?.(8);
}

// Ends a one-finger gesture early (a second finger arrived, or the touch was cancelled).
function abandonOne() {
  clearTimeout(T.timer);
  if (T.mode === 'drag') {
    flushMove();
    const f = toFrame(T.last.x, T.last.y, true);
    if (f) sendButton(0, false, f);
  }
}

function onTouchStart(e) {
  if (onUI(e)) return;
  if (resume(e)) return;
  e.preventDefault();
  const n = e.touches.length;
  if (n === 1 && T.mode === 'idle') {
    const t = e.touches[0];
    T.mode = 'pending';
    T.start = { x: t.clientX, y: t.clientY };
    T.last = T.start;
    T.t0 = performance.now();
    T.timer = setTimeout(longPress, 500);
  } else if (n === 2 && (T.mode === 'pending' || T.mode === 'drag' || T.mode === 'pan' || T.mode === 'done' || T.mode === 'idle')) {
    abandonOne();
    const [a, b] = [e.touches[0], e.touches[1]];
    const m = mid(a, b);
    T.mode = 'two';
    T.two = { d0: dist(a, b), m0: m, s0: viewT.s, x0: viewT.x, y0: viewT.y, kind: '', prev: m, ax: 0, ay: 0 };
  } else if (n > 2) {
    if (T.mode !== 'two') abandonOne();
    T.mode = 'ignore';
  }
}

function onTouchMove(e) {
  if (T.mode === 'idle') return;
  e.preventDefault();
  if (T.mode === 'pending' || T.mode === 'drag' || T.mode === 'pan') {
    const t = e.touches[0];
    if (!t) return;
    const p = { x: t.clientX, y: t.clientY };
    if (T.mode === 'pending') {
      if (Math.hypot(p.x - T.start.x, p.y - T.start.y) < TAP_SLOP) return;
      clearTimeout(T.timer);
      const f = (dragMode || viewT.s <= 1.001) && toFrame(T.start.x, T.start.y);
      if (f) {
        sendButton(0, true, f);
        T.mode = 'drag';
      } else {
        T.mode = 'pan';
      }
    }
    if (T.mode === 'drag') {
      const f = toFrame(p.x, p.y, true);
      if (f) queueMove(f);
    } else {
      viewT.x += p.x - T.last.x;
      viewT.y += p.y - T.last.y;
      clampView();
      applyView();
    }
    T.last = p;
  } else if (T.mode === 'two' && e.touches.length >= 2) {
    const [a, b] = [e.touches[0], e.touches[1]];
    const m = mid(a, b);
    const d = dist(a, b);
    const g = T.two;
    if (!g.kind) {
      if (Math.abs(d / g.d0 - 1) > 0.12) g.kind = 'pinch';
      else if (Math.hypot(m.x - g.m0.x, m.y - g.m0.y) > 12) {
        g.kind = 'scroll';
        g.prev = m;
      }
    }
    if (g.kind === 'pinch') {
      // Keep what was under the fingers' first midpoint under their midpoint now.
      const r = stage.getBoundingClientRect();
      const s = clamp((g.s0 * d) / g.d0, 1, 5);
      viewT.x = m.x - r.left - (g.m0.x - r.left - g.x0) * (s / g.s0);
      viewT.y = m.y - r.top - (g.m0.y - r.top - g.y0) * (s / g.s0);
      viewT.s = s;
      clampView();
      applyView();
    } else if (g.kind === 'scroll') {
      // Natural scrolling: fingers moving up scroll the content down (dy > 0).
      const step = 40;
      g.ax += m.x - g.prev.x;
      g.ay += m.y - g.prev.y;
      g.prev = m;
      const dx = Math.trunc(-g.ax / step);
      const dy = Math.trunc(-g.ay / step);
      if (dx || dy) {
        g.ax += dx * step;
        g.ay += dy * step;
        const f = toFrame(m.x, m.y, true);
        if (f) sendWheel(dx, dy, f);
      }
    }
  }
}

function onTouchEnd(e) {
  if (T.mode === 'idle') return;
  e.preventDefault();
  const left = e.touches.length;
  if (T.mode === 'pending') {
    clearTimeout(T.timer);
    // A tap: up before the long press fired, without moving.
    const f = toFrame(T.start.x, T.start.y);
    if (f) {
      click(0, f);
      if (coarse && kbdOpen) kbd.focus({ preventScroll: true }); // keep the keyboard up
    }
    T.mode = left ? 'ignore' : 'idle';
  } else if (T.mode === 'drag') {
    if (left === 0) {
      flushMove();
      const f = toFrame(T.last.x, T.last.y, true);
      if (f) sendButton(0, false, f);
      T.mode = 'idle';
    }
  } else if (T.mode === 'two') {
    if (left < 2) T.mode = left ? 'ignore' : 'idle';
  } else if (left === 0) {
    T.mode = 'idle';
  }
}

function onTouchCancel(e) {
  abandonOne();
  if (e.touches.length === 0) T.mode = 'idle';
  else T.mode = 'ignore';
}

stage.addEventListener('touchstart', onTouchStart, { passive: false });
stage.addEventListener('touchmove', onTouchMove, { passive: false });
stage.addEventListener('touchend', onTouchEnd, { passive: false });
stage.addEventListener('touchcancel', onTouchCancel, { passive: false });
// Safari's own pinch-zoom of the page.
for (const ev of ['gesturestart', 'gesturechange', 'gestureend']) {
  document.addEventListener(ev, (e) => e.preventDefault(), { passive: false });
}

// ---- Mouse and trackpad -------------------------------------------------------------

const MOUSE_BUTTON = { 0: 0, 1: 2, 2: 1 }; // DOM main/aux/secondary -> spec left/middle/right
const mouseDown = new Set();
let wheelX = 0;
let wheelY = 0;

stage.addEventListener('mousemove', (e) => {
  if (onUI(e) && !mouseDown.size) return;
  const f = toFrame(e.clientX, e.clientY, mouseDown.size > 0);
  if (f) queueMove(f);
});
stage.addEventListener('mousedown', (e) => {
  if (onUI(e) || resume(e)) return;
  e.preventDefault(); // no text selection, no dragging the video, no focus change
  const b = MOUSE_BUTTON[e.button];
  const f = toFrame(e.clientX, e.clientY);
  if (b === undefined || !f) return;
  mouseDown.add(b);
  sendButton(b, true, f);
});
window.addEventListener('mouseup', (e) => {
  const b = MOUSE_BUTTON[e.button];
  if (!mouseDown.has(b)) return;
  mouseDown.delete(b);
  const f = toFrame(e.clientX, e.clientY, true);
  if (f) sendButton(b, false, f);
});
stage.addEventListener('contextmenu', (e) => e.preventDefault());
stage.addEventListener(
  'wheel',
  (e) => {
    if (onUI(e)) return; // the window list scrolls itself
    e.preventDefault();
    const f = toFrame(e.clientX, e.clientY);
    if (!f) return;
    // One wheel click per ~100 px, 3 lines, or a tenth of a page.
    const per = e.deltaMode === 1 ? 3 : e.deltaMode === 2 ? 0.1 : 100;
    wheelX += e.deltaX / per;
    wheelY += e.deltaY / per;
    const dx = Math.trunc(wheelX);
    const dy = Math.trunc(wheelY);
    if (dx || dy) {
      wheelX -= dx;
      wheelY -= dy;
      sendWheel(dx, dy, f);
    }
  },
  { passive: false },
);

// ---- Keyboard -----------------------------------------------------------------------

// Sticky modifiers from the key bar: 0 off, 1 for the next key, 2 locked.
const sticky = { ctrl: 0, alt: 0, super: 0 };
const stickyTap = { ctrl: 0, alt: 0, super: 0 };
const pressed = new Set(); // keycodes this page holds down (desktop keyboards)
const consumed = new Set(); // codes whose keyup belongs to a combo already sent
let kbdOpen = false;

function anySticky() {
  return sticky.ctrl || sticky.alt || sticky.super;
}
function tapSticky(name) {
  const now = performance.now();
  if (sticky[name] === 1 && now - stickyTap[name] < 350) sticky[name] = 2;
  else sticky[name] = sticky[name] ? 0 : 1;
  stickyTap[name] = now;
  renderSticky();
}
function consumeSticky() {
  for (const m of Object.keys(sticky)) if (sticky[m] === 1) sticky[m] = 0;
  renderSticky();
}
function renderSticky() {
  for (const b of keysEl.querySelectorAll('[data-mod]')) {
    const s = sticky[b.dataset.mod];
    b.dataset.state = s === 2 ? 'lock' : s === 1 ? 'once' : '';
  }
}

// A key pressed and released with modifiers held around it: those asked for, plus the
// key bar's sticky ones (which a single tap spends).
function combo(code, mods = {}) {
  const held = [];
  for (const m of ['ctrl', 'alt', 'super', 'shift']) {
    if (mods[m] || (m !== 'shift' && sticky[m])) held.push(MODKEY[m]);
  }
  for (const k of held) sendKey(k, true);
  sendKey(code, true);
  sendKey(code, false);
  for (const k of held.reverse()) sendKey(k, false);
  consumeSticky();
}

// Text from the phone's keyboard (or a paste). With a sticky modifier on, a single
// character becomes a shortcut instead.
function typeText(s) {
  if (!s) return;
  if (anySticky() && [...s].length === 1 && US[s]) {
    const [code, shift] = US[s];
    combo(code, { shift });
    return;
  }
  consumeSticky();
  sendText(s);
}

// Some keyboards (and automation) deliver "*" as Digit8 with shiftKey set and no Shift
// keydown of its own; a modifier held before the page had focus looks the same. Before a
// key goes down, the modifiers the event says are held are made to match, and any the
// page pressed only for that reason are let go when the key comes up.
const MODFLAGS = [['shiftKey', 'ShiftLeft', 'ShiftRight'], ['ctrlKey', 'ControlLeft', 'ControlRight'],
  ['altKey', 'AltLeft', 'AltRight'], ['metaKey', 'MetaLeft', 'MetaRight']];
const synthetic = new Set();
function syncModifiers(e) {
  for (const [flag, left, right] of MODFLAGS) {
    const l = KEYCODES[left], r = KEYCODES[right];
    const held = pressed.has(l) || pressed.has(r);
    if (e[flag] && !held) {
      sendKey(l, true);
      pressed.add(l);
      synthetic.add(l);
    } else if (!e[flag] && held) {
      for (const c of [l, r]) if (pressed.delete(c)) { sendKey(c, false); synthetic.delete(c); }
    }
  }
}
function releaseSynthetic() {
  for (const c of synthetic) if (pressed.delete(c)) sendKey(c, false);
  synthetic.clear();
}

function onKey(e) {
  if (e.isComposing || e.keyCode === 229) return; // an input method's; its text comes later
  const code = KEYCODES[e.code];
  const down = e.type === 'keydown';
  if (document.activeElement === kbd) {
    // The phone's (or an iPad's hardware) keyboard: characters come through beforeinput;
    // here only keys that type nothing, and shortcuts.
    if (!down || MODIFIER_CODES.has(e.code) || !code) return;
    if (isPasteKey(e)) return; // the browser pastes into the textarea: beforeinput has it
    const shortcut = e.ctrlKey || e.altKey || e.metaKey;
    if (!shortcut && e.key.length === 1) return;
    e.preventDefault();
    combo(code, { ctrl: e.ctrlKey, alt: e.altKey, super: e.metaKey, shift: e.shiftKey });
    return;
  }
  if (e.target.closest?.('#bar, #keys') && (e.code === 'Enter' || e.code === 'Space')) return;
  if (down && isPasteKey(e)) {
    // ⌘ is let go on the desktop (the remote saw it go down), and V never reaches it.
    for (const k of ['MetaLeft', 'MetaRight']) {
      const c = KEYCODES[k];
      if (pressed.delete(c)) {
        sendKey(c, false);
        synthetic.delete(c);
      }
    }
    pasteHere();
    return; // not prevented: the browser's paste is the point
  }
  if (!code) return;
  e.preventDefault();
  if (down) {
    if (e.repeat) return; // the remote app repeats a held key itself
    if (anySticky() && !MODIFIER_CODES.has(e.code)) {
      combo(code, { shift: e.shiftKey });
      consumed.add(code);
      return;
    }
    if (!MODIFIER_CODES.has(e.code)) syncModifiers(e);
    pressed.add(code);
    sendKey(code, true);
  } else {
    if (consumed.delete(code)) return;
    if (!pressed.delete(code)) return;
    sendKey(code, false);
    if (!MODIFIER_CODES.has(e.code)) releaseSynthetic();
  }
}
// ⌘V pastes the viewer's clipboard; Ctrl+V still goes to the desktop as keys.
// pasteHere moves focus to the hidden textarea before the browser acts on the key, so
// its paste lands there, where beforeinput reads it as it does the phone's: the page
// never asks for clipboard permission. Focus goes back as soon as it has.
const isPasteKey = (e) => e.code === 'KeyV' && e.metaKey && !e.ctrlKey && !e.altKey;
let pasteFocused = false;
let pasteTimer = 0;
function pasteHere() {
  if (document.activeElement === kbd) return;
  pasteFocused = true;
  kbd.focus({ preventScroll: true });
  clearTimeout(pasteTimer);
  pasteTimer = setTimeout(endPaste, 500); // nothing to paste (an image, say): give focus back anyway
}
function endPaste() {
  clearTimeout(pasteTimer);
  if (!pasteFocused) return;
  pasteFocused = false;
  kbd.blur();
}
window.addEventListener('keydown', onKey, true);
window.addEventListener('keyup', onKey, true);

function releaseAll() {
  pressed.clear();
  synthetic.clear();
  consumed.clear();
  mouseDown.clear();
  if (T.mode === 'drag') T.mode = 'idle';
  sendRelease();
}
window.addEventListener('blur', releaseAll);
document.addEventListener('visibilitychange', () => {
  if (document.hidden) releaseAll();
});

// The phone keyboard types into a hidden textarea that is kept empty.
kbd.addEventListener(
  'beforeinput',
  (e) => {
    switch (e.inputType) {
      case 'insertText':
      case 'insertReplacementText':
      case 'insertFromDrop':
        typeText(e.data ?? e.dataTransfer?.getData('text/plain') ?? '');
        break;
      case 'insertFromPaste':
        // A paste (the phone keyboard's, or ⌘V routed here by pasteHere) goes over
        // as the clipboard, not as typing.
        consumeSticky();
        sendPaste(e.dataTransfer?.getData('text/plain') ?? e.data ?? '');
        if (pasteFocused) endPaste();
        break;
      case 'deleteContentBackward':
      case 'deleteWordBackward':
        combo(KEY.backspace);
        break;
      case 'deleteContentForward':
        combo(KEY.delete);
        break;
      case 'insertLineBreak':
      case 'insertParagraph':
        combo(KEY.enter);
        break;
      case 'insertCompositionText':
        return; // not cancelable; the result arrives at compositionend
      default:
        break;
    }
    e.preventDefault();
  },
  true,
);
kbd.addEventListener('compositionend', (e) => {
  typeText(e.data);
  kbd.value = '';
});
kbd.addEventListener('input', () => {
  if (!kbd.matches(':focus') || kbd.value === '') return;
  kbd.value = '';
});
kbd.addEventListener('focus', () => {
  kbdOpen = true;
  renderKeys();
});
kbd.addEventListener('blur', () => {
  kbdOpen = false;
  renderKeys();
});

// ---- Key bar and toolbar ------------------------------------------------------------

let keysPinned = load('keysPinned', false);
function renderKeys() {
  keysEl.hidden = !(keysPinned || (coarse && kbdOpen));
  btn.keys.setAttribute('aria-pressed', String(keysPinned));
  btn.kbd.setAttribute('aria-pressed', String(kbdOpen));
}

// Buttons act on click; pointerdown is cancelled so the keyboard keeps its focus.
for (const el of [keysEl, bar, fitBtn]) {
  el.addEventListener('pointerdown', (e) => e.preventDefault());
  el.addEventListener('mousedown', (e) => e.preventDefault());
}

keysEl.addEventListener('click', (e) => {
  const b = e.target.closest('button');
  if (!b) return;
  if (b.dataset.mod) tapSticky(b.dataset.mod);
  else if (b.dataset.key) combo(Number(b.dataset.key), { [b.dataset.combo || 'none']: true });
  if (kbdOpen) kbd.focus({ preventScroll: true });
});

btn.kbd.addEventListener('click', () => {
  if (kbdOpen) kbd.blur();
  else kbd.focus({ preventScroll: true });
});
// The toolbar's paste, for the phone (whose keyboard has no ⌘V): the browser asks
// before handing over the clipboard (iOS shows its Paste bubble).
btn.paste.hidden = !navigator.clipboard?.readText;
btn.paste.addEventListener('click', () => {
  navigator.clipboard.readText().then(
    (t) => (t ? sendPaste(t) : notice('the clipboard has no text')),
    (err) => notice(`could not read the clipboard: ${err.message}`),
  );
});
btn.fit.addEventListener('click', resetView);
fitBtn.addEventListener('click', resetView);
fitBtn.addEventListener('touchstart', (e) => e.stopPropagation(), { passive: true });
function renderDrag() {
  btn.drag.setAttribute('aria-pressed', String(dragMode));
}
btn.drag.addEventListener('click', () => {
  dragMode = !dragMode;
  save('dragMode', dragMode);
  renderDrag();
});
btn.keys.addEventListener('click', () => {
  keysPinned = !keysPinned;
  save('keysPinned', keysPinned);
  renderKeys();
});
let showStats = load('stats', false);
function renderStats() {
  statsEl.hidden = !showStats;
  btn.stats.setAttribute('aria-pressed', String(showStats));
}
btn.stats.addEventListener('click', () => {
  showStats = !showStats;
  save('stats', showStats);
  renderStats();
});
let collapsed = load('collapsed', false);
function renderBar() {
  bar.classList.toggle('collapsed', collapsed);
  btn.more.setAttribute('aria-expanded', String(!collapsed));
}
btn.more.addEventListener('click', () => {
  collapsed = !collapsed;
  save('collapsed', collapsed);
  renderBar();
});

// ---- Desktops -----------------------------------------------------------------------
//
// Two desktops on one sway: the agents', fixed in size, to watch them and
// help; and the owner's own, sized to this screen. Asked on opening (or given as
// ?desk=agent or ?desk=own); a reconnect keeps it, ⇄ switches, and ▣ moves windows
// between them. Either way, this page's input is on the owner's own seat, so it never
// moves the agents' focus.
const DESKS = { agent: "the agents'", own: 'mine' };
const deskParam = new URLSearchParams(location.search).get('desk');
let desk = deskParam in DESKS ? deskParam : '';
let screens = ['agent'];
let asked = null; // the size the owner's desktop was fitted to: [w, h, dpr]

function choose() {
  chooser.hidden = false;
  say('');
  const last = load('desk', '');
  for (const b of chooser.querySelectorAll('button')) b.classList.toggle('last', b.dataset.desk === last);
}
chooser.addEventListener('click', (e) => {
  const b = e.target.closest('button');
  if (!b) return;
  desk = b.dataset.desk;
  save('desk', desk);
  chooser.hidden = true;
  backoff = 500;
  connect();
});

function renderDesk() {
  const both = screens.length > 1;
  btn.desk.hidden = btn.wins.hidden = btn.apps.hidden = !both;
  renderZoom();
  btn.desk.title = both ? `switch to ${desk === 'own' ? DESKS.agent : DESKS.own} desktop` : '';
  if (!both) {
    showWins(false);
    showApps(false);
  }
}
btn.desk.addEventListener('click', () => {
  desk = desk === 'own' ? 'agent' : 'own';
  save('desk', desk);
  backoff = 500;
  connect();
});

// Zoom on the owner's desktop: it is always drawn in this screen's own
// pixels; the zoom is how big apps are in them. At the screen's pixel ratio (the
// most) they are this screen's size; lower gives them more room, smaller. Kept per
// device, and applied by connecting again.
let zoom = 0; // as the server last gave it
let maxZoom = 0;
const zoomSteps = (max) => [...new Set([max, 2, 1.5, 1].filter((z) => z <= max))].sort((a, b) => b - a);
function renderZoom() {
  btn.zoom.hidden = desk !== 'own' || !(maxZoom > 1);
  btn.zoom.textContent = `${zoom || maxZoom}×`;
}
btn.zoom.addEventListener('click', () => {
  const steps = zoomSteps(maxZoom);
  const next = steps[(steps.indexOf(zoom) + 1) % steps.length] ?? maxZoom;
  save('zoom', next);
  backoff = 500;
  connect();
});

// The owner's desktop is fitted to the stage when connecting; when the stage changes
// size (the phone turned, the window resized), connect again to fit it anew. Not while
// a phone's keyboard is up: it shrinks the stage only for as long as it is open.
function stageSize() {
  return [Math.round(stage.clientWidth), Math.round(stage.clientHeight), window.devicePixelRatio || 1];
}
let refitTimer = 0;
new ResizeObserver(() => {
  if (desk !== 'own' || !asked || (coarse && kbdOpen)) return;
  clearTimeout(refitTimer);
  refitTimer = setTimeout(() => {
    const [w, h, dpr] = stageSize();
    if (Math.abs(w - asked[0]) > 8 || Math.abs(h - asked[1]) > 8 || dpr !== asked[2]) connect();
  }, 700);
}).observe(stage);

function showWins(open) {
  if (open) showApps(false);
  winsEl.hidden = !open;
  btn.wins.setAttribute('aria-pressed', String(open));
  if (open && ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: 'windows' }));
}
btn.wins.addEventListener('click', () => showWins(winsEl.hidden));
function renderWins(list) {
  winsEl.replaceChildren();
  const other = desk === 'own' ? 'agent' : 'own';
  for (const d of [desk, other]) {
    const h = document.createElement('h3');
    h.textContent = d === desk ? `On this desktop (${DESKS[d]})` : `On ${DESKS[d]}`;
    winsEl.append(h);
    const here = list.filter((w) => w.screen === d);
    if (!here.length) {
      const p = document.createElement('p');
      p.textContent = 'No windows.';
      winsEl.append(p);
    }
    for (const w of here) {
      const row = document.createElement('div');
      row.className = 'win';
      const name = document.createElement('span');
      name.textContent = w.title || w.app || `window ${w.id}`;
      name.title = `${w.app} · ${w.title}`;
      const b = document.createElement('button');
      b.type = 'button';
      b.textContent = d === desk ? (other === 'own' ? 'to mine' : "to the agents'") : 'bring here';
      b.addEventListener('click', () => ws?.send(JSON.stringify({ type: 'move', id: w.id, to: d === desk ? other : desk })));
      row.append(name, b);
      winsEl.append(row);
    }
  }
}
// The apps installed on the machine, each opened on the owner's desktop.
function showApps(open) {
  if (open) showWins(false);
  appsEl.hidden = !open;
  btn.apps.setAttribute('aria-pressed', String(open));
  if (open && ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: 'apps' }));
}
btn.apps.addEventListener('click', () => showApps(appsEl.hidden));
function renderApps(list) {
  appsEl.replaceChildren();
  const h = document.createElement('h3');
  h.textContent = 'Open on my desktop';
  appsEl.append(h);
  if (!list.length) {
    const p = document.createElement('p');
    p.textContent = 'No apps found.';
    appsEl.append(p);
  }
  for (const a of list) {
    const row = document.createElement('div');
    row.className = 'win';
    const name = document.createElement('span');
    name.textContent = a.name;
    name.title = a.id;
    const b = document.createElement('button');
    b.type = 'button';
    b.textContent = 'open';
    b.addEventListener('click', () => {
      ws?.send(JSON.stringify({ type: 'launch', app: a.id }));
      showApps(false);
      notice(`starting ${a.name}…`);
    });
    row.append(name, b);
    appsEl.append(row);
  }
}
for (const el of [chooser, winsEl, appsEl]) el.addEventListener('mousedown', (e) => e.target.closest('button') && e.preventDefault());

// ---- Connection ---------------------------------------------------------------------

let ws = null;
let pc = null;
let session = 0; // bumped per attempt; callbacks from an older one are ignored
let backoff = 500;
let retryTimer = 0;
let codec = '';
let firstFrame = false;
let errorText = '';

function setDot(state) {
  dot.dataset.state = state;
}
function say(text, isError = false) {
  overlay.textContent = text;
  overlay.classList.toggle('error', isError);
  overlay.hidden = !text;
}
// notice says something for a few seconds, unless something else is said first.
let noticeTimer = 0;
function notice(text) {
  say(text);
  clearTimeout(noticeTimer);
  noticeTimer = setTimeout(() => overlay.textContent === text && say(''), 4000);
}

function teardown() {
  if (dc && dc.readyState === 'open') sendRelease();
  if (dc) dc.onopen = dc.onclose = null;
  if (pc) {
    pc.ontrack = pc.onconnectionstatechange = null;
    pc.close();
  }
  if (ws) {
    ws.onopen = ws.onmessage = ws.onclose = ws.onerror = null;
    if (ws.readyState <= WebSocket.OPEN) ws.close();
  }
  dc = pc = ws = null;
}

function retry() {
  teardown();
  setDot(errorText ? 'bad' : 'connecting');
  say(errorText ? `${errorText} — retrying…` : 'reconnecting…', !!errorText);
  clearTimeout(retryTimer);
  retryTimer = setTimeout(connect, backoff);
  backoff = Math.min(backoff * 2, 5000);
}

function connect() {
  clearTimeout(retryTimer);
  teardown();
  const id = ++session;
  firstFrame = false;
  setDot('connecting');
  if (!errorText) say(id === 1 ? 'connecting…' : 'reconnecting…');
  const sock = new WebSocket(`${location.origin.replace(/^http/, 'ws')}/ws`);
  ws = sock;
  sock.onopen = () => {
    if (id === session) startPeer(id).catch((err) => fail(id, `could not start: ${err.message || err}`));
  };
  sock.onmessage = (e) => {
    if (id === session) onSignal(id, e.data);
  };
  sock.onclose = (e) => {
    if (id !== session) return;
    if (e.code === CLOSE_REPLACED) takenOver();
    else retry();
  };
}

// One viewer at a time: when another page takes the session, the server closes this
// one's socket with CLOSE_REPLACED. Retrying would take it back, and the two pages would
// trade it forever, so wait for a tap instead.
const CLOSE_REPLACED = 4000;
let replaced = false;
function takenOver() {
  teardown();
  clearTimeout(retryTimer);
  replaced = true;
  setDot('bad');
  say('another viewer took over — tap to reconnect');
}
function resume(e) {
  if (!replaced) return false;
  e.preventDefault();
  replaced = false;
  backoff = 500;
  connect();
  return true;
}

function fail(id, text) {
  if (id !== session) return;
  errorText = text;
  retry();
}

// AV1 first, then H.264 High, then H.264 Constrained Baseline; nothing else.
function preferCodecs(tr) {
  if (!tr.setCodecPreferences || !window.RTCRtpReceiver?.getCapabilities) return;
  const caps = RTCRtpReceiver.getCapabilities('video')?.codecs || [];
  const fmtp = (c) => (c.sdpFmtpLine || '').toLowerCase();
  const mime = (c) => c.mimeType.toLowerCase();
  const plid = (c) => (fmtp(c).match(/profile-level-id=([0-9a-f]{6})/) || [])[1] || '';
  const h264 = caps.filter((c) => mime(c) === 'video/h264' && /packetization-mode=1/.test(fmtp(c)));
  const order = [
    ...caps.filter((c) => mime(c) === 'video/av1'),
    ...h264.filter((c) => plid(c).startsWith('640c')),
    ...h264.filter((c) => plid(c).startsWith('42e0')),
  ];
  if (!order.length) return;
  const rtx = caps.filter((c) => mime(c) === 'video/rtx');
  for (const list of [[...order, ...rtx], order]) {
    try {
      tr.setCodecPreferences(list);
      return;
    } catch {
      /* some browsers refuse rtx here; try without */
    }
  }
}

function lowLatency(receiver) {
  try {
    receiver.jitterBufferTarget = 0;
  } catch {
    /* not supported */
  }
  try {
    receiver.playoutDelayHint = 0;
  } catch {
    /* not supported */
  }
}

async function startPeer(id) {
  const peer = new RTCPeerConnection({ iceServers: [], bundlePolicy: 'max-bundle', rtcpMuxPolicy: 'require' });
  pc = peer;
  const tr = peer.addTransceiver('video', { direction: 'recvonly' });
  preferCodecs(tr);
  lowLatency(tr.receiver);
  const ch = peer.createDataChannel('input', { ordered: true });
  ch.binaryType = 'arraybuffer';
  dc = ch;
  peer.ontrack = (e) => {
    if (id !== session) return;
    lowLatency(e.receiver);
    video.srcObject = e.streams[0] || new MediaStream([e.track]);
    video.play().catch(() => {});
  };
  let downTimer = 0;
  peer.onconnectionstatechange = () => {
    if (id !== session) return;
    const st = peer.connectionState;
    clearTimeout(downTimer);
    if (st === 'connected') {
      setDot('ok');
      connectedAt = performance.now();
    } else if (st === 'failed' || st === 'closed') {
      retry();
    } else if (st === 'disconnected') {
      setDot('connecting');
      downTimer = setTimeout(() => id === session && peer.connectionState === 'disconnected' && retry(), 4000);
    }
  };
  const offer = await peer.createOffer();
  await peer.setLocalDescription(offer);
  if (id !== session) return;
  const offer2 = { type: 'offer', sdp: peer.localDescription.sdp, screen: desk || 'agent' };
  if (desk === 'own') {
    asked = stageSize();
    [offer2.width, offer2.height, offer2.dpr] = asked;
    offer2.zoom = load('zoom', 0);
  }
  ws.send(JSON.stringify(offer2));
}

async function onSignal(id, data) {
  let m;
  try {
    m = JSON.parse(data);
  } catch {
    return;
  }
  if (m.type === 'answer') {
    codec = m.codec || '';
    W = m.width | 0;
    H = m.height | 0;
    screens = m.screens || ['agent'];
    zoom = m.zoom || 0;
    maxZoom = m.maxZoom || 0;
    if (desk === 'own' && m.screen !== 'own') {
      notice("your desktop isn't set up on this sway yet: showing the agents'");
    }
    desk = m.screen || 'agent';
    renderDesk();
    if (desk === 'own') resetView(); // fitted to this screen: no zoom left over
    layout();
    try {
      await pc.setRemoteDescription({ type: 'answer', sdp: m.sdp });
    } catch (err) {
      fail(id, `bad answer: ${err.message || err}`);
    }
  } else if (m.type === 'windows') {
    renderWins(m.windows || []);
  } else if (m.type === 'apps') {
    renderApps(m.apps || []);
  } else if (m.type === 'notice') {
    notice(m.message || '');
  } else if (m.type === 'error') {
    errorText = m.message || 'error';
    say(errorText, true);
    setDot('bad');
  }
}

function requestKeyframe() {
  if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: 'keyframe' }));
}

// ---- Frames, stats, and keyframes when the picture is stuck --------------------------
//
// An idle desktop sends no frames at all, so silence is not a stall. A keyframe is asked
// for only when nothing has been decoded since connecting, or bytes keep arriving while
// no frame decodes.

let frames = 0;
let connectedAt = 0;
function onFrame() {
  frames++;
  if (!firstFrame) {
    firstFrame = true;
    errorText = '';
    backoff = 500;
    say('');
  }
}
if ('requestVideoFrameCallback' in HTMLVideoElement.prototype) {
  const tick = () => {
    onFrame();
    video.requestVideoFrameCallback(tick);
  };
  video.requestVideoFrameCallback(tick);
} else {
  video.addEventListener('playing', onFrame);
}

const last = { t: 0, bytes: 0, decoded: 0, frames: 0, stuck: 0, keyAt: 0 };
async function poll() {
  const peer = pc;
  if (!peer || peer.connectionState !== 'connected') return;
  let inbound = null;
  let pair = null;
  let pairId = '';
  const stats = await peer.getStats();
  stats.forEach((s) => {
    if (s.type === 'inbound-rtp' && s.kind === 'video') inbound = s;
    if (s.type === 'transport' && s.selectedCandidatePairId) pairId = s.selectedCandidatePairId;
  });
  stats.forEach((s) => {
    if (s.type === 'candidate-pair' && (s.id === pairId || (!pairId && s.nominated && s.state === 'succeeded'))) pair = s;
  });
  const now = performance.now();
  const dt = last.t ? (now - last.t) / 1000 : 0;
  const bytes = inbound?.bytesReceived ?? 0;
  const decoded = inbound?.framesDecoded ?? 0;
  const kbps = dt ? Math.round(((bytes - last.bytes) * 8) / 1000 / dt) : 0;
  const fps = dt ? Math.round((frames - last.frames) / dt) : 0;

  // stuck: bytes arrived but nothing decoded, two polls running
  last.stuck = bytes > last.bytes && decoded === last.decoded ? last.stuck + 1 : 0;
  const neverDecoded = decoded === 0 && connectedAt && now - connectedAt > 2000;
  if ((neverDecoded || last.stuck >= 2) && now - last.keyAt > 2000) {
    requestKeyframe();
    last.keyAt = now;
  }
  Object.assign(last, { t: now, bytes, decoded, frames });

  if (showStats) {
    const rtt = pair?.currentRoundTripTime != null ? `${Math.round(pair.currentRoundTripTime * 1000)} ms` : '–';
    const dec = inbound?.decoderImplementation || '–';
    const hw = inbound?.powerEfficientDecoder === true ? ' (hw)' : inbound?.powerEfficientDecoder === false ? ' (sw)' : '';
    statsEl.textContent = [
      `${codec || '–'}  ${W || '?'}×${H || '?'}`,
      `${fps} fps  ${kbps} kbit/s`,
      `rtt ${rtt}`,
      `decoder ${dec}${hw}`,
      `lost ${inbound?.packetsLost ?? 0}  zoom ${viewT.s.toFixed(2)}x`,
    ].join('\n');
  }
}
setInterval(() => poll().catch(() => {}), 1000);

// ---- Start ------------------------------------------------------------------------------

renderKeys();
renderDrag();
renderStats();
renderBar();
layout();
if (desk) connect();
else choose();
