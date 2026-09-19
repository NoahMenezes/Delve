# Delve

A local-first, AI-powered file browser CLI. Delve indexes your files locally
and lets you search by meaning — entirely offline. No account, no
subscription, no cloud calls ever.

Inspired by intelligent file browsers like [Poly](https://poly.app), minus the
cloud: everything on your machine, in a single binary.

## Core principles

- **100% local-first** — no network calls except downloading the embedding
  model once on first run; everything else runs offline.
- **Single, dependency-free binary** — download one file, run it, works
  everywhere. No Python, Node, or other language runtime required.
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
```

## Usage (so far)

```bash
delve scan [path] [--verbose] [--extract-content=true]  # index metadata + text (default: current dir)
delve search <query> [--limit 20] [--extension pdf]     # keyword search names/paths/content
```

The index lives at `~/.delve/delve.db` (plain SQLite — inspect it with the
`sqlite3` CLI any time).

## Roadmap

- [x] **Phase 1: core directory scanner + SQLite metadata index** ✅
- [x] **Phase 2: keyword search via FTS5** ✅ (filename/path matching)
- [x] **Phase 3: content extraction** ✅ — text from txt, md, pdf
  (`ledongthuc/pdf`, pure Go), docx (stdlib zip+XML). Capped at 50,000 chars
  per file; scanned/image PDFs yield no text (no OCR in this phase)
- [ ] **Phase 3.5: OCR for images/screenshots** — a screenshots-heavy folder
  proves text-only isn't enough
- [ ] **Phase 4: local semantic search via embeddings** — offline,
  meaning-based search (pure-Go / Go-native ONNX runtime only)
- [ ] **Phase 5: smart organization suggestions** — dry-run previews only,
  explicit confirmation before any move/rename
- [ ] **Phase 6: live file watching / incremental re-indexing**
- [ ] **Phase 7: tags, saved searches, duplicate detection** (falls out of the
  content-hash groundwork)
- Later, optional: TUI, desktop GUI — out of scope for now.
