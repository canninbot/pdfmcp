// Command pdfmcp indexes and serves a local MinerU PDF corpus.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/canninbot/pdfmcp/internal/config"
	indexdb "github.com/canninbot/pdfmcp/internal/index"
	"github.com/canninbot/pdfmcp/internal/mcpserver"
	"github.com/canninbot/pdfmcp/internal/search"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const usage = `pdfmcp - local MinerU PDF search MCP

Usage:
  pdfmcp index --root /data/pdfs [--database PATH] [--rebuild]
  pdfmcp serve --root /data/pdfs [--database PATH] [--sync]
  pdfmcp search "TEAD inhibition" --root /data/pdfs [search options]
  pdfmcp stats --root /data/pdfs [--database PATH]
  pdfmcp doctor --root /data/pdfs [--database PATH]
`

func main() {
	log.SetOutput(os.Stderr)
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx := context.Background()
	var err error
	switch os.Args[1] {
	case "index":
		err = runIndex(ctx, os.Args[2:])
	case "serve":
		err = runServe(ctx, os.Args[2:])
	case "search":
		err = runSearch(ctx, os.Args[2:])
	case "stats":
		err = runStats(ctx, os.Args[2:])
	case "doctor":
		err = runDoctor(ctx, os.Args[2:])
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "pdfmcp: %v\n", err)
		os.Exit(1)
	}
}

func commonFlags(name string) (*flag.FlagSet, *string, *string) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	root := flags.String("root", "./pdfs", "MinerU corpus root")
	database := flags.String("database", "", "SQLite index path (default ROOT/papers.sqlite)")
	return flags, root, database
}

func parseConfig(root, database string) (config.Config, error) {
	return config.New(root, database)
}

func runIndex(ctx context.Context, arguments []string) error {
	flags, root, database := commonFlags("index")
	rebuild := flags.Bool("rebuild", false, "fully rebuild the index")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	cfg, err := parseConfig(*root, *database)
	if err != nil {
		return err
	}
	db, err := indexdb.Open(ctx, cfg, false)
	if err != nil {
		return err
	}
	defer db.Close()
	progress, err := db.Sync(ctx, *rebuild)
	if encodeErr := writeJSON(progress); err == nil {
		err = encodeErr
	}
	return err
}

func runServe(ctx context.Context, arguments []string) error {
	flags, root, database := commonFlags("serve")
	syncIndex := flags.Bool("sync", false, "incrementally synchronize the index before serving")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	cfg, err := parseConfig(*root, *database)
	if err != nil {
		return err
	}
	if *syncIndex {
		db, err := indexdb.Open(ctx, cfg, false)
		if err != nil {
			return err
		}
		progress, syncErr := db.Sync(ctx, false)
		db.Close()
		if syncErr != nil {
			return syncErr
		}
		log.Printf("index sync: indexed=%d unchanged=%d deleted=%d failed=%d",
			progress.Indexed, progress.Unchanged, progress.Deleted, progress.Failed)
	}
	service, err := mcpserver.New(ctx, cfg)
	if err != nil {
		return err
	}
	defer service.Close()
	// Stdout is reserved exclusively for MCP JSON-RPC framing.
	return service.Server().Run(ctx, &mcp.StdioTransport{})
}

func runSearch(ctx context.Context, arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("search query is required")
	}
	query := ""
	if arguments[0] != "" && arguments[0][0] != '-' {
		query = arguments[0]
		arguments = arguments[1:]
	}
	flags, root, database := commonFlags("search")
	mode := flags.String("mode", "ranked", "ranked, literal, or regex")
	scope := flags.String("scope", "text", "text, figures, tables, or all")
	caseSensitive := flags.Bool("case-sensitive", false, "enable case-sensitive exact matching")
	contextLines := flags.Int("context-lines", 0, "surrounding context lines")
	limit := flags.Int("limit", 20, "result limit")
	offset := flags.Int("offset", 0, "result offset")
	output := flags.String("output", "full", "pmids, snippets, or full")
	debug := flags.Bool("debug", false, "include query-planner diagnostics")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if query == "" && flags.NArg() > 0 {
		query = flags.Arg(0)
	}
	cfg, err := parseConfig(*root, *database)
	if err != nil {
		return err
	}
	db, err := indexdb.Open(ctx, cfg, true)
	if err != nil {
		return err
	}
	defer db.Close()
	response, err := search.New(db).Search(ctx, search.Options{
		Query: query, Mode: *mode, Scope: *scope, CaseSensitive: *caseSensitive,
		ContextLines: *contextLines, Limit: *limit, Offset: *offset, Output: *output,
		Debug: *debug,
	})
	if err != nil {
		return err
	}
	return writeJSON(response)
}

func runStats(ctx context.Context, arguments []string) error {
	flags, root, database := commonFlags("stats")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	cfg, err := parseConfig(*root, *database)
	if err != nil {
		return err
	}
	db, err := indexdb.Open(ctx, cfg, true)
	if err != nil {
		return err
	}
	defer db.Close()
	stats, err := db.CorpusStats(ctx)
	if err != nil {
		return err
	}
	return writeJSON(stats)
}

func runDoctor(ctx context.Context, arguments []string) error {
	flags, root, database := commonFlags("doctor")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	cfg, err := parseConfig(*root, *database)
	if err != nil {
		return err
	}
	checks := map[string]any{"root": cfg.Root, "database": cfg.Database}
	rootInfo, rootErr := os.Stat(cfg.Root)
	checks["root_readable"] = rootErr == nil && rootInfo.IsDir()
	if rootErr != nil {
		checks["root_error"] = rootErr.Error()
	}
	db, err := indexdb.Open(ctx, cfg, true)
	if err != nil {
		checks["index_readable"] = false
		checks["index_error"] = err.Error()
		_ = writeJSON(checks)
		return err
	}
	defer db.Close()
	checks["index_readable"] = true
	var ftsVersion string
	ftsErr := db.SQL().QueryRowContext(ctx, `SELECT fts5('version')`).Scan(&ftsVersion)
	if ftsErr != nil {
		// Creating and querying the virtual table is the authoritative FTS5 check.
		var count int
		ftsErr = db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM fts_blocks`).Scan(&count)
	}
	checks["fts5_available"] = ftsErr == nil
	stats, statsErr := db.CorpusStats(ctx)
	if statsErr == nil {
		checks["stats"] = stats
	}
	checks["embedded_pdfium_wasm"] = true
	checks["checked_at"] = time.Now().UTC().Format(time.RFC3339)
	checks["database_wal_note"] = "WAL mode may create temporary -wal and -shm sidecar files during indexing."
	if err := writeJSON(checks); err != nil {
		return err
	}
	return ftsErr
}

func writeJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
