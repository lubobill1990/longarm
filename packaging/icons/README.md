# Longarm icon

The friendly agent with long arms on a blue tile: one arm reaches out past the
corner and holds the finished task.

| file | what |
|---|---|
| `longarm.png` | the master, 512 px, transparent outside the tile: used for 40 px and up |
| `longarm-small.png` | its simplification for 16-32 px: the head and the long arm only, inside the tile (the full icon turns to mush below 40 px) |
| `longarm.ico` | generated: 16, 20, 24, 32 from the small master, 40, 48, 64, 96, 128, 256 from the master |
| `main.go` | the generator |

Where the icon goes (one icon for the whole product, like Microsoft's apps: the
CLI, the worker service and the updater carry the same one as the tray, so
Explorer and Task Manager show they belong together):

- every Windows exe: `cmd/<tool>/rsrc_windows_{amd64,arm64}.syso` (Windows
  resource objects; `go build` links them in) for `longarm`, `longarmd`,
  `longarm-tray`, `longarm-update`, `longarm-hub`, `longarm-relay`;
- the MSI (`packaging/windows/longarm.wxs`): Add/Remove Programs
  (`ARPPRODUCTICON`) and the Start menu shortcut - which is also what the tray's
  notifications show;
- the web UI's favicon (`web/public/favicon.png`, 64 px from the small master):
  the hub's panel and the tray's window.

The tray's notification-area icon stays the live chart (a CPU / memory column
per worker, `internal/tray/icon.go`): it carries state, not the brand.

## Changing it

Replace a master (PNG, square, transparent background; keep the small one a
simplification of the big one) and regenerate:

```sh
go run ./packaging/icons      # writes longarm.ico, the .syso files and web/public/favicon.png
node web/build.mjs            # the embedded web UI (internal/webui/dist) picks up the favicon
```

The masters were drawn with gpt-image-2.5 (Fluent-style 3D app icon; the small
one by editing the big one), and the checkerboard the model painted for
"transparent" was cut out afterwards.
