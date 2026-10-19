<p align="center"><img src="logo.jpg" alt="swampcy: the letters MCP rising out of a swamp, with eyes" width="360"></p>

# swampcy

swampcy gives a coding agent a Linux desktop it can see and use. It is an MCP server, spoken
over stdio, that drives a headless [sway](https://swaywm.org) session with no GPU and no
monitor. The agent takes screenshots, clicks, types and starts apps. It can also find an app's
buttons and text fields through the accessibility bus (AT-SPI) and press or fill them by name
instead of by pixel.

I wrote it because the computer-use tools coding agents ship with are for macOS, and the Linux
ones that work without a GPU are X11. It runs on a VM the agents work in, and I watch it from
my phone with `swampcy web`.

## What it needs

- Linux, and sway run headless (`WLR_BACKENDS=headless WLR_RENDERER=pixman sway`), with an
  output of the size you want screenshots at. [`examples/`](examples/) has a sway config and
  systemd user units for it.
- `grim`, for screenshots
- `at-spi2-core`, for the widget tools
- `img2webp` (libwebp) or `ffmpeg`, for the `replay` tool's clips
- for `swampcy web`: [Tailscale](https://tailscale.com) with HTTPS certificates on, and libaom
  (AV1, x86-64 only) or OpenH264 (H.264) as shared libraries. swampcy loads them at run time
  and needs no cgo; without them the MCP server still works.

The pointer and keyboard are swampcy's own: it holds a virtual pointer and keyboard on sway's
seat, because a headless seat has none and apps ignore input without them, and it types on
that keyboard. With several seats or outputs, `-seat` (default `seat0`) and `-output` (default
the top-left one) say which it joins and sees.

## Install

```sh
go install github.com/nabkey/swampcy@latest
claude mcp add --scope user swampcy -- ~/go/bin/swampcy
```

Any MCP client that runs a stdio server works the same way. swampcy finds the session from
`SWAYSOCK` and `WAYLAND_DISPLAY`, or from the sockets in `$XDG_RUNTIME_DIR` when there is
exactly one sway. Start apps with its `launch` tool, so they get the display and accessibility
switched on.

| Tool | |
| --- | --- |
| `screenshot`, `zoom` | the whole screen, or a region at full resolution |
| `click`, `move`, `drag`, `scroll` | the pointer, in screenshot pixels |
| `type`, `key` | any Unicode text; keys and shortcuts like `ctrl+s` |
| `windows`, `focus_window`, `move_window`, `launch` | sway's windows, and starting apps |
| `accessibility_tree`, `find_elements` | widgets, each with a ref like `e12` |
| `click_element`, `set_text`, `get_text` | act on a widget by ref |
| `replay` | an animated clip of what the agent did, saved under `-replay-dir` |

Coordinates are always pixels of swampcy's screenshots, which are scaled to at most 1568 on the
long edge (`-max-edge`); swampcy converts them to the screen's.

## Watching: `swampcy web`

`swampcy web` serves the desktop to a browser, phone included: the screen as AV1 or H.264 over
WebRTC, encoded only when something on it changes, and your pointer, touch and keyboard sent
back. With a second output and seat in sway's config it also gives you a desktop of your own
beside the agents', and windows can move between the two.

```sh
swampcy web -login you@example.com -origin https://box.example.ts.net:8443
sudo tailscale serve --bg --https=8443 http://127.0.0.1:7682
```

It listens on loopback only and lets in the one Tailscale login given, trusting the
`Tailscale-User-Login` header that `tailscale serve` sets; the video goes over UDP 7683 on the
machine's Tailscale address. It assumes a single-user machine: anything that can reach its
loopback port can claim to be you.

## What doesn't work yet

- AV1 is x86-64 only. libaom's structs are laid out by hand from the v3.13.3 headers, and a
  library update that moves one makes swampcy refuse to encode until it is updated.
- One login, one viewer. A second browser tab ends the first.
- Chrome binds only the first seat, so while you type into Chrome on your own desktop, an
  agent's keys go there too. Electron apps have the same problem and are not matched by
  default; add them to `-single-seat-apps`.
- sway only. Another compositor would need its own seat, output and input handling.
- Restarting sway closes every app on both desktops.

The full list is at the end of [docs/design.md](docs/design.md#known-limits).

## Good to know

- GTK 4 reports its widgets' positions a few pixels off. `click_element` performs a widget's
  action instead of clicking where GTK says it is.
- The apps run as you. A browser signed in on swampcy's screen lets every agent with swampcy act
  as you on those sites; sign in only to what you would hand the agent anyway.
- swampcy itself opens no network port. If you run a VNC server instead of `swampcy web`, keep
  it on loopback.

Why it is built this way: [docs/design.md](docs/design.md). How it behaves:
[docs/spec.md](docs/spec.md) and, for the browser viewer, [docs/web.md](docs/web.md).

## License

[MIT](LICENSE)
