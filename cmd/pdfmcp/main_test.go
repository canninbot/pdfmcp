package main

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"testing"
	"time"

	indexdb "github.com/canninbot/pdfmcp/internal/index"
	"github.com/canninbot/pdfmcp/internal/mcpserver"
	"github.com/canninbot/pdfmcp/internal/testutil"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestMCPStdioDiscoveryAndInvocation exercises the real newline-delimited
// stdin/stdout transport in a subprocess using the current test executable.
func TestMCPStdioDiscoveryAndInvocation(t *testing.T) {
	cfg := testutil.CreateCorpus(t)
	database, err := indexdb.Open(context.Background(), cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Sync(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=TestMCPHelperProcess", "--",
		"serve", "--root", cfg.Root)
	command.Env = append(os.Environ(), "PDFMCP_HELPER_PROCESS=1")
	transport := &mcp.CommandTransport{Command: command, TerminateDuration: 2 * time.Second}
	client := mcp.NewClient(&mcp.Implementation{Name: "stdio-integration-test", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(listed.Tools))
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, mcpserver.ToolNames()) {
		t.Fatalf("unexpected stdio tools: %v", names)
	}
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "corpus_stats", Arguments: map[string]any{},
	})
	if err != nil || result.IsError || len(result.Content) != 1 {
		t.Fatalf("stdio invocation: %+v, %v", result, err)
	}
}

// TestMCPHelperProcess turns the test executable into pdfmcp for the parent.
func TestMCPHelperProcess(t *testing.T) {
	if os.Getenv("PDFMCP_HELPER_PROCESS") != "1" {
		return
	}
	separator := slices.Index(os.Args, "--")
	if separator < 0 || separator+1 >= len(os.Args) {
		os.Exit(2)
	}
	os.Args = append([]string{"pdfmcp"}, os.Args[separator+1:]...)
	main()
	os.Exit(0)
}
