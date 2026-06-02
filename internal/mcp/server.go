// Package mcp implements a read-only MCP (Model Context Protocol) HTTP server
// that exposes Orca artifacts to external clients. It is the only package that
// speaks the MCP wire protocol; all core packages remain MCP-unaware.
//
// The server uses the JSON-RPC 2.0 protocol over plain HTTP POST.
// It is read-only: no tool may write to the store or event log.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/micronwave/orca/internal/schema"
)

// StoreReader is the narrow read-only artifact store surface the MCP server needs.
type StoreReader interface {
	LoadActiveGoal(ctx context.Context) (*schema.GoalIR, error)
	LoadGoal(ctx context.Context, goalID string) (*schema.GoalIR, error)
	LoadOpenObligations(ctx context.Context, goalID string) ([]*schema.Obligation, error)
	LoadCapsule(ctx context.Context, capsuleID string) (*schema.ExecutionCapsule, error)
	LoadLatestRuntimeStatus(ctx context.Context, capsuleID string) (*schema.CapsuleRuntimeEvent, error)
	LoadPatch(ctx context.Context, patchID string) (*schema.PatchArtifact, error)
	LoadEvidenceForObligation(ctx context.Context, obligationID string) ([]*schema.EvidenceArtifact, error)
	LoadVerifierResultForPatch(ctx context.Context, patchID string) (*schema.VerifierResult, error)
	LoadBudgetForGoal(ctx context.Context, goalID string) ([]*schema.BudgetRecord, error)
}

// LogReader is the narrow read-only event log surface the MCP server needs.
type LogReader interface {
	ReadForGoal(ctx context.Context, goalID string, afterSeq int64, limit int) ([]schema.Event, error)
}

// Server is a read-only MCP HTTP server. Create with New; start with ListenAndServe.
type Server struct {
	store   StoreReader
	log     LogReader
	workDir string // project root for read-only git operations
	srv     *http.Server
}

// New creates a Server backed by st and log. workDir is the project root used
// for read-only git tools; pass "" to disable git tools. No goroutines are started.
func New(st StoreReader, log LogReader, workDir ...string) *Server {
	var wdir string
	if len(workDir) > 0 {
		wdir = workDir[0]
	}
	s := &Server{store: st, log: log, workDir: wdir}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRPC)
	s.srv = &http.Server{Handler: mux}
	return s
}

// Serve accepts connections on ln and serves MCP requests until ctx is
// cancelled or a fatal listener error occurs. It is safe to call from a
// goroutine. Callers that need to detect bind errors synchronously should
// call net.Listen themselves and pass the result here.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	errCh := make(chan error, 1)
	go func() { errCh <- s.srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutCtx)
		return nil
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return fmt.Errorf("mcp: server: %w", err)
	}
}

// ListenAndServe binds to addr and serves MCP requests until ctx is cancelled
// or a fatal listener error occurs. It is safe to call from a goroutine.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("mcp: listen %s: %w", addr, err)
	}
	return s.Serve(ctx, ln)
}

// JSON-RPC 2.0 wire types.

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *Server) handleRPC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeRPCError(w, nil, -32700, "parse error")
		return
	}
	// Notifications (no ID) require no response.
	if len(req.ID) == 0 || string(req.ID) == "null" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	result, rpcErr := s.dispatch(r.Context(), req.Method, req.Params)
	if rpcErr != nil {
		writeRPCError(w, req.ID, rpcErr.Code, rpcErr.Message)
		return
	}
	writeRPCResult(w, req.ID, result)
}

func (s *Server) dispatch(ctx context.Context, method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case "initialize":
		return handleInitialize()
	case "tools/list":
		return handleToolsList()
	case "tools/call":
		return s.handleToolsCall(ctx, params)
	case "ping":
		return map[string]any{}, nil
	default:
		return nil, &rpcError{Code: -32601, Message: "method not found: " + method}
	}
}

func handleInitialize() (any, *rpcError) {
	return map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities": map[string]any{
			"tools": map[string]any{},
		},
		"serverInfo": map[string]any{
			"name":    "orca",
			"version": "1.0.0",
		},
	}, nil
}

func handleToolsList() (any, *rpcError) {
	return map[string]any{
		"tools": toolDefinitions(),
	}, nil
}

type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func (s *Server) handleToolsCall(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var p toolCallParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid params"}
	}
	text, err := s.callTool(ctx, p.Name, p.Arguments)
	if err != nil {
		return toolErrorContent(err.Error()), nil
	}
	return toolTextContent(text), nil
}

func toolTextContent(text string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
	}
}

func toolErrorContent(msg string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": msg}},
		"isError": true,
	}
}

// ServeHTTP implements http.Handler so Server can be used with httptest.NewServer in tests.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.srv.Handler.ServeHTTP(w, r)
}

func writeRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	resp := rpcResponse{JSONRPC: "2.0", ID: id, Result: result}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	resp := rpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &rpcError{Code: code, Message: msg},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
