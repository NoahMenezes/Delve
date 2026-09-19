# Delve

A local-first, AI-powered file browser CLI. Delve indexes your files locally
and lets you search by meaning — entirely offline. No account, no
subscription, no cloud calls ever.

Inspired by intelligent file browsers like [Poly](https://poly.app), minus the
cloud: everything on your machine, in a single binary.

## Core principles

- **100% local-first** — no network calls except a one-time download of the
  embedding model + inference runtime on first use; everything else runs
  offline. No API keys, no accounts, no cloud storage — ever.
- **Single-file distribution** — download one binary, run it, works
  everywhere. No Python, Node, or other language runtime required. (The
  embedding runtime library downloads itself once into `~/.delve/lib/` —
  still zero setup, still offline afterwards.)
- **Trust and transparency** — Delve never moves, renames, or deletes files
  without a `--dry-run` preview first and explicit user confirmation.

## Building

Requires Go 1.21+ and a C compiler (for SQLite via `mattn/go-sqlite3`).
Delve's full-text search needs SQLite's FTS5 extension, which is compiled in
via a build tag — **always build with `-tags fts5`**:

```bash
go build -tags fts5 -o delve .
./delve scan ~/Documents
./delve search invoice --extension pdf
./delve search --semantic "documents about vacation planning"
```

## Usage (so far)

```bash
delve scan [path] [--verbose] [--extract-content=true] [--embed=true]  # index metadata + text + vectors (default: current dir)
delve search <query> [--limit 20] [--extension pdf]     # keyword search names/paths/content
delve search --semantic <query> [--limit 20]            # meaning search over local embeddings
delve organize [path] --dry-run                         # dry-run folder suggestions (grouped, explained; never moves)
delve watch [path]                                      # live incremental re-indexing (Ctrl+C stops)
delve                                                   # interactive terminal browser (needs a TTY)
```

The index lives at `~/.delve/delve.db` (plain SQLite — inspect it with the
`sqlite3` CLI any time).

## Desktop GUI (`delve-gui`, Phase 8)

`gui/` holds a Wails v2 app — a **separate binary** sharing the same
`internal/` Go packages via a `replace` directive, so the CLI is untouched.
The React frontend calls thin Go bindings (`ScanDirectory`, `SearchFiles`,
`SemanticSearch`, `SuggestOrganization`); no engine logic is duplicated.
The Organize view records per-suggestion approve/reject but its Apply button
stays disabled — the engine is dry-run only, and the GUI must not imply
otherwise.

Honest packaging note: unlike the CLI's single static binary, the GUI links
the system WebKitGTK on Linux, and Bun (JS runtime/package manager, build
time only — never at runtime) is required to build the React frontend.
The CLI remains the primary artifact.

```bash
# Linux system dependency (WebKitGTK) — required for wails dev/build:
sudo dnf install -y webkit2gtk4.0-devel gtk3-devel   # Fedora/RHEL
# sudo apt install -y libgtk-3-dev libwebkit2gtk-4.0-dev  # Debian/Ubuntu

go install github.com/wailsapp/wails/v2/cmd/wails@v2.16.0
cd gui
wails dev -tags fts5        # dev server with live reload
wails build -tags fts5      # production binary: gui/build/bin/delve-gui
```

The `-tags fts5` requirement carries over — without it the binary builds but
every command fails with the FTS5 error at startup. macOS/Windows binaries
must be built on their own OS (`wails build -tags fts5` there); cross-compiling
the WebKit shell is unsupported.

## Roadmap

- [x] **Phase 1: core directory scanner + SQLite metadata index** ✅
- [x] **Phase 2: keyword search via FTS5** ✅ (filename/path matching)
- [x] **Phase 3: content extraction** ✅ — text from txt, md, pdf
  (`ledongthuc/pdf`, pure Go), docx (stdlib zip+XML). Capped at 50,000 chars
  per file; scanned/image PDFs yield no text (no OCR in this phase)
- [ ] **Phase 3.5: OCR for images/screenshots** — a screenshots-heavy folder
  proves text-only isn't enough
- [x] **Phase 4: local semantic search via embeddings** ✅ — MiniLM-L6-v2
  (quantized ONNX, 23MB) on ONNX Runtime, CPU-only, brute-force cosine over
  SQLite BLOBs. First run downloads model + runtime once (~50MB total),
  then fully offline
- [x] **Phase 5: smart organization suggestions** ✅ — dry-run previews only
  (greedy leader clustering over stored vectors + type folders + 180-day
  stale rule); explicit confirmation before any move/rename
- [x] **Phase 6: live file watching / incremental re-indexing** ✅ —
  `delve watch` (fsnotify, recursive, 500ms debounce, shared scan pipeline)
- [x] **Phase 7: interactive TUI browser** ✅ — bare `delve` (bubbletea:
  debounced live search, Tab keyword/semantic, read-only detail panel)
- [x] **Phase 8: desktop GUI via Wails** ✅ — `gui/` (separate `delve-gui`
  binary sharing `internal/`; React frontend; Organize view is
  select-only, Apply disabled until a move phase) — see below
- [ ] **Phase 3.5: OCR for images/screenshots** — a screenshots-heavy folder
  proves text-only isn't enough
- [ ] **Phase 7 follow-ups: tags, saved searches, duplicate detection**
  (falls out of the content-hash groundwork)
