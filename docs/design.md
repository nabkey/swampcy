# Design

Why swampcy is built the way it is. [spec.md](spec.md) says how the MCP server behaves and
[web.md](web.md) how `swampcy web` does; this file says why, what else was tried, and where
the design has known limits.

## Contents

- [What it is for](#what-it-is-for)
- [Headless sway, on Wayland](#headless-sway-on-wayland)
- [Attaching to a session it does not own](#attaching-to-a-session-it-does-not-own)
- [Its own virtual pointer and keyboard](#its-own-virtual-pointer-and-keyboard)
- [Typing on its own keyboard](#typing-on-its-own-keyboard)
- [A named seat and a named output](#a-named-seat-and-a-named-output)
- [Coordinates and screenshots](#coordinates-and-screenshots)
- [Accessibility through AT-SPI](#accessibility-through-at-spi)
- [Dependencies](#dependencies)
- [Two desktops on two seats](#two-desktops-on-two-seats)
- [The web viewer](#the-web-viewer)
- [Opening apps on your desktop](#opening-apps-on-your-desktop)
- [Zoom](#zoom)
- [Single-seat apps through seat0](#single-seat-apps-through-seat0)
- [Pasting the viewer's clipboard](#pasting-the-viewers-clipboard)
- [Replays](#replays)
- [Signing a browser in](#signing-a-browser-in)
- [Known limits](#known-limits)

## What it is for

A coding agent on a Linux machine, reached over SSH, should be able to open a GUI app, look
at it, and click and type in it. When swampcy was started, in October 2026, nothing did that
well:

- **Claude Code's own computer use was macOS only.** Its docs said computer use in the CLI
  was not available on Linux or Windows. The API's computer-use tool works on any OS, but it
  is a tool for an agent loop of your own, billed to an API key, not to a subscription a CLI
  agent runs on.
- **An MCP server's images reach the model.** Claude Code shows the model an image a tool
  returns, scaled to at most 2000 pixels a side. A third-party MCP server is therefore a
  complete route.
- **The mature third-party servers were X11.** The best drove an `Xvnc` display. On Wayland,
  a server that starts an isolated KWin session found that KWin's virtual backend never
  answers a screenshot request without a GPU, and there was no way to watch such a session
  live. The GNOME servers need a logged-in Shell. Nothing maintained drove a headless
  wlroots compositor.

So swampcy is an MCP server over stdio, started by the agent, that drives a headless sway
nobody sits at.

## Headless sway, on Wayland

A headless sway needs no GPU and no monitor. With `WLR_BACKENDS=headless` and
`WLR_RENDERER=pixman` it renders in software. wlroots gives a client everything swampcy
needs as protocols: screencopy for screenshots (through grim, or swampcy's own client), a
virtual pointer, and a virtual keyboard. AT-SPI, the accessibility bus, does not depend on
the compositor at all. Every piece is an ordinary distribution package.

Alternatives considered:

- **X11 (Xvnc or Xvfb) with an existing server.** Mature, but X11. Any X client can read
  every other client's input and windows, and the GUI stack is moving to Wayland; Chrome,
  GTK 4 and Qt all run natively there.
- **KWin's virtual session.** Would not take a screenshot without a GPU.
- **A GNOME Shell session.** Needs a logged-in user session and a heavier stack.

sway is the only compositor swampcy supports. Its IPC (`swaymsg`, the tree, `get_seats`),
its per-seat configuration and its headless outputs are all used directly.

## Attaching to a session it does not own

swampcy does not start sway. A separate user unit runs it (see
[`examples/swampcy-sway.service`](../examples/swampcy-sway.service)), and each MCP server
finds it from `SWAYSOCK` and `WAYLAND_DISPLAY`, or from the sockets in `XDG_RUNTIME_DIR`. It
refuses to guess between two sessions. It looks again whenever the session it knew stops
answering, so sway may start after swampcy or restart under it.

The reasons:

- Each agent session starts its own swampcy, and several may run at once. They share one
  desktop rather than each owning one.
- Apps outlive an agent session. A window opened by one session is still there for the next.
- A new swampcy binary takes effect at the next agent session without restarting the
  desktop. Restarting sway closes every app on it.

## Its own virtual pointer and keyboard

A headless sway has no input devices. Its seat offers apps neither a pointer nor a keyboard,
and nothing sent to it reaches them; sway's own `seat cursor` commands included, which is how
this was found. swampcy therefore holds a Wayland connection of its own for as long as the
session lives, with a virtual pointer (`zwlr_virtual_pointer_v1`) and a virtual keyboard
(`zwp_virtual_keyboard_v1`) on the seat.

It speaks the Wayland wire protocol with the Go standard library: a few requests, and file
descriptors passed both ways (keymaps out, clipboard pipes in). No libwayland, no cgo.

Holding the keyboard for the session's life matters for more than typing. sway sends apps
the active keyboard's keymap, only a keyboard that sends something becomes active, and a
seat whose last active keyboard has gone leaves apps with modifiers and no keymap. Chrome
crashes on that. So swampcy makes its keyboard active as soon as its keymap is set, and
presses a lone modifier before it launches an app.

## Typing on its own keyboard

swampcy first typed through wtype, which loads a keymap holding whatever characters it is
given. That had two costs:

- wtype's keyboard is created and destroyed on every run, leaving the seat with no active
  keyboard after each one; Chrome then crashed when it took focus.
- wtype binds a seat the way any client does, with no option to choose one. With two seats,
  two people (or a person and an agent) typing at once went into the same window,
  interleaved.

So swampcy types on its own keyboard. A character on the US keymap is typed as that key,
with Shift when it needs it. Anything else (accented letters, CJK, emoji, symbols) is typed
by giving the keyboard, for the moment, a keymap that holds exactly the characters needed, up
to 247 a keymap (xkb's keycodes 9 to 255), then the US keymap back with the held modifiers
restored. This is what wtype does, on a keyboard that never goes away. Keysym names are
limited to letters, digits and underscores before they are written into a keymap.

The cost: a keymap swap reaches every app on the seat. GTK and Chrome take it; an app that
rebuilds expensive state on every keymap could notice.

## A named seat and a named output

Running two desktops ([below](#two-desktops-on-two-seats)) showed that a client that binds
"the" seat or "the" output binds an arbitrary one. sway announces one `wl_seat` per seat,
and swampcy had kept the last one announced; the web viewer's capture likewise bound the last
`wl_output`. Screenshots, being of the whole layout, came back at half size with every
coordinate moved.

So:

- The pointer and keyboard join the seat `-seat` names (default `seat0`). swampcy binds every
  `wl_seat` to learn its name.
- The MCP server sees and points at one output, `-output`, by default the one at the
  layout's top left. Not the first sway lists: sway lists `HEADLESS-2` before `HEADLESS-1`.
- The virtual pointer is created on that output, so its motion covers that output alone.
  An agent's pointer cannot leave its screen; it moves windows with `move_window`, a sway
  command, not by dragging.

## Coordinates and screenshots

Every coordinate an agent sends or reads is a pixel of swampcy's screenshot, and swampcy
converts to sway's layout itself. The agent never has to reason about scale. Screenshots are
scaled down so the long edge is at most `-max-edge` (default 1568), never up.

The example config fixes the agents' output at 1280×800, scale 1, so screenshots are sent
at the screen's own size and nothing is rescaled. A screenshot that size costs about 1,330
image tokens. `zoom` captures a region at full resolution for small text, without changing
any other tool's coordinates.

## Accessibility through AT-SPI

Pixels are a fallback. Most toolkits (GTK, Qt, Firefox, LibreOffice, Chromium and Electron
when asked) describe their widgets on AT-SPI, and acting on a widget by reference is more
reliable than clicking where it was drawn.

The difficulty is position. Wayland tells no client where its window is, so AT-SPI's screen
coordinates are meaningless. swampcy takes an element's extents in window coordinates and
adds the window's client-area origin from sway's tree. It matches an AT-SPI application to a
sway window by the D-Bus connection's process id, then by title.

Three corrections follow from what toolkits actually do:

- **Chrome on Wayland draws a shadow around its window, even when tiled.** Its frame is its
  whole surface, about 10 pixels larger on every side than the window sway places, and its
  elements count from that surface. When a frame is larger than the client area by a small
  margin, swampcy takes half the difference off each element.
- **GTK 4 reports widget bounds approximately.** It sums each widget's offset from its parent
  and gets some wrong: a few pixels to 12 pixels off in places. `click_element` therefore
  performs the widget's action rather than clicking its bounds. Only a click aimed from the
  outline (`pointer`) is affected.
- **A GTK button acts on a click 250 ms after it is asked to.** Its activation shows the
  button pressed for `ACTIVATE_TIMEOUT` (250 ms), emits `clicked` only then, and ignores
  another activation in the meantime. `click_element` used to return at once, so an agent
  pressing the same button twice in quick succession (calculator's "0" in "800", say) lost
  the second press, whether the calls were parallel or one after the other: three presses
  100 ms apart gave one digit. A screenshot taken straight after a click showed it not done
  yet. After a click action in a GTK app, `click_element` now waits 300 ms before returning,
  holding the input lock so the next action waits too. Other toolkits' actions are not
  delayed.

Chrome shows AT-SPI only its frame unless started with `--force-renderer-accessibility`, so
`launch` adds that flag to Chrome and Chromium. `launch` also sets
`QT_LINUX_ACCESSIBILITY_ALWAYS_ON=1` and `ACCESSIBILITY_ENABLED=1`, and swampcy turns
`org.a11y.Status.IsEnabled` on at start. Apps started another way may have no tree.

Every D-Bus call has a timeout, so a hung app cannot hang a tool.

## Dependencies

swampcy is Go, built with `CGO_ENABLED=0`. A dependency is added when the standard library
cannot do the job safely:

- `github.com/godbus/dbus/v5`, because AT-SPI is D-Bus and the standard library has no D-Bus
  client.
- `github.com/modelcontextprotocol/go-sdk`, chosen over a JSON-RPC loop on the standard
  library, which could have done the job. The SDK keeps up with the protocol's versions and
  infers and checks the tools' input schemas.
- `github.com/pion/webrtc/v4` and `github.com/pion/interceptor`, for `swampcy web`: the
  standard library has no ICE, DTLS, SRTP, SCTP, RTP packetisation or congestion control.
- `github.com/ebitengine/purego`, to call libaom and OpenH264 at run time without cgo.

The Wayland client needs nothing beyond the standard library.

## Two desktops on two seats

Watching the agents' screen is not the same as using the machine. A tap from a phone moved
the agents' focus, and the screen was the agents' size whatever device looked at it. So
there are two desktops, chosen when connecting:

- **The agents'**, `HEADLESS-1`: fixed at 1280×800, the size screenshots are sent at. You
  watch it, or help directly.
- **Yours** (the owner's, in the flags and the config), `HEADLESS-2`, to its right:
  `swampcy web` sizes it to each viewer that asks for it.

They share apps as windows, not processes. A Chrome window on your desktop is the same
Chrome, with the same profile, as the agents'. Windows move between the desktops with a
button in the page, and with the `move_window` tool for agents.

Two shapes were tried:

- **A second sway session** isolates completely, but a window cannot move between
  compositors, and Chrome cannot open one profile twice.
- **One sway with two outputs and two seats** keeps windows movable. It needed the named
  seat and output and swampcy's own keyboard, above. With those, two seats typing at once
  into two windows of one GNOME Text Editor process did not cross: ASCII, accented letters,
  CJK, emoji and symbols, with one side's shortcuts interleaved with the other's keymap swaps.

The second was chosen. What it taught:

- `seat0` is the agents'. Every MCP server joins it by default, and **sway's commands act on
  seat0 even while the other seat is in use.** The second seat, `owner`, is yours:
  `swampcy web -seat owner` puts every viewer's input there, on either desktop.
- **Seats are declared in sway's config, never at run time.** Declaring one in a running
  sway took the devices off the existing seats.
- **New windows open where seat0's focus is.** The config focuses the agents' output at
  start, and `launch` moves a window that opened elsewhere to the agent's output.
- **A window moved between outputs can take seat0's focus with it.** If seat0's focus
  followed a window to your desktop, an agent's next keys would land in your app. So a move
  away from the agents keeps their focus on their desktop: on the window they had focused
  when there is another one there, and otherwise on their output (`focus output`). This holds
  for the page's moves and for `move_window`.

Without the second output or seat, `swampcy web` still works: it offers only the agents'
desktop, and shares the first seat.

## The web viewer

`swampcy web` is a subcommand, not a second binary: a long-running server beside the MCP
servers agents start. It shows either desktop in a browser, a phone's included, and sends
taps, clicks and keys back.

### WebRTC rather than VNC

The first viewer was wayvnc on loopback, reached through an SSH tunnel. That works from a
laptop and not from a phone, and wayvnc's per-client seats (`-t`) leak one seat per
connection, with a client's devices left on its seat after it leaves. The goal was the
desktop on a phone, as responsive as possible, with effort to build not counted as a cost.

The picture is a WebRTC video track; input comes back on a WebRTC data channel. Media and
data go over UDP straight to the machine, port 7683 by default, with host candidates only:
no STUN, no TURN, ICE-lite. Only a browser that completed the authenticated signalling can
finish the DTLS handshake, because the fingerprints travel over that signalling WebSocket.
The budget is about 15–20 ms to capture, encode and decode, on top of the network's round
trip.

wayvnc is not in the example config. A VNC server on loopback still works if you want one.

### AV1 and H.264, loaded through purego

A scripted 22-second desktop workload (typing, scrolling syntax-coloured code, page jumps,
clicking a calculator, dragging a window) was encoded at 1280×800 on an 8-vCPU VM with no
GPU, only on the frames sway reported as changed:

| Encoder | kbit/s | PSNR | Largest packet | CPU per frame, one thread | Latency, four threads |
|---|---|---|---|---|---|
| libaom AV1, real-time, speed 10, screen tools, constant quality | 320 | 52.4 dB | 58 KB | 6.0 ms | 4.4 ms |
| libvpx VP9, real-time, screen tuning | 891 | 42.9 dB | 94 KB | 5.2 ms | 2.5 ms |
| OpenH264, screen-content mode | 586 | 43.1 dB | 53 KB | 5.3 ms | |
| OpenH264, camera mode, CABAC | 1043 | 41.0 dB | 63 KB | 2.1 ms | |
| x264 veryfast, zerolatency | 660 | 36.5 dB | 59 KB | 4.6 ms | |
| SVT-AV1 preset 12, screen content | 755 | 43.6 dB | 189 KB | 4.1 ms | |

AV1's screen-content tools (palette mode, intra block copy) give near-lossless text at a
third of H.264's bits. Safari decodes AV1 over WebRTC only on devices with a hardware
decoder (iPhone 15 Pro and later, M3 Macs and later); Apple has no software AV1 decoder. So:

- **AV1 from libaom** when the browser offers it: real-time mode, speed 10, screen-content
  tools, constant quality 24 under a bitrate cap that follows the network (transport-wide
  congestion control and Google's estimator).
- **H.264 from OpenH264** otherwise, in screen-content mode, High profile when the browser
  accepts it. x264 came last of the H.264 encoders on this content.

Both libraries are loaded at run time through purego, so the build stays `CGO_ENABLED=0` and
cross-compiles. The price is calling C structs through hand-checked offsets. Each encoder
reads the library's defaults back at the offsets it uses before encoding, and refuses to
encode when they do not match; a library update that changes a layout fails loudly rather
than encoding garbage. Check the log after upgrading libaom or OpenH264.

Without a GPU, encoding is in software: about 36% of one core while the screen changes
continuously at 60 fps. A GPU or VA-API device would make hardware encoding the better
choice; swampcy does not use one.

### Encoding only what changed

screencopy's `copy_with_damage` holds each copy until sway has something new and says which
rectangles changed. Only those are converted to the encoder's colour format. A keyframe is
sent when the viewer reports loss or asks, and at a session's start, never on a schedule. An
idle desktop costs no CPU and no bytes.

### Who gets in

`swampcy web` listens on loopback only and refuses to start otherwise. It is meant to sit
behind `tailscale serve`, which terminates TLS, removes any `Tailscale-User-Login` header the
client sent, and sets it from the WireGuard peer's identity. `swampcy web` lets in one login,
`-login`, exactly, and the signalling WebSocket one `Origin`, exactly. Every response carries
a Content-Security-Policy that allows only the page's own origin and no framing.

The WebRTC UDP port is not behind `tailscale serve`. It is bound to the machine's Tailscale
IPv4 address only, never to every address, and speaks only ICE and DTLS-SRTP for a session
the authenticated WebSocket set up.

This is simple, and it rests on an assumption; see [Known limits](#known-limits).

### One viewer at a time

A new viewer ends the old session, whose page is told so and stops reconnecting. One seat
for viewers' input means two viewers would fight over one pointer.

## Opening apps on your desktop

New windows open where seat0's focus is, on the agents' desktop. Without a launcher, the
only way to start something on your desktop was to ask an agent to `launch` it and move it,
or to open it on the agents' desktop and bring it across.

The page lists the apps installed, from the freedesktop desktop entries in
`$XDG_DATA_HOME/applications` and `$XDG_DATA_DIRS`. Games a launcher such as Steam installs
appear there by themselves. The rules:

- **The page names an entry, never a command.** The server starts an ID only if a fresh scan
  finds it, and only if it is plain characters ending in `.desktop`, since it goes into a
  sway command: `exec gtk-launch <id>`.
- **It runs through sway's `exec`**, so the app gets sway's environment, XWayland's
  `DISPLAY` included, for X11-only apps. (The MCP `launch` tool removes `DISPLAY` on
  purpose.) `gtk-launch` applies the entry's `Exec` quoting and field codes.
- **It follows the app's windows for three minutes.** The first app tried this way, Steam,
  opened an updater window, closed it, and started again with a new window, which a
  launcher watching only the first window missed. So every new window that looks like the
  app's (its `StartupWMClass`, or its desktop file ID, against the window's app ID or X11
  class, ignoring case) is moved to your desktop for three minutes. If none looks like it
  after 45 seconds, the first new window is taken.

The launcher can start any installed app, a terminal among them, so `swampcy web` can give
a shell as the user it runs as.

## Zoom

Your desktop is first sized to the page: its CSS size at its pixel ratio, capped at 2560×1600
pixels. At a phone's pixel ratio of 3 that is 390×844 points in portrait, too narrow for some
dialogs, and an X11 app cannot adapt; XWayland apps are also drawn at 1× and scaled up.

So the output's mode stays the viewer's device pixels and its sway scale is a zoom, from 1 to
that pixel ratio, by quarters. At the most, apps are the viewer's own size; lower, they have
more room in the same pixels. The page keeps the choice per device and applies it by
connecting again. What is gained in room is lost in size.

## Single-seat apps through seat0

With two seats, taps from the page often did nothing in Chrome. A Wayland trace showed why:
Chrome's Ozone/Wayland connection, which owns its windows, binds the first `wl_seat` the
compositor offers (sway's `seat0`) and ignores the rest. sway sends a seat's events only to a
client's pointer and keyboard for that seat, so input on the `owner` seat never reached any
Chrome window, on either desktop. GTK apps and foot get it. Chromium has never supported more
than one seat.

The agents' seat cannot simply become yours (that is why there are two), moving Chrome to the
second seat would take it from the agents, and XWayland also takes one seat. So
`swampcy web` borrows the first seat for such apps:

- It keeps a second virtual pointer and keyboard per output, on seat0. When the window under
  a press is one whose app ID or X11 class matches `-single-seat-apps` (Chrome, Chromium,
  Brave and Edge by default), the input goes through seat0; otherwise through yours. A
  release goes where its press went. Keys follow your last press.
- **seat0's focus is lent while you use such a window, and given back** after 1.5 s with no
  input and nothing held, to the window the agents had focused, or to their output. It is not
  given back if an agent has meanwhile moved seat0's focus.
- While focus is lent, a press on no window in the tree also goes through seat0, because
  Chrome's menus and lists are popups the tree does not show.

The cost is that while you use Chrome, and for 1.5 s after, an agent's `type` or `key` goes
into your Chrome window. The agents' window loses keyboard focus on each loan and return,
and Chrome may close an open menu when the loan ends. The two pointers share one cursor on
seat0; each moves to its own point before pressing, but hover effects follow whichever moved
last. `-single-seat-apps ""` turns all of this off.

Two smaller fixes came with it: a press waits 20 ms after its move (GTK 4 ignores a press
with no time before it), and the page counts a touch as a tap up to 16 CSS pixels of drift,
since a fingertip rolls further than 10 as it lifts.

## Pasting the viewer's clipboard

The page first pasted by typing the text on the virtual keyboard. Typing turns every newline
into an Enter press, so a chat app sent a multi-line paste as several messages, and long text
typed slowly. On a desktop browser ⌘V reached the remote as Super+V and pasted nothing.

A Wayland clipboard is a selection some client offers; an app that pastes gets a pipe to read
it from. wlroots' data-control protocol (`zwlr_data_control_manager_v1`) lets a client with no
window set a seat's selection. So a paste is its own message: `swampcy web` makes the text the
selection of the seat the keys go to (seat0 for a single-seat app, yours otherwise), and only
once the compositor has it presses Ctrl+V there. If the selection cannot be set nothing is
pressed, since Ctrl+V would paste whatever was there before. A selection lasts only as long
as the connection that set it, so the server holds one clipboard connection per seat for its
life.

On ⌘V the page lets the browser paste into a hidden textarea and reads the text there, so the
keyboard path never asks for clipboard permission. A paste button uses
`navigator.clipboard.readText()`, and the browser asks first.

Limits: text only; one data-channel message, about 64 KB, refused rather than split; no copy
back out of the desktop. What you paste stays in that seat's clipboard until something
replaces it. On seat0 that is the agents' seat, so an agent can read the last thing you pasted
into Chrome. Do not paste a secret there and leave an agent working on the desktop.

## Replays

When an agent does something on the screen, a person may want to see what happened, not only
read a sentence about it: a short animated clip under the agent's reply, say.

- **A frame after each acting tool, in memory.** After `click`, `click_element`, `key`,
  `type`, `scroll`, `drag`, `launch` and `set_text` succeed, swampcy captures its output at
  640 pixels wide about 300 ms later, without making the tool wait, into a ring of the last
  40 frames. When the ring has had nothing for a minute it also captures the screen before
  the action, so a clip opens on the scene. The ring belongs to one MCP session and is never
  written to disk by itself.
- **A `replay` tool writes a clip:** an animated WebP at 2 to 4 fps from the last 30 minutes'
  frames, through `img2webp` or ffmpeg, falling back to an ffmpeg GIF. Running those as
  programs, as grim is run, adds no Go dependency. It returns the path and empties the ring.
- **Clips stay on the machine, in one private directory**, by default
  `$XDG_STATE_HOME/swampcy/replays` (else `~/.local/state/swampcy/replays`). swampcy makes it
  0700 and writes each clip 0600 by renaming a temporary file into place. Clips expire after
  seven days, and swampcy deletes only files it named.
- **Nothing logs or sends a clip's contents.** The tool's description tells the agent never to
  send or upload one. Serving clips to a person is another program's job, and it should serve
  them only to that person, authenticated.

Screenshots go to the agent over stdio and are gone afterwards; a clip is a file that outlives
the tool call. If a browser on the screen is signed in to your accounts, a clip can show
them. Every agent on the machine can already see the screen, so a clip gives them nothing new;
the new exposure is a file that stays around. `-replay=false` turns all of it off.

## Signing a browser in

The apps on swampcy's screen run as the user swampcy runs as. A file chooser there can open
anything that user can, `~/.config` included. swampcy does not separate the driven apps from
the user's own credentials, and does not try to.

Whether to sign a browser on that screen in to your own accounts is your decision, and it
changes what an agent can do. The case for it: an agent with a signed-in browser can do on the
web what you could, which is much of what makes it useful. If you do it:

- **Sign in by hand, through `swampcy web`.** Type passwords and second factors yourself, so
  they never pass through an agent's transcript. Only the session cookies stay on the machine.
- **Any agent with swampcy can then act as you on those sites.** That is every agent session
  on the machine, several at once, each limited only by its permission prompts and its own
  judgment. Nothing in swampcy or the browser tells one agent's use from another's.
- **A web page can steer an agent with your accounts in the same window.** The screen is a
  way in for prompt injection: a page or document shown on it can carry instructions.
  swampcy has no screenshot classifier; do not assume one.
- **The cookies are readable by anything running as that user.** Copying the profile copies
  the sessions.
- **The record is the agents' transcripts.** There is no separate log of what was done in the
  browser. The sites' own account activity pages are the other side of it.
- **A sign-in on your desktop is the agents' too,** since it is the same browser and profile.
- **Undoing it is signing out**, or deleting the browser's profile. Restoring a VM snapshot
  does not undo sessions made after it.

The safe default is not to sign in to anything that matters. The case against is strongest
on a machine anyone else can reach.

## Known limits

- **`swampcy web` trusts the `Tailscale-User-Login` header from anything that can reach its
  loopback port.** `tailscale serve` sets that header honestly, but any local process can
  connect to `127.0.0.1:7682` and send whatever header it likes. The design assumes a
  single-user machine, where every local process already runs as you. Do not run it on a
  machine shared with users you do not trust.
- **One login and one viewer.** It lets in exactly one Tailscale login, and a new viewer ends
  the old one.
- **The UDP port is outside `tailscale serve`**, on the machine's Tailscale IPv4 address. It
  speaks only ICE and DTLS-SRTP for a session the WebSocket set up, but it is a listener of
  its own.
- **Apps run as you.** No sandbox or separate user stands between the driven apps and your
  files and logins. See [Signing a browser in](#signing-a-browser-in).
- **No prompt-injection screening.** Anything shown on the screen can carry instructions.
- **Tool inputs may be logged by the MCP client.** swampcy logs no tool arguments, but a
  client that logs tool inputs will record the text of `type` and `set_text`, which may be
  typed into a signed-in browser.
- **Software encoding through hand-checked C struct offsets.** A library update that moves a
  layout makes swampcy refuse to encode until it is updated.
- **Keymap swaps reach every app on the seat** when typing characters off the US keymap.
- **An agent's pointer cannot leave its output.** It moves windows with `move_window`.
- **GTK 4's widget bounds are a few pixels off.** Prefer `click_element`'s actions.
- **A click action on a GTK app takes 300 ms**, waiting for GTK's button activation to finish.
- **A seat can be left with no active keyboard** when a swampcy process exits, and Chrome
  crashes the next time it takes focus, until some swampcy types or launches something.
  `launch` guards against it; an app focused another way may not.
- **Single-seat apps are matched by name.** An Electron app also binds only the first seat but
  is not matched by default; add it to `-single-seat-apps`. Whether XWayland passes a second
  seat's input through was not tested. While you use Chrome, an agent's keys go into it.
- **The launcher can move the wrong window.** A single-instance app may raise an existing
  window on the agents' desktop instead of opening one; another window of the same app opened
  in the three minutes after a launch is moved too.
- **Paste is text only, at most one data-channel message, and one way.** ⌘V also presses and
  releases Super on the remote, because the page sends ⌘ before it knows V will follow.
- **Restarting sway closes every app on both desktops.**
- **sway only.** Another compositor would need its own seat, output and input handling.
