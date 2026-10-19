# swampcy web, the desktop in a browser

How `swampcy web` behaves. The code is `main.go` (`runWeb`), `internal/web/`,
`internal/codec/`, and the screencopy, keyboard and clipboard parts of `internal/desktop/`.
The MCP server is specified in [spec.md](spec.md), and the reasons behind both are in
[design.md](design.md). The key words MUST, MUST NOT, SHOULD and MAY are used as in RFC 2119.

## Overview

`swampcy web` serves swampcy's headless desktop to a browser on a Tailscale network. The
page and its signalling WebSocket come through `tailscale serve`; the picture comes as a
WebRTC video track, AV1 where the browser can decode it in hardware and H.264 otherwise;
taps, clicks and keys go back on a WebRTC data channel to swampcy's virtual pointer and
keyboard.

`swampcy web` shows two desktops. The agents' is the one the MCP servers see. The owner's is
for the person using the machine, and is sized to the device they view it from
([design.md](design.md#two-desktops-on-two-seats)).

## Requirements

### Listening and who gets in

- `swampcy web` MUST refuse to start unless `-listen` (default `127.0.0.1:7682`) is a
  loopback IP literal with a port, and unless `-login` and `-origin` are both set.
- Every HTTP request MUST be refused with 403 unless its `Tailscale-User-Login` header
  equals `-login` exactly, and a WebSocket upgrade MUST be refused with 403 unless its
  `Origin` equals `-origin` exactly. Refusals MUST be logged with the login presented, the
  path and the peer.
- Every response MUST carry a Content-Security-Policy allowing scripts, styles and
  connections only from the page's own origin, and `frame-ancestors 'none'`.
- The WebRTC media and data MUST use one UDP port (`-udp`, default `:7683`) bound to a
  single Tailscale IPv4 address (in 100.64.0.0/10): the machine's own when `-udp` gives no
  host, and the one given otherwise. A host outside that range, or a machine with no
  Tailscale IPv4 address, MUST stop the server at start. The server MUST offer only host
  candidates on that address, MUST NOT offer loopback candidates, and MUST NOT use STUN or
  TURN.
- These checks rest on `tailscale serve` setting `Tailscale-User-Login`. Anything that can
  reach the loopback port can send the header itself, so `swampcy web` assumes a
  single-user machine ([design.md](design.md#known-limits)).

### Signalling

The page opens `wss://<origin>/ws`. Messages are JSON text, one object each, with `type`:

| From | `type` | Fields | Meaning |
|---|---|---|---|
| page | `offer` | `sdp`, `screen`, and with `own`: `width`, `height`, `dpr`, `zoom` | An SDP offer with one receive-only video transceiver and a data channel labelled `input`. Candidates MAY be absent. `screen` is the desktop asked for, `agent` or `own`; for `own`, the page's size in CSS pixels, its pixel ratio, and the zoom wanted (0 for the most). |
| server | `answer` | `sdp`, `codec`, `width`, `height`, `screen`, `screens`, and for `own`: `zoom`, `maxZoom` | The answer, with all of the server's candidates in it (no trickle); the codec chosen (`AV1` or `H264`); the screen's size in device pixels; the desktop given; the desktops there are; and the owner's desktop's zoom and the most it could be. |
| server | `error` | `message` | The session cannot start; the socket closes after it. |
| page | `keyframe` | | Asks for a keyframe now (also available as RTCP PLI). |
| page | `windows` | | Asks for the windows. |
| server | `windows` | `windows`: each `id`, `title`, `app`, `screen` | Every window, and the desktop it is on (`agent`, `own`, or another output's name). |
| page | `move` | `id`, `to` | Moves a window to a desktop. The server answers with `windows`, after a `notice` if it could not. |
| server | `notice` | `message` | Something the page SHOULD show for a moment. |
| page | `apps` | | Asks for the apps. |
| server | `apps` | `apps`: each `id`, `name` | The apps installed, sorted by name. |
| page | `launch` | `app` | Opens an app, by its desktop file ID, on the owner's desktop. |

- With `-codec auto` (the default) the server MUST choose AV1 when the offer lists
  `video/AV1`, else H.264, preferring High (profile-level-id `640c1f`, `64001f`) over
  Constrained Baseline (`42e01f`, `42001f`). `-codec av1` MUST allow only AV1 and
  `-codec h264` only H.264. When the offer lists nothing allowed, the server MUST answer
  with an `error`.
- One viewer at a time: a new session MUST end the previous one.
- The page SHOULD order its codec preferences AV1, H.264 High, H.264 Constrained Baseline
  with `setCodecPreferences`, leaving out the rest.

### Desktops ([design](design.md#two-desktops-on-two-seats))

- The agents' desktop MUST be `-agent-output`, or without it the output at the layout's top
  left. The owner's MUST be `-own-output` when that names an active output other than the
  agents', and there MUST be none otherwise. A missing output MUST be logged, not fatal.
- An offer for a desktop there is not MUST get the agents', and the answer MUST say so.
- Before streaming the owner's desktop, the server MUST size its output to the page:
  `output <name> mode <w>x<h> scale <s>`. The mode MUST be the page's CSS size at its pixel
  ratio to the nearest quarter, at least 1 and at most 3, lowered by quarters while the mode
  would exceed 2560×1600 pixels (that ratio is the most zoom), rounded to even numbers. The
  scale MUST be the offer's `zoom` to the nearest quarter, between 1 and the most zoom, or
  the most zoom when the offer has none ([design](design.md#zoom)). A page size outside
  200 to 4096 CSS pixels a side MUST be logged and the output streamed as it is. The agents' output MUST NOT be resized.
- `move` MUST move the window with `[con_id=<id>] move container to output <name>`, and in
  the same command keep seat0's focus with the agents: when seat0 had another window focused
  on the agents' output (or anywhere, for a move to the agents), focus that one; otherwise,
  for a move away from the agents, `focus output <agents' output>`.
- The page MUST ask which desktop when it opens, unless its URL carries `?desk=agent` or
  `?desk=own`, and MUST keep the desktop across reconnects. When the server offers both, it
  MUST show a control that switches desktop (a new session) and one that lists the windows
  with a button moving each to the other desktop.
- On the owner's desktop, the page MUST offer a zoom control that steps through the most
  zoom, 2, 1.5 and 1 (those not above the most), keeps the choice in the browser's storage,
  and applies it by connecting again.
- On the owner's desktop, the page MUST connect again when its stage's size or pixel ratio
  changes by more than 8 CSS pixels a side, so that the output is sized anew, but MUST NOT
  while a phone's keyboard is open.

### Apps ([design](design.md#opening-apps-on-your-desktop))

- `apps` MUST list the desktop entries in `$XDG_DATA_HOME/applications` (default
  `~/.local/share/applications`) and then each `$XDG_DATA_DIRS` directory's `applications`
  (default `/usr/local/share` and `/usr/share`), whose `[Desktop Entry]` group has `Type`
  Application, a `Name` and an `Exec`, and is not `Hidden`, `NoDisplay` or `Terminal`. An ID
  found in an earlier directory MUST hide the same ID in a later one.
- `launch` MUST start only an ID that a fresh scan lists and that is 1 to 120 letters, digits,
  `.`, `_`, `+` and `-` followed by `.desktop`, and MUST answer any other with a `notice`.
  Nothing else the page sends MAY go into a command.
- It MUST start the app through sway, `exec gtk-launch <id>`, so that the app gets sway's
  environment (its X display among it). For three minutes it MUST then move to the owner's
  desktop, as `move` does, every window that was not there before and whose app ID or X11
  class is, ignoring case, the entry's `StartupWMClass` or its ID without `.desktop`; and
  when none has after 45 seconds, the first new window. It MUST send a `notice` for the
  first window moved, and one when none came.
- The page MUST show its apps control only when the owner's desktop exists, and MUST say
  that an app is starting when one is chosen.

### Input (data channel `input`)

The page MUST treat a touch as a tap (a left click where it began) when it ends within
500 ms, before the long press (a right click) fires, having drifted less than 16 CSS pixels;
farther, it is a drag, or a pan of a zoomed-in view.

The channel is ordered and reliable. Each message is binary; the first byte is the kind;
integers are little-endian; coordinates are pixels of the video frame (`0..width-1`,
`0..height-1`).

| Kind | Bytes after it | Meaning |
|---|---|---|
| `0x01` move | `u16 x, u16 y` | Move the pointer. |
| `0x02` button | `u8 button` (0 left, 1 right, 2 middle), `u8 down` (1 press, 0 release), `u16 x, u16 y` | Move there, then press or release. |
| `0x03` wheel | `i8 dx, i8 dy`, `u16 x, u16 y` | Move there, then scroll by wheel clicks: `dy` > 0 down, `dx` > 0 right. |
| `0x04` key | `u16 code` (Linux evdev keycode), `u8 down` | Press or release a key on the US keymap. |
| `0x05` text | UTF-8 bytes | Type text (input-method and paste output), on the session's own virtual keyboard, as swampcy's `type` does ([design](design.md#typing-on-its-own-keyboard)). |
| `0x06` release | (nothing) | Release every key and button the session holds (blur, disconnect). |
| `0x07` paste | UTF-8 bytes | Paste the viewer's clipboard text: make it the selection of the seat the keys go to, then press Ctrl+V there ([design](design.md#pasting-the-viewers-clipboard)). |

- On `0x07`, the server MUST set the selection of the seat a key would go to at that moment
  (the first seat for a single-seat app's window, the owner's seat otherwise) through
  `zwlr_data_control_manager_v1`, offering the text as `text/plain;charset=utf-8`,
  `text/plain`, `UTF8_STRING`, `STRING` and `TEXT`, and write it whole to each pipe an app
  sends to read it. Only once the compositor has the selection MUST it press and release
  Ctrl, V on that seat's keyboard. When the selection cannot be set it MUST press nothing.
  It MUST keep one data-control connection per seat for its life, reopening it when the
  session went away, so that a selection outlives the viewer's session.
- The page MUST send `0x07`, not `0x05`, for a paste: ⌘V (with neither Ctrl nor Alt),
  which it MUST NOT send to the desktop as keys, letting the browser paste into its hidden
  textarea instead; the textarea's `insertFromPaste`; and its paste button, which reads
  `navigator.clipboard`. It MUST release ⌘ on the desktop if it sent it down. A paste larger
  than the data channel's maximum message size MUST be refused with a notice, not split.
- A session's virtual pointer and keyboard MUST join the seat `-seat` names, on either
  desktop, so that a viewer never moves the agents' focus; when sway has no such seat, the
  server MUST log it and use the first. The pointer MUST be created on the session's output,
  and coordinates MUST be that output's device pixels, the frame's.
- When `-seat` names a seat other than `seat0` and `-single-seat-apps` is not empty, input
  to a window whose app ID or X11 class matches `-single-seat-apps` (default
  `(?i)^(google-chrome|chrome|chromium|brave|microsoft-edge)`) MUST go through a second
  virtual pointer and keyboard on the first seat, on the session's output, since such an app
  binds no other seat ([design](design.md#single-seat-apps-through-seat0)):
  - A move, a press or a wheel MUST go through the first seat when the window whose content
    is under the point, on the session's output, is such an app's, read from sway's tree at
    most 250 ms old. While seat0's focus is lent (below), so MUST one over no window.
    Everything else MUST go through the owner's seat.
  - A button's release, and a key's, MUST go through the seat its press went through.
  - Keys and text MUST go through the first seat when the viewer's last press went through
    it onto a window that still exists, and through the owner's seat otherwise.
  - Before keys go through the first seat, seat0's focus MUST be on their window, by
    `[con_id=<id>] focus` when it is not.
  - The first time a press or keys go through the first seat, the server MUST remember the
    container seat0's focus was on (its `get_seats` focus). After 1.5 s with no input
    through the first seat and no key or button held there, it MUST give seat0's focus back:
    to that container when it is still a window, and otherwise `focus output` the agents'
    output. It MUST NOT do so when seat0's focus is no longer on the window it lent it to.
    A session's end MUST give focus back at once.
- A press MUST come about 20 ms after the move before it, as swampcy's `click`'s does.
- `-single-seat-apps` MUST be a Go regular expression; one that does not compile MUST stop
  the server at start. An empty one MUST route nothing.
- The server MUST ignore a malformed message, an unknown kind, a coordinate outside the
  frame, and a keycode above 767, and MUST NOT end the session for one.
- When the session ends, the server MUST release every key and button it pressed.
- Modifier keys (Shift, Ctrl, Alt, Super, AltGr) MUST change the virtual keyboard's
  modifier state as well as send the key, so that apps see `ctrl+c` as a shortcut.

### The picture

- The server MUST capture the session's output, bound by name, through screencopy's
  `copy_with_damage`, so that nothing is
  captured or encoded while the screen does not change, and MUST convert only the damaged
  rectangles (widened to even coordinates) into the encoder's I420 frame, full-range BT.709.
- It MUST encode at most `-fps` frames a second (default 60), always the latest picture,
  and MUST mark the stream's colour as full-range BT.709.
- AV1 MUST be libaom in real-time mode: speed 10, `tune-content=screen`, palette and intra
  block copy on, no lookahead, constant quality `-quality` (default 24) under a bitrate cap.
- H.264 MUST be OpenH264 in screen-content real-time mode, bitrate rate control, no frame
  skipping, CABAC and High profile when the negotiated profile is High and `-h264-profile`
  is `high` (the default); with `-h264-profile baseline` it MUST encode Constrained Baseline
  whatever was negotiated.
- Each encoder MUST use `-threads` threads (default 4).
- The encoders MUST NOT make periodic keyframes. A keyframe MUST follow an RTCP PLI or FIR, a
  `keyframe` message, and the start of a session; when the screen is idle, the current
  picture MUST be re-encoded as a keyframe rather than waiting for a change.
- The bitrate cap MUST start at `-start-kbps` (default 2500) and follow the send-side
  bandwidth estimate (transport-wide congestion control), within `-max-kbps` (default
  20000), changing the encoder only when the estimate moves by more than 10%.
- Each encoder MUST check, before its first frame, that the loaded library's defaults read
  back at the offsets it uses, and MUST refuse to encode when they do not.

## Running it

This section is guidance, not requirements. [`examples/`](../examples/) has a working set.

- Run `swampcy web` as a user unit, such as
  [`examples/swampcy-web.service`](../examples/swampcy-web.service), after the sway unit:
  `swampcy web -login you@example.com -origin https://box.example.ts.net:8443
  -own-output HEADLESS-2 -seat owner`. Without `-own-output` there is only the agents'
  desktop; without `-seat` viewers share the first seat.
- Put it behind `tailscale serve` on a port of its own, for example
  `tailscale serve --bg --https=8443 http://127.0.0.1:7682`, and set `-origin` to exactly
  the URL the browser will show, port included.
- `-login` is your Tailscale login as `tailscale serve` reports it. Every other login gets a
  403, and the refusal is logged.
- The browser must reach the machine's Tailscale address on the UDP port (7683 by default).
  Tailnet access rules that allow only the HTTPS port leave the page connected with no
  picture.
- libaom (for AV1) and OpenH264 (for H.264) are loaded at run time. Install at least one;
  read the log after upgrading either, since an encoder whose library layout changed
  refuses to encode.
- A long-running `swampcy web` keeps the binary it started with. Restart its unit after
  installing a new one; the page reconnects. Do not restart sway for it.
