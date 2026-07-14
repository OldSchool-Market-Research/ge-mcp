// ge-mcp: read-only MCP server exposing GE market domain tools to the
// ge-agent directive loop. stdio transport (DESIGN §2); one SELECT-only
// pgxpool to the ge-data Timescale DB. Never writes, no SQL passthrough.
package main

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mark3labs/mcp-go/server"

	"github.com/osrs-ge/ge-mcp/internal/tools"
)

const version = "0.1.0"

func main() {
	// stdio transport: stdout is the protocol channel, so all logging must go
	// to stderr (log's default).
	log.SetPrefix("ge-mcp: ")

	dsn := os.Getenv("GE_MCP_DSN")
	if dsn == "" {
		log.Fatal("GE_MCP_DSN not set (postgresql://ge-mcp:<pw>@host:5432/ge-data)")
	}

	pool, err := newPool(context.Background(), dsn)
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	defer pool.Close()

	s := server.NewMCPServer("ge-mcp", version, server.WithToolCapabilities(false))
	s.AddTool(tools.NewLookupItemTool(), tools.LookupItemHandler(pool))
	s.AddTool(tools.NewQuoteTool(), tools.QuoteHandler(pool))
	s.AddTool(tools.NewItemHistoryTool(), tools.ItemHistoryHandler(pool))
	s.AddTool(tools.NewLiquidityTool(), tools.LiquidityHandler(pool))

	if err := server.ServeStdio(s); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

func newPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	// The agent is single-threaded-ish (DESIGN §7).
	cfg.MaxConns = 4
	// Belt-and-braces on top of the role's default_transaction_read_only=on.
	cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	// Point-tool backstop; windowed/research tools will override per-query
	// (DESIGN §4: no scan-length cap for legitimate long lookbacks).
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = "30s"

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}

	// Health check: fail fast at startup instead of on the first tool call.
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}
