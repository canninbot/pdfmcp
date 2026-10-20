# AGENTS.md

## Commands

Use Go 1.26 or newer and disable CGO:

```bash
go fmt ./...
CGO_ENABLED=0 go vet ./...
CGO_ENABLED=0 go test ./...
make current
make release
```

For a synthetic or real corpus:

```bash
./dist/pdfmcp index --root ./pdfs
./dist/pdfmcp doctor --root ./pdfs
./dist/pdfmcp search "TEAD inhibition" --root ./pdfs
```

## Architecture Boundaries

- `internal/corpus`: discovery and safe filesystem resolution only.
- `internal/mineru`: version-tolerant MinerU parsing and bbox semantics.
- `internal/index`: SQLite schema, incremental synchronization, and retrieval queries.
- `internal/search`: the single shared ranked/literal/regex implementation.
- `internal/renderer`: lazy embedded PDFium WASM page and crop rendering.
- `internal/mcpserver`: exactly eight read-only MCP tools and `paper://` resources.
- `cmd/pdfmcp`: CLI parsing and stdio startup.

## Editing Constraints

- Do not add another search tool, embeddings, RAG, external search executables, or runtime services.
- Keep MCP handlers read-only. Index mutations belong to the CLI `index` command.
- Keep source paths relative in SQLite and constrain all reads to the corpus root.
- Preserve stdout exclusively for MCP protocol data in `serve` mode.
- Do not infer unknown MinerU bounding-box coordinate systems.
- Keep the deployed artifact to one executable plus the user corpus and its SQLite index.

## Testing Requirements

Run formatting, vet, all tests, a current-system build, and supported cross-builds after dependency or rendering changes. Test both full-page and crop rendering when changing PDFium code. Confirm MCP tool discovery still returns exactly eight names.

## Repository Pitfalls

- The PDFium WASM dependency materially increases link time and executable size.
- `modernc.org/sqlite` must retain FTS5 support.
- PDFium pages are zero-based internally; MCP page parameters and results are one-based.
- WAL can create temporary SQLite sidecars during indexing.
- Most MinerU archives do not necessarily contain the original PDF; `render_region` and the PDF resource report that absence instead of re-extracting or fabricating content.
