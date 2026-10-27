# xodus-nexus-wails

> **This is not a standalone project.** It is a concept / experiment: a port of
> [Xodus Nexus](https://github.com/domsvoboda/xodus-nexus) by
> [@domsvoboda](https://github.com/domsvoboda), rewritten in **Go + [Wails](https://wails.io)**.

The launcher is meant for running **Windows Xbox Game Pass games on Linux**.

All ideas, design and functionality belong to the original project. This
repository only exists to try out how the same launcher runs and feels when
built with Go and Wails instead of the original stack.

If you are looking for the real thing, please use and support the
[original Xodus Nexus](https://github.com/domsvoboda/xodus-nexus).

## Why

- **Experiment.** Check how the launcher works on a Go backend with a Wails
  webview frontend.
- **Size.** The Go + Wails build is roughly **10× smaller** than the original
  build.

## Status

Work in progress. The backend is being ported module by module
(`internal/...`); the frontend lives in `frontend/`. Expect missing features
and rough edges.

The project currently builds into a single Wails binary with the frontend
embedded. The original Electron main-process API has been ported into the Go
backend, including authentication, account logout, keyring integration,
downloads, game launch, game removal, diagnostics, settings, demo mode and
shutdown handling.

Implemented so far:

- Authentication through the external browser.
- Main launcher API bindings for the existing React frontend.
- Download queue with pause, resume and cancel support.
- Game launch flow, including screen sleep inhibition through D-Bus.
- VC++ runtime installation flow.
- Game removal helpers.
- Steam and desktop shortcut helpers.
- Diagnostics and settings APIs.
- Single-instance handling per profile.
- Native dialogs, notifications, clipboard access and fallback font handling.
- Frontend adapter that exposes the Wails bindings through `window.launcher`.

Known limitations:

- The app builds, but the full runtime flow still needs manual testing.
- The new `internal/launcher` service still needs dedicated tests.
- Login currently uses Chrome / the external browser flow only.
- Embedded WebKitGTK login window is not implemented yet.
- Window position persistence is not implemented yet.
- X11 window identifier passing for keyring password prompts is not implemented
  yet.
- AppImage packaging is not implemented yet.
- Auto-update is disabled until this port has its own release channel.

## Requirements

- Linux
- Go 1.27+
- GNU Make
- Node.js — build time only, to compile the TypeScript/React frontend
  (not needed to run the built binary)
- GTK 4 and WebKitGTK 6.0 development libraries (plus a C compiler and
  `pkg-config`, needed by cgo)

The Wails CLI is not required: the project is built with plain `go build`
through the `Makefile`.

### System libraries

Fedora / RHEL:

```sh
sudo dnf install gtk4-devel webkitgtk6.0-devel gcc pkgconf-pkg-config
```

Ubuntu (24.04+) / Debian (12+):

```sh
sudo apt install libgtk-4-dev libwebkitgtk-6.0-dev build-essential pkg-config
```

Arch Linux / Manjaro:

```sh
sudo pacman -S gtk4 webkitgtk-6.0 base-devel pkgconf
```

Other distributions: install the GTK 4 and WebKitGTK 6.0 development
packages, then check that both are found:

```sh
pkg-config --modversion gtk4 webkitgtk-6.0
```

## Build and run

Build the frontend and the binary (installs npm dependencies on first run):

```sh
make
```

The binary embeds `frontend/dist`, so the frontend is always built first.
The version defaults to the one in `frontend/package.json`; override it with
`make VERSION=1.2.3`.

Run the built binary:

```sh
./build/bin/xodus-nexus-wails
# or build and run in one step
make run
```

### AppImage (Steam Deck)

SteamOS has a read-only system and does not ship GTK 4 or WebKitGTK 6.0, so
the Steam Deck uses a self-contained AppImage with these libraries bundled:

```sh
make appimage   # writes build/bin/xodus-nexus-wails-x86_64.AppImage
```

Each GitHub release includes it. On the Steam Deck, in Desktop Mode, download
`xodus-nexus-wails-<version>-x86_64.AppImage`, make it executable
(`chmod +x`, or Properties → Permissions → Is executable) and run it. To use it
in Game Mode, add it to Steam with **Add a Non-Steam Game**, or with
**Add Nexus to Steam** in the launcher settings.

Run the Go and frontend tests:

```sh
make test
```

Run the Go tests with the race detector:

```sh
make test-race
```

Build the frontend only:

```sh
make frontend
```

Remove build output:

```sh
make clean
```

## License

MIT, same as the original project. See [LICENSE](LICENSE).

Original work © Xodus Nexus contributors.
