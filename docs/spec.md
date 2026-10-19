# swampcy, the MCP server

How `swampcy` behaves as an MCP server. The code is `main.go` and `internal/desktop/`.
`swampcy web` is specified in [web.md](web.md), and the reasons behind both are in
[design.md](design.md). The key words MUST, MUST NOT, SHOULD and MAY are used as in RFC 2119.

## Overview

`swampcy` is an MCP server spoken over stdio. An agent starts it; it finds a running sway
session and gives the agent tools to see that session's screen, move its pointer, type on its
keyboard, start apps on it, and read and operate the apps' widgets through AT-SPI. It holds
no session of its own and serves nothing on the network.

## Requirements

### The session

- swampcy MUST take the sway IPC socket from `SWAYSOCK` and the Wayland display from
  `WAYLAND_DISPLAY` when they are set.
- Otherwise it MUST look in `XDG_RUNTIME_DIR` (`/run/user/<uid>` when that is unset): for
  `sway-ipc.<uid>.*.sock` sockets that answer a version request, and for `wayland-*` sockets
  without a `.lock` suffix. When it finds more than one of either, it MUST fail with an error
  naming them, not choose one.
- It MUST start and serve its tools even when no session is found, and each tool MUST look
  for the session again when the one it knew stops answering. A tool called with no session
  MUST return a tool error saying how to start one.
- Programs swampcy runs for the session (grim, launched apps) MUST get swampcy's
  environment with `WAYLAND_DISPLAY`, `SWAYSOCK`, `XDG_RUNTIME_DIR` and
  `XDG_SESSION_TYPE=wayland` set to the session's, with `DISPLAY` removed, and with
  `DBUS_SESSION_BUS_ADDRESS` defaulted to `unix:path=$XDG_RUNTIME_DIR/bus`.

### Coordinates

- Every coordinate a tool takes or returns MUST be a pixel of swampcy's full-screen
  screenshot. swampcy MUST convert between those and sway's layout itself.
- The MCP server MUST see one output: the one `-output` names, or without it the one at the
  layout's top left (leftmost, then topmost; not the first sway lists). A name that is not an
  active output MUST be a tool error that lists the outputs there are.
- A screenshot MUST be that output, scaled down when its long edge exceeds `-max-edge`
  (default 1568) so that the long edge equals it, and never scaled up.
- A point outside the screenshot MUST be refused with a tool error, not clamped.

### Seeing

- `screenshot` MUST return the image as MCP image content, followed by text giving its size,
  the layout's size when it was scaled, and the focused window.
- Screenshots MUST be PNG, or JPEG with `-jpeg`.
- `zoom` MUST capture the given rectangle at the layout's own resolution, scaled down only to
  keep its long edge within `-max-edge`, and MUST say that other tools' coordinates are
  unchanged.
- `windows` MUST list every window in sway's tree, tiled and floating, with its container id,
  title, app id (or X11 class), process id, bounds, workspace, and whether it is floating or
  focused. A window on another output than swampcy's MUST say which, and that it is not in
  screenshots and cannot be clicked.

### Acting

- Pointer, keyboard, focus, launch and element actions MUST be serialised: a tool that moves
  the pointer or presses keys MUST NOT interleave with another, even when the agent calls
  tools in parallel.
- `click` MUST move the pointer to the point and press and release the button `count` times
  (1 to 3); `button` is `left`, `right` or `middle`.
- `drag` MUST press at the start, pass through intermediate points to the end, and release,
  releasing the button if any step fails.
- `scroll` MUST move the pointer to the point and send `amount` (1 to 50) wheel clicks.
- swampcy MUST hold, for as long as the session answers, a Wayland connection with a virtual
  pointer (`zwlr_virtual_pointer_v1`) and a virtual keyboard (`zwp_virtual_keyboard_v1`,
  given a US keymap), creating them at start when a session is found and again when the
  connection dies. A headless sway has no input devices without them.
- They MUST join the seat `-seat` names (default `seat0`). sway announces one `wl_seat` per
  seat, so swampcy MUST bind each to learn its name and use the one named, and MUST fail
  naming the seats there are when none is. The pointer MUST be created on swampcy's output
  (`create_virtual_pointer_with_output`), so that its motion covers that output alone.
- The virtual keyboard MUST make itself the seat's active keyboard once its keymap is set, by
  sending a modifiers change (Shift held, then nothing) with no key: sway sends apps the
  active keyboard's keymap, only a keyboard that sends something becomes active, and apps
  that take focus on a seat with none get modifiers with no keymap (Chrome crashes on them).
- The pointer MUST be driven through the virtual pointer: absolute motion over its output,
  buttons as evdev codes, and wheel clicks as discrete axis events, each followed by a frame.
- `click` MUST pause about 20 ms after moving, hold each button about 40 ms, and leave about
  60 ms between repeated clicks: GTK 4 ignores a press and release with no time between them.
- `type` MUST type on swampcy's own virtual keyboard, so that the text lands on its seat. A
  character on the US keymap MUST be typed as that key, with Shift when it needs it, unless
  a modifier is held. Any other text MUST be typed by giving the keyboard a keymap that holds
  exactly the characters needed, at most 247 to a keymap (xkb's keycodes 9 to 255), with no
  modifiers set, and then the US keymap back with the modifiers held restored. Newline and
  carriage return MUST be typed as Return and tab as Tab; any other control character MUST be
  refused. `-type-delay` (default 2 ms) is the pause after each character.
- `key` MUST accept space-separated combinations of modifiers (`ctrl`, `shift`, `alt`,
  `super`, `altgr` and their aliases) and one key each, map common names (Enter, Esc,
  PageDown, punctuation) to XKB keysyms, pass unknown names through as keysyms, and refuse an
  unknown modifier. A key on the US keymap MUST be pressed with its modifier keys held around
  it. Any other keysym MUST be pressed on a keymap of its own with the modifiers sent as a
  mask, and the US keymap given back. A keysym name MUST be refused unless it is 1 to 64
  letters, digits and underscores, since it is written into a keymap.
- `focus_window` MUST refuse a container id that is not a window.
- `move_window` MUST move a window to the output named (default swampcy's) with
  `[con_id=<id>] move container to output <name>`, refusing a window or output that is not
  there and an output name other than 1 to 64 letters, digits, `_`, `.` and `-`. Moving one
  to swampcy's output MUST focus it; moving one away MUST, in the same command, focus again
  the window that had focus when that is another one on swampcy's output, and otherwise focus
  swampcy's output, so that seat0's focus never follows the window away.
- `launch` MUST run the command with `/bin/sh -c` in a new session, detached from swampcy,
  with output appended to `-app-log` (default `$XDG_RUNTIME_DIR/swampcy/apps.log`), and with
  `QT_LINUX_ACCESSIBILITY_ALWAYS_ON=1` and `ACCESSIBILITY_ENABLED=1` set. Before starting it,
  it MUST press a lone modifier (`Shift_L`) on its own keyboard, so that the seat has a live
  active keyboard: Chrome segfaults at start-up when the seat's last-used keyboard has gone,
  as it has whenever another swampcy session has exited. A command whose first word is Chrome or
  Chromium (`google-chrome`, `google-chrome-stable`, `chromium`, `chromium-browser`, `chrome`)
  MUST get `--force-renderer-accessibility` after that word unless it already has it; Chrome
  154 otherwise shows AT-SPI only its frame. It SHOULD wait up to
  `wait_ms` (default 5000) for a window that was not there before, and report it, preferring
  one owned by the process it started. It SHOULD then wait about half a second more, because
  a window appears before its first frame is drawn. A window that opened on another output
  (new windows open where seat0's focus is) MUST be moved to swampcy's output and focused.

### Accessibility

- With `-a11y` (the default), swampcy MUST set `org.a11y.Status.IsEnabled` on the session bus
  at start, and MUST still start when that fails.
- It MUST reach AT-SPI through the address `org.a11y.Bus.GetAddress` returns, dialling again
  when the connection drops. Every D-Bus call MUST have a timeout, so a hung app cannot hang a
  tool.
- When a window's application is not yet on the accessibility bus, swampcy SHOULD keep
  looking for about three seconds: apps register a moment after their window appears.
- A sway window's AT-SPI frame MUST be found by matching the application's D-Bus connection's
  process id to the window's, then the frame's name to the window's title. When no process
  matches, a frame whose name is the window's title MAY be used if it is the only one.
- An element's position MUST be its extents in window coordinates, less its frame's, plus
  the origin of the window's client area in sway's tree (the container's rect plus its
  window rect). Screen coordinates from AT-SPI MUST NOT be used.
- When the frame's extents are larger than that client area in both width and height, by
  at most 128 pixels each, the difference MUST be taken as a shadow the client draws
  around its window, the same on both sides, and half of it MUST be subtracted from each
  element's position. Chrome on Wayland does this even when tiled: its frame is its whole
  surface, 10 pixels larger on every side than the window sway places, and its elements'
  window coordinates count from that surface. A frame no larger than the client area MUST
  get no correction.
- Element references MUST be short (`e12`), and MUST be stable: the same element MUST get the
  same reference from every call.
- `accessibility_tree` MUST outline one window (default the focused one) depth-first, leave
  out elements that are not showing unless `all` is set. It MUST leave out, while keeping
  their children, unnamed generic containers (GTK 4's `generic` and `group` among them)
  without actions of their own, and labels that repeat their parent's name. It MUST NOT list
  the actions toolkits give every label and text widget (`clipboard.`, `selection.`, `menu.`,
  `link.`, `text.`), and MUST list at most six actions a line. Each line MUST carry the
  reference, role, name, bounds, notable states, and actions. It MUST stop at `max_nodes`
  (default 400) and say so.
- `find_elements` MUST match case-insensitive substrings of name and role, in one window or
  all of them, and MUST refuse a query with neither.
- `click_element` MUST perform the named action, or the first of click, press, activate,
  toggle, jump, open, select, switch that the element offers. With `pointer`, or when the
  element has no action, it MUST click the centre of the element's current bounds instead.
- `set_text` MUST use EditableText's `SetTextContents`; `get_text` MUST return at most 20,000
  characters of Text and say how many more there were.

### Replays

- With `-replay` (the default), after `click`, `click_element`, `key`, `type`, `scroll`,
  `drag`, `launch` or `set_text` succeeds, swampcy MUST queue a capture of its output, scaled
  so that it is `-replay-width` (default 640) pixels wide and never scaled up, taken about
  300 ms after the tool returns. The tool MUST NOT wait for that capture, and a capture
  that cannot be queued MUST be dropped. No other tool MAY capture a frame.
- Before such a tool acts, when the ring holds no frame from the last minute, swampcy MUST
  capture the output once as it is, so that a clip starts with the scene.
- Frames MUST be kept in memory only, in a ring of at most `-replay-frames` (default 40) that
  drops the oldest.
- `replay` MUST be offered only with `-replay`. It MUST wait for queued captures, then encode
  the ring's frames from the last 30 minutes, followed by three copies of the last, at `fps`
  frames a second (2 to 4, default 3). It MUST use `img2webp` when it is on PATH, and
  otherwise ffmpeg's `libwebp_anim`. If that fails, it MUST fall back to an ffmpeg GIF. It
  MUST return the clip's path, and MUST empty the ring unless `keep` is set. With no frames,
  it MUST return a tool error.
- A clip's `name` MUST be 1 to 64 characters from `A-Z`, `a-z`, `0-9`, `-` and `_`, and MUST
  be refused otherwise. swampcy adds the extension (`.webp` or `.gif`). Without a name it
  MUST pick a random one.
- Clips MUST be written to `-replay-dir` (default `$XDG_STATE_HOME/swampcy/replays` when
  `XDG_STATE_HOME` is an absolute path, else `~/.local/state/swampcy/replays`). swampcy MUST
  create that directory with mode 0700, or set an existing one to 0700, and MUST write each
  clip with mode 0600 by renaming a temporary file into place. Frames written for an encoder
  MUST go in a 0700 temporary directory inside it, which MUST be removed after encoding.
- swampcy MUST delete clips older than `-replay-keep` (default seven days) when it starts and
  on each `replay`, and leftover frame directories older than an hour. It MUST NOT delete
  anything in the directory but files named like its clips (`<name>.webp`, `<name>.gif`) and
  its own `.frames-*` directories.
- swampcy MUST NOT log, send or upload a frame or a clip. It MAY log their count and size.

### Exposure and logging

- As an MCP server (no subcommand), swampcy MUST NOT open any network listener. Its protocol
  is stdin and stdout only. `swampcy web` is specified in [web.md](web.md).
- It MUST log to stderr: each tool's name, duration and error. It MUST NOT log tool
  arguments, which may hold typed text.
- `-version` MUST print the commit the binary was built from (Go's `vcs.revision`, else the
  module version) and exit.

## Out of scope

- Starting or owning a session (swampcy attaches to one).
- Credential separation between the driven apps and the user running swampcy.
- Any compositor but sway.

## Running it

This section is guidance, not requirements. [`examples/`](../examples/) has a working set.

- Run sway as a user unit of its own, such as
  [`examples/swampcy-sway.service`](../examples/swampcy-sway.service): `WLR_BACKENDS=headless`
  and `WLR_RENDERER=pixman` for no GPU, `WLR_HEADLESS_OUTPUTS=2` for two desktops, and
  `Restart=on-failure`. swampcy attaches to it; it does not start sway.
- Give sway a configuration of its own, such as [`examples/sway.conf`](../examples/sway.conf),
  rather than the distribution's: no bar, no idle or lock, no keybindings. For the two
  desktops ([design.md](design.md#two-desktops-on-two-seats)) it puts `HEADLESS-1` at
  1280×800, scale 1, at the layout's origin with workspace 1, and `HEADLESS-2` to its right
  with workspace 2.
- Declare the seats in the configuration (`seat seat0 fallback true`, `seat owner fallback
  false`), never at run time: adding a seat to a running sway takes the devices off the
  others.
- Focus `HEADLESS-1` at start, since new windows open where seat0's focus is.
- Register the server with the agent, for example
  `claude mcp add --scope user swampcy -- swampcy`. Each agent session starts its own
  swampcy, so a new binary takes effect at the next session; restarting sway is never needed
  for that, and closes every app on it.
- Install grim, and `at-spi2-core` for the widget tools. `img2webp` (libwebp) or ffmpeg is
  needed only for `replay`.
