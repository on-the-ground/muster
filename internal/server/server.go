// Package server exposes a fixed catalog through the official MCP SDK.
package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/on-the-ground/muster/internal/contract"
	"github.com/on-the-ground/muster/internal/proxy"
	"github.com/on-the-ground/muster/internal/strictjson"
)

func New(c *contract.Catalog, p *proxy.Proxy) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "muster", Version: "0.1.0"}, &mcp.ServerOptions{
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{ListChanged: false}},
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Instructions: "Calls execute registered contracts in fresh agent sessions. Calls are never automatically retried. Cancellation is best-effort process termination, not rollback. The catalog is fixed until restart.",
	})
	for _, t := range c.Tools {
		d := t.Definition
		s.AddTool(&mcp.Tool{Name: d.Name, Description: d.Description, InputSchema: d.InputSchema, OutputSchema: d.OutputSchema}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			r := p.Call(ctx, d.Name, req.Params.Arguments)
			result := &mcp.CallToolResult{Meta: mcp.Meta{"muster/evidence": r.Evidence}}
			if r.Error != "" {
				result.IsError = true
				result.Content = []mcp.Content{&mcp.TextContent{Text: r.Error}}
			} else {
				result.StructuredContent = json.RawMessage(r.Output)
				result.Content = []mcp.Content{&mcp.TextContent{Text: string(r.Output)}}
			}
			return result, nil
		})
	}
	return s
}

// GuardedReader validates each complete JSON-RPC frame before the SDK can
// collapse duplicate names. Bytes, including every number token, pass unchanged.
type GuardedReader struct {
	source  io.ReadCloser
	scanner *bufio.Scanner
	pending []byte
}

func NewGuardedReader(source io.ReadCloser) *GuardedReader {
	scanner := bufio.NewScanner(source)
	scanner.Buffer(make([]byte, 4096), strictjson.MaxBytes+1)
	return &GuardedReader{source: source, scanner: scanner}
}

func (r *GuardedReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(r.pending) == 0 {
		if !r.scanner.Scan() {
			if err := r.scanner.Err(); err != nil {
				return 0, errors.New("invalid or oversized MCP frame")
			}
			return 0, io.EOF
		}
		line := r.scanner.Bytes()
		if _, err := strictjson.Object(line); err != nil {
			return 0, errors.New("invalid MCP JSON: duplicate keys, malformed JSON or limits exceeded")
		}
		r.pending = append(append([]byte{}, line...), '\n')
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}
func (r *GuardedReader) Close() error { return r.source.Close() }
