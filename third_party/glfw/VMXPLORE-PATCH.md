# Patched GLFW: fullscreen on the output the window is on

This directory is a copy of the Go module `github.com/go-gl/glfw/v3.4/glfw`
at the revision Fyne pins (see `go.mod` at the repository root, which
`replace`s the module with this directory), with one change:

    glfw/src/wl_window.c, acquireMonitor():
      xdg_toplevel_set_fullscreen(..., NULL)  instead of  ...->wl.output
      libdecor_frame_set_fullscreen(..., NULL)  likewise

## Why

Under Wayland a client is never told which output its window is on.
Upstream GLFW therefore fullscreens onto the monitor it *guesses* — the
first output the compositor announced, which it calls primary — and on a
multi-head desktop the console jumped to another screen on every toggle
(onyx, 2026-08-11: window on monitor 1, fullscreen on monitor 3; again
2026-09-04 with the window on DP-2 and DP-1 primary).

The protocol has the answer: `xdg_toplevel.set_fullscreen` accepts a NULL
output and then the compositor uses the output the surface is on. GLFW
never passes NULL, and neither GLFW nor Fyne exposes a way to ask for it,
so the one-line change lives here. The monitor GLFW still records for the
window is only used for its size and refresh hints, which Wayland ignores.

Only the Wayland backend is touched; X11 keeps GLFW's geometry search,
which is correct there.

## Refreshing after a Fyne upgrade

1. `go mod download github.com/go-gl/glfw/v3.4/glfw` at the new pin and
   find it under `$(go env GOMODCACHE)/github.com/go-gl/glfw/v3.4/glfw@<rev>`.
2. Copy that directory over this one (drop `testdata/`, it is 2 MB of
   images) and `chmod -R u+w`.
3. Re-apply the change above; `go test -run TestGLFWPatchPresent` fails
   until the marker comment `vmxplore patch` is back in `wl_window.c`.
4. Build with `-tags gui` and toggle fullscreen on a window that is NOT on
   the primary monitor.

Licences: GLFW is zlib (`glfw/LICENSE.md`), the Go binding BSD-3
(`LICENSE`). Both travel with the copy.
