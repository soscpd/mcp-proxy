package main

import (
	"context"
	"encoding/json"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

const maxDiscoverResults = 10

// mutexRequest is the JSON input accepted by the mutex tool.
type mutexRequest struct {
	Mode    string         `json:"mode"`
	Query   string         `json:"query,omitempty"`   // discover
	Handler string         `json:"handler,omitempty"` // dispatch, alias
	Args    map[string]any `json:"args,omitempty"`    // dispatch, alias
	JobID   string         `json:"job_id,omitempty"`  // status
	ChunkID string         `json:"chunk_id,omitempty"` // fetch
	Name    string         `json:"name,omitempty"`    // alias
}

// mutexResponse is the consistent envelope returned by all modes.
type mutexResponse struct {
	OK    bool   `json:"ok"`
	Mode  string `json:"mode"`
	Data  any    `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}

// MutexTool is the single-tool interface that wraps the entire router.
type MutexTool struct {
	registry *Registry
	queue    *JobQueue
}

// NewMutexTool creates a new mutex tool backed by the given registry and queue.
func NewMutexTool(registry *Registry, queue *JobQueue) *MutexTool {
	return &MutexTool{
		registry: registry,
		queue:    queue,
	}
}

// Tool returns the MCP tool definition for mutex.
func (m *MutexTool) Tool() mcp.Tool {
	return mcp.NewTool("mutex",
		mcp.WithDescription("Single entry point for discovery, dispatch, status, fetch, alias, and reload of all capabilities in this environment."),
		mcp.WithString("mode", mcp.Required(), mcp.Description("One of: discover, dispatch, status, fetch, alias, reload")),
		mcp.WithString("query", mcp.Description("Keywords for discover mode")),
		mcp.WithString("handler", mcp.Description("Handler name for dispatch or alias mode")),
		mcp.WithObject("args", mcp.Description("Arguments for dispatch or alias mode")),
		mcp.WithString("job_id", mcp.Description("Job ID for status mode")),
		mcp.WithString("chunk_id", mcp.Description("Chunk ID for fetch mode")),
		mcp.WithString("name", mcp.Description("Alias name for alias mode")),
	)
}

// Handler returns the MCP tool handler function.
func (m *MutexTool) Handler() server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var mr mutexRequest
		argsJSON, err := json.Marshal(req.GetArguments())
		if err != nil {
			return textResult(respond("", false, nil, "invalid arguments")), nil
		}
		if err := json.Unmarshal(argsJSON, &mr); err != nil {
			return textResult(respond("", false, nil, "invalid arguments: "+err.Error())), nil
		}

		sessionID := extractSessionID(ctx)

		var resp mutexResponse
		switch mr.Mode {
		case "discover":
			resp = m.handleDiscover(mr)
		case "dispatch":
			resp = m.handleDispatch(ctx, sessionID, mr)
		case "status":
			resp = m.handleStatus(sessionID, mr)
		case "fetch":
			resp = m.handleFetch(sessionID, mr)
		case "alias":
			resp = m.handleAlias(sessionID, mr)
		case "reload":
			resp = m.handleReload(sessionID)
		default:
			resp = respond(mr.Mode, false, nil, "unknown mode: "+mr.Mode)
		}

		return textResult(resp), nil
	}
}

func (m *MutexTool) handleDiscover(mr mutexRequest) mutexResponse {
	if mr.Query == "" {
		return respond("discover", false, nil, "query is required for discover mode")
	}

	tools := m.registry.GetAllToolInfo()
	results := searchTools(tools, mr.Query, maxDiscoverResults)

	matches := make([]map[string]string, 0, len(results))
	for _, r := range results {
		matches = append(matches, map[string]string{
			"handler":     r.Name,
			"description": r.Description,
			"call":        extractHandlerCallSignature(r.Name),
		})
	}

	data := map[string]any{"matches": matches}
	if len(matches) == 0 {
		data["message"] = "No matches found. Try broader or different keywords."
	}
	return respond("discover", true, data, "")
}

func (m *MutexTool) handleDispatch(ctx context.Context, sessionID string, mr mutexRequest) mutexResponse {
	if mr.Handler == "" {
		return respond("dispatch", false, nil, "handler is required for dispatch mode")
	}

	resolvedHandler := mr.Handler
	resolvedArgs := mr.Args

	// Check if handler is an alias.
	if h, aliasArgs, ok := m.queue.ResolveAlias(sessionID, mr.Handler); ok {
		resolvedHandler = h
		// Merge: dispatch args override alias args.
		merged := make(map[string]any)
		for k, v := range aliasArgs {
			merged[k] = v
		}
		for k, v := range mr.Args {
			merged[k] = v
		}
		resolvedArgs = merged
	}

	// Verify handler exists.
	if !m.registry.HasTool(resolvedHandler) {
		return respond("dispatch", false, nil, "handler not found")
	}

	job := m.queue.Dispatch(ctx, sessionID, resolvedHandler, resolvedArgs)

	return respond("dispatch", true, map[string]any{
		"job_id":  job.ID,
		"handler": resolvedHandler,
		"status":  job.Status,
	}, "")
}

func (m *MutexTool) handleStatus(sessionID string, mr mutexRequest) mutexResponse {
	if mr.JobID == "" {
		return respond("status", false, nil, "job_id is required for status mode")
	}

	job := m.queue.GetJob(sessionID, mr.JobID)
	if job == nil {
		return respond("status", false, nil, "job not found")
	}

	data := map[string]any{
		"job_id": job.ID,
		"status": job.Status,
	}
	if job.Status == JobStatusDone {
		data["chunk_id"] = job.ChunkID
		// Get summary from chunk.
		chunk := m.queue.GetChunk(sessionID, job.ChunkID)
		if chunk != nil {
			data["summary"] = chunk.Summary
		}
	}
	if job.Status == JobStatusFailed {
		data["error"] = job.Error
	}
	return respond("status", true, data, "")
}

func (m *MutexTool) handleFetch(sessionID string, mr mutexRequest) mutexResponse {
	if mr.ChunkID == "" {
		return respond("fetch", false, nil, "chunk_id is required for fetch mode")
	}

	chunk := m.queue.GetChunk(sessionID, mr.ChunkID)
	if chunk == nil {
		return respond("fetch", false, nil, "chunk not found")
	}

	return respond("fetch", true, map[string]any{
		"chunk_id": chunk.ID,
		"content":  chunk.Content,
	}, "")
}

func (m *MutexTool) handleAlias(sessionID string, mr mutexRequest) mutexResponse {
	if mr.Name == "" {
		return respond("alias", false, nil, "name is required for alias mode")
	}
	if mr.Handler == "" {
		return respond("alias", false, nil, "handler is required for alias mode")
	}

	err := m.queue.RegisterAlias(sessionID, &Alias{
		Name:    mr.Name,
		Handler: mr.Handler,
		Args:    mr.Args,
	})
	if err != nil {
		return respond("alias", false, nil, err.Error())
	}

	return respond("alias", true, map[string]any{
		"name": mr.Name,
		"call": "mutex({mode: dispatch, handler: " + mr.Name + "})",
	}, "")
}

func (m *MutexTool) handleReload(sessionID string) mutexResponse {
	tools := m.registry.GetAllToolInfo()
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.Name
	}
	aliasCount := m.queue.GetSessionAliasCount(sessionID)

	return respond("reload", true, map[string]any{
		"handlers": len(tools),
		"aliases":  aliasCount,
		"names":    names,
	}, "")
}

// --- helpers ---

func respond(mode string, ok bool, data any, errMsg string) mutexResponse {
	return mutexResponse{
		OK:    ok,
		Mode:  mode,
		Data:  data,
		Error: errMsg,
	}
}

func textResult(resp mutexResponse) *mcp.CallToolResult {
	b, _ := json.Marshal(resp)
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			mcp.TextContent{
				Type: "text",
				Text: string(b),
			},
		},
	}
}

func extractSessionID(ctx context.Context) string {
	session := server.ClientSessionFromContext(ctx)
	if session != nil {
		return session.SessionID()
	}
	return "_default"
}
