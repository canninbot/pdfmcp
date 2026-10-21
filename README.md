# pdfmcp

`pdfmcp` is a read-only Model Context Protocol server and CLI for scientific PDFs already extracted by MinerU. It provides FTS5/BM25 search, exact native-Go search, source retrieval, and embedded PDFium WebAssembly rendering in one CGO-free executable. MCP clients can connect through local stdio or a remotely hosted Streamable HTTP URL.

## Setup

The deployment needs the `pdfmcp` executable and the user-owned `pdfs/`
corpus. The SQLite index is created inside the corpus by default.

```text
pdfmcp/
├── pdfmcp
└── pdfs/
    ├── papers.sqlite
    ├── 12345678/
    └── ...
```

### 1. Make the executable runnable

From the deployment directory:

```bash
chmod +x ./pdfmcp
./pdfmcp help
```

The supplied executable is self-contained. Go, PDFium, SQLite, Python, Node.js,
Docker, and `rg` are not required on the machine where it runs.

### 2. Build or update the index

Run this after initially copying the corpus or whenever MinerU files change:

```bash
./pdfmcp index --root ./pdfs
```

This command is incremental. Unchanged papers are not reparsed. To discard and
recreate the derived index:

```bash
./pdfmcp index --root ./pdfs --rebuild
```

If a valid `pdfs/papers.sqlite` was supplied with the corpus, the initial index
command is optional.

### 3. Verify the installation

```bash
./pdfmcp doctor --root ./pdfs
./pdfmcp stats --root ./pdfs
./pdfmcp search "TEAD inhibition" --root ./pdfs
```

`doctor` should report `root_readable`, `index_readable`, `fts5_available`, and
`embedded_pdfium_wasm` as `true`.

### 4. Configure Codex

Codex CLI, the Codex IDE extension, and the ChatGPT desktop app share MCP
configuration for the same Codex host. Use absolute paths because MCP launchers
may not expand `~` or resolve paths from the project directory.

#### Local stdio

The simplest local setup uses the Codex CLI:

```bash
codex mcp add pdfmcp -- \
  /absolute/path/to/pdfmcp/pdfmcp serve \
  --root /absolute/path/to/pdfmcp/pdfs
```

Confirm that Codex saved the server:

```bash
codex mcp get pdfmcp --json
codex mcp list
```

Alternatively, add this directly to `~/.codex/config.toml`, or to a trusted
project's `.codex/config.toml` for project-scoped setup:

```toml
[mcp_servers.pdfmcp]
command = "/absolute/path/to/pdfmcp/pdfmcp"
args = ["serve", "--root", "/absolute/path/to/pdfmcp/pdfs"]
startup_timeout_sec = 20
tool_timeout_sec = 60
enabled = true
```

Start a new Codex session after saving the configuration. In the Codex terminal
UI, run `/mcp` to verify that `pdfmcp` is connected and advertises eight tools.
For the IDE extension or desktop app, restart the client and inspect its
**MCP servers** settings.

To remove the server later:

```bash
codex mcp remove pdfmcp
```

#### Remote Streamable HTTP

On the machine that stores the corpus, start a Streamable HTTP endpoint:

```bash
./pdfmcp serve \
  --transport streamable-http \
  --listen 127.0.0.1:8080 \
  --endpoint /mcp \
  --public-url https://pdf.example.com/mcp \
  --root /absolute/path/to/pdfs
```

Put that listener behind an HTTPS reverse proxy and connect from the local
computer with its public URL:

```bash
codex mcp add pdfmcp --url https://pdf.example.com/mcp
codex mcp get pdfmcp --json
```

The equivalent `~/.codex/config.toml` entry is:

```toml
[mcp_servers.pdfmcp]
url = "https://pdf.example.com/mcp"
startup_timeout_sec = 20
tool_timeout_sec = 60
enabled = true
```

For a private LAN or VPN where TLS terminates elsewhere, bind directly to a
reachable interface and any available port, for example
`--listen 0.0.0.0:9090`, then use `http://SERVER:9090/mcp`. The HTTP transport
currently has no authentication layer. Anyone who can reach the endpoint can
search the index and retrieve
corpus content, including PDFs and images, so restrict access with a firewall,
VPN, or authenticating reverse proxy. Use HTTPS across untrusted networks.

See the official OpenAI documentation for [Codex MCP setup](https://learn.chatgpt.com/docs/extend/mcp#connect-codex-to-an-mcp-server), including Streamable HTTP URL configuration.

### Other MCP clients

For clients that use JSON server configuration, the equivalent stdio entry is:

```json
{
  "mcpServers": {
    "pdfmcp": {
      "command": "/absolute/path/to/pdfmcp/pdfmcp",
      "args": [
        "serve",
        "--root",
        "/absolute/path/to/pdfmcp/pdfs"
      ]
    }
  }
}
```

The client starts `pdfmcp serve` itself; do not separately launch the server
for normal stdio use. Server diagnostics go to stderr, while stdout is
reserved for MCP. URL-based clients instead connect to the separately running
Streamable HTTP server at its configured endpoint.

The default database is `ROOT/papers.sqlite`. The corpus remains authoritative:
the database contains searchable text, metadata, and relative file references,
never PDF or image BLOBs.

## Build from Source

Build with Go 1.26 or newer:

```bash
make current
./dist/pdfmcp help
```

`make` runs tests first, creates release binaries, and leaves the current-system binary at `dist/pdfmcp`. All builds use `CGO_ENABLED=0`. Dependencies are pinned in `go.mod` and `go.sum`:

- Official `github.com/modelcontextprotocol/go-sdk`
- `modernc.org/sqlite` with FTS5
- `github.com/klippa-app/go-pdfium/webassembly`

The PDFium WASM module and Wazero runtime are linked into the executable. No `.wasm`, shared library, browser, external `rg`, Python, Node.js, Docker, or database server is needed at runtime.

## Corpus Layout

Each discovered PMID directory can contain:

```text
pdfs/12345678/
├── 12345678.pdf
├── markdown.md
├── structured_content.json
├── middle_json.json
└── images/
```

The importer supports partial and version-varying MinerU JSON. It prefers structured blocks and falls back to Markdown passages. Unknown coordinate conventions remain unknown; normalized 0–1 boxes and explicitly identified 0–1000 or PDF-point boxes can be canonicalized.

## Basic Usage

```bash
# Incremental synchronization
pdfmcp index --root /data/pdfs

# Full, reproducible rebuild
pdfmcp index --root /data/pdfs --rebuild

# Shared CLI/MCP search implementation
pdfmcp search '"exact phrase" AND treatment' --mode ranked --root /data/pdfs
pdfmcp search 'exact substring' --mode literal --context-lines 2 --root /data/pdfs
pdfmcp search 'TEAD\s+(inhibition|blockade)' --mode regex --scope all --root /data/pdfs
pdfmcp search sin3 --mode literal --scope text --output pmids --root /data/pdfs
pdfmcp search sin3 --mode literal --scope all --output snippets --debug --root /data/pdfs

pdfmcp stats --root /data/pdfs
pdfmcp doctor --root /data/pdfs
```

The server never synchronizes automatically. Use `serve --sync` only when an explicit startup sync is wanted. In stdio mode, stdout carries MCP messages exclusively and diagnostics go to stderr.

To host the same MCP surface over a URL, use:

```bash
pdfmcp serve --transport streamable-http --listen 127.0.0.1:8080 --endpoint /mcp \
  --public-url https://pdf.example.com/mcp --root /data/pdfs
```

The default transport is `stdio`. Streamable HTTP defaults to
`127.0.0.1:8080` and `/mcp`; setting either flag does not change stdio mode.

## MCP Surface

The server advertises exactly eight tools:

1. `search`
2. `get_document`
3. `get_page`
4. `get_figure`
5. `list_documents`
6. `get_document_info`
7. `render_region`
8. `corpus_stats`

`search` is the only search tool. Its `mode` selects ranked FTS5, literal substring, or Go RE2 regex behavior; `scope` selects text, figures, tables, or all canonical content. `output` selects `pmids`, `snippets`, or `full` and defaults to the existing rich `full` response. Optional `debug` output reports the selected internal strategy.

Ranked queries use the BM25 index. Eligible literal and regex queries over
indexed blocks use a document-level, contentless FTS5 trigram prefilter followed
by exact verification. One- and two-character literals and regexes without a
guaranteed literal prerequisite use a correctness-preserving scan. PMID-only
Markdown searches use a bounded native-Go scanner and do not hydrate snippets
or metadata. See [the measured optimization report](benchmarks/REPORT.md) for
query plans, correctness results, and reproducible before/after benchmarks.

Direct resources use these templates:

```text
paper://{pmid}/markdown
paper://{pmid}/structured
paper://{pmid}/middle
paper://{pmid}/pdf
paper://{pmid}/pdf?offset={offset}&length={length}
paper://{pmid}/image/{image}
```

PDFs up to 1 MiB are returned inline. Larger PDFs return a small JSON manifest
with the byte size, SHA-256 digest, actionable `download_url`, preferred
delivery method, and bounded chunk URI. Configure `serve --public-url` with the
externally reachable MCP endpoint to advertise an absolute download URL. When
it is omitted, the manifest returns a URL relative to the configured MCP
endpoint. HTTP-capable clients can avoid base64 entirely with one `GET`, for
example:

```bash
curl -fL 'https://pdf.example.com/mcp?download=39331709' -o 39331709.pdf
```

Chunk reads accept a length of at most 256 KiB and can reconstruct the PDF in
clients that only support MCP resources. PDFs and images are sent only when a
client reads the resource, uses the raw HTTP download, or invokes an
image-returning tool. Some MCP clients do not preview binary resources even
though they can read them.

## Configuration and Limits

- `--root`: corpus directory; defaults to `./pdfs`
- `--database`: SQLite path; defaults to `ROOT/papers.sqlite`
- `serve --transport`: `stdio` (default) or `streamable-http`
- `serve --listen`: HTTP listen address; defaults to `127.0.0.1:8080`
- `serve --endpoint`: HTTP endpoint path; defaults to `/mcp`
- `serve --public-url`: externally reachable HTTP(S) MCP endpoint advertised for direct PDF downloads; defaults to a relative URL
- PMID-only search results: maximum 5,000 per request and 1 MiB per MCP response
- Snippet and full search results: maximum 200 per request
- Document text: maximum 200,000 characters per request
- Existing images: maximum 20 MiB
- Resources: maximum 100 MiB; large PDF chunks are limited to 256 KiB per read
- Render dimensions: 64–4096 pixels; default 1568
- Render DPI: maximum 600; default 140

SQLite uses WAL for indexing and concurrent readers. WAL can temporarily create `papers.sqlite-wal` and `papers.sqlite-shm` sidecars; checkpointing normally removes or truncates them. The durable index is the single SQLite database and can always be rebuilt from the corpus.

Paths are constrained to the configured root, PMID identifiers are numeric strings, image arguments are filenames only, and symlinks resolving outside the corpus are rejected.

## Development

```bash
go fmt ./...
go vet ./...
go test ./...
make release
```

Tests use a generated synthetic MinerU corpus and a tiny valid PDF. They do not call external services.

## License

MIT. See [LICENSE](LICENSE). Third-party dependencies retain their respective licenses; notably Wazero is Apache-2.0 and go-pdfium is MIT licensed.
