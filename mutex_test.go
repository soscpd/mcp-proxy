package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// newTestMutex creates a registry with a backend, a job queue, and a mutex tool.
func newTestMutex(t *testing.T) (*MutexTool, *Registry, func()) {
	t.Helper()
	_, backendURL, shutdown := startBackendServer(t,
		mcp.NewTool("clone", mcp.WithDescription("Clones a remote repository to a local path")),
		mcp.NewTool("fetch", mcp.WithDescription("Fetches from a remote without merging")),
		mcp.NewTool("commit", mcp.WithDescription("Commits staged changes")),
	)

	registry := newTestRegistry()
	_, err := registry.Register(context.Background(), "git-mcp", &MCPClientConfigV2{
		URL:     backendURL,
		Options: &OptionsV2{},
	})
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	queue := NewJobQueue(registry, 1*time.Hour, 0)
	mutex := NewMutexTool(registry, queue)

	return mutex, registry, func() {
		registry.Deregister("git-mcp")
		shutdown()
	}
}

// callMutex invokes the mutex handler and returns the parsed response.
func callMutex(t *testing.T, mutex *MutexTool, args map[string]any) mutexResponse {
	t.Helper()
	req := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "mutex",
			Arguments: args,
		},
	}
	result, err := mutex.Handler()(context.Background(), req)
	if err != nil {
		t.Fatalf("mutex handler error: %v", err)
	}
	if len(result.Content) == 0 {
		t.Fatal("empty result from mutex")
	}
	tc, ok := result.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatal("expected TextContent")
	}
	var resp mutexResponse
	if err := json.Unmarshal([]byte(tc.Text), &resp); err != nil {
		t.Fatalf("failed to parse mutex response: %v\nraw: %s", err, tc.Text)
	}
	return resp
}

// --- discover tests ---

func TestDiscoverWithMatchingKeywords(t *testing.T) {
	mutex, _, cleanup := newTestMutex(t)
	defer cleanup()

	resp := callMutex(t, mutex, map[string]any{
		"mode":  "discover",
		"query": "clone repository",
	})

	if !resp.OK {
		t.Fatalf("expected ok, got error: %s", resp.Error)
	}
	data, ok := resp.Data.(map[string]any)
	if !ok {
		t.Fatal("expected data to be a map")
	}
	matches, ok := data["matches"].([]any)
	if !ok || len(matches) == 0 {
		t.Fatal("expected at least one match")
	}
	first := matches[0].(map[string]any)
	if first["handler"] != "git-mcp.clone" {
		t.Fatalf("expected git-mcp.clone as top result, got %v", first["handler"])
	}
}

func TestDiscoverWithNoMatches(t *testing.T) {
	mutex, _, cleanup := newTestMutex(t)
	defer cleanup()

	resp := callMutex(t, mutex, map[string]any{
		"mode":  "discover",
		"query": "xyzzynonexistent",
	})

	if !resp.OK {
		t.Fatalf("expected ok even with no matches, got error: %s", resp.Error)
	}
	data := resp.Data.(map[string]any)
	matches := data["matches"].([]any)
	if len(matches) != 0 {
		t.Fatalf("expected empty matches, got %d", len(matches))
	}
}

// --- dispatch tests ---

func TestDispatchWithValidHandler(t *testing.T) {
	mutex, _, cleanup := newTestMutex(t)
	defer cleanup()

	resp := callMutex(t, mutex, map[string]any{
		"mode":    "dispatch",
		"handler": "git-mcp.clone",
		"args":    map[string]any{"repo_url": "http://example.com/repo.git"},
	})

	if !resp.OK {
		t.Fatalf("expected ok, got error: %s", resp.Error)
	}
	data := resp.Data.(map[string]any)
	if data["status"] != "queued" {
		t.Fatalf("expected queued status, got %v", data["status"])
	}
	if data["job_id"] == nil || data["job_id"] == "" {
		t.Fatal("expected job_id")
	}
}

func TestDispatchWithUnknownHandler(t *testing.T) {
	mutex, _, cleanup := newTestMutex(t)
	defer cleanup()

	resp := callMutex(t, mutex, map[string]any{
		"mode":    "dispatch",
		"handler": "nonexistent.tool",
	})

	if resp.OK {
		t.Fatal("expected ok: false for unknown handler")
	}
	if resp.Error != "handler not found" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}
}

func TestDispatchWithAlias(t *testing.T) {
	mutex, _, cleanup := newTestMutex(t)
	defer cleanup()

	// Register alias.
	callMutex(t, mutex, map[string]any{
		"mode":    "alias",
		"name":    "quick_clone",
		"handler": "git-mcp.clone",
		"args":    map[string]any{"repo_url": "http://example.com/repo.git"},
	})

	// Dispatch using alias.
	dispResp := callMutex(t, mutex, map[string]any{
		"mode":    "dispatch",
		"handler": "quick_clone",
	})
	if !dispResp.OK {
		t.Fatalf("dispatch with alias failed: %s", dispResp.Error)
	}
	data := dispResp.Data.(map[string]any)
	if data["handler"] != "git-mcp.clone" {
		t.Fatalf("expected resolved handler git-mcp.clone, got %v", data["handler"])
	}
}

// --- status tests ---

func TestStatusDoneWithChunkAndSummary(t *testing.T) {
	mutex, _, cleanup := newTestMutex(t)
	defer cleanup()

	dispResp := callMutex(t, mutex, map[string]any{
		"mode":    "dispatch",
		"handler": "git-mcp.clone",
	})
	jobID := dispResp.Data.(map[string]any)["job_id"].(string)

	// Wait for completion.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		statusResp := callMutex(t, mutex, map[string]any{
			"mode":   "status",
			"job_id": jobID,
		})
		data := statusResp.Data.(map[string]any)
		if data["status"] == "done" {
			if data["chunk_id"] == nil || data["chunk_id"] == "" {
				t.Fatal("expected chunk_id")
			}
			if data["summary"] == nil {
				t.Fatal("expected summary")
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("job did not complete in time")
}

func TestStatusFailed(t *testing.T) {
	errBackend := server.NewMCPServer("err-backend", "1.0",
		server.WithToolCapabilities(true),
	)
	errBackend.AddTool(mcp.NewTool("fail_tool", mcp.WithDescription("Always fails")),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return nil, errStr("intentional failure")
		})
	ts := server.NewTestServer(errBackend)
	defer ts.Close()

	registry := newTestRegistry()
	_, err := registry.Register(context.Background(), "err-svc", &MCPClientConfigV2{
		URL: ts.URL + "/sse", Options: &OptionsV2{},
	})
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	defer registry.Deregister("err-svc")

	queue := NewJobQueue(registry, 1*time.Hour, 0)
	mutex := NewMutexTool(registry, queue)

	dispResp := callMutex(t, mutex, map[string]any{
		"mode":    "dispatch",
		"handler": "err-svc.fail_tool",
	})
	jobID := dispResp.Data.(map[string]any)["job_id"].(string)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		statusResp := callMutex(t, mutex, map[string]any{
			"mode":   "status",
			"job_id": jobID,
		})
		data := statusResp.Data.(map[string]any)
		if data["status"] == "failed" {
			if data["error"] == nil || data["error"] == "" {
				t.Fatal("expected error message")
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("timeout waiting for job to fail")
}

func TestStatusRunning(t *testing.T) {
	backend, backendURL, shutdown := startBackendServer(t,
		mcp.NewTool("slow_tool", mcp.WithDescription("A slow tool")),
	)
	backend.DeleteTools("slow_tool")
	backend.AddTool(mcp.NewTool("slow_tool", mcp.WithDescription("A slow tool")),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			time.Sleep(2 * time.Second)
			return &mcp.CallToolResult{
				Content: []mcp.Content{mcp.TextContent{Type: "text", Text: "done slowly"}},
			}, nil
		})

	registry := newTestRegistry()
	_, err := registry.Register(context.Background(), "slow-svc", &MCPClientConfigV2{
		URL: backendURL, Options: &OptionsV2{},
	})
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	defer func() {
		registry.Deregister("slow-svc")
		shutdown()
	}()

	queue := NewJobQueue(registry, 1*time.Hour, 0)
	mutex := NewMutexTool(registry, queue)

	dispResp := callMutex(t, mutex, map[string]any{
		"mode":    "dispatch",
		"handler": "slow-svc.slow_tool",
	})
	jobID := dispResp.Data.(map[string]any)["job_id"].(string)

	time.Sleep(200 * time.Millisecond)
	statusResp := callMutex(t, mutex, map[string]any{
		"mode":   "status",
		"job_id": jobID,
	})
	if !statusResp.OK {
		t.Fatalf("expected ok, got error: %s", statusResp.Error)
	}
	status := statusResp.Data.(map[string]any)["status"].(string)
	if status != "running" && status != "queued" {
		t.Fatalf("expected running or queued, got %s", status)
	}
}

// --- fetch tests ---

func TestFetchValidChunk(t *testing.T) {
	mutex, _, cleanup := newTestMutex(t)
	defer cleanup()

	dispResp := callMutex(t, mutex, map[string]any{
		"mode":    "dispatch",
		"handler": "git-mcp.clone",
	})
	jobID := dispResp.Data.(map[string]any)["job_id"].(string)

	var chunkID string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		statusResp := callMutex(t, mutex, map[string]any{
			"mode":   "status",
			"job_id": jobID,
		})
		data := statusResp.Data.(map[string]any)
		if data["status"] == "done" {
			chunkID = data["chunk_id"].(string)
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if chunkID == "" {
		t.Fatal("job did not complete in time")
	}

	fetchResp := callMutex(t, mutex, map[string]any{
		"mode":     "fetch",
		"chunk_id": chunkID,
	})
	if !fetchResp.OK {
		t.Fatalf("expected ok, got error: %s", fetchResp.Error)
	}
	data := fetchResp.Data.(map[string]any)
	if data["content"] == nil || data["content"] == "" {
		t.Fatal("expected content")
	}
}

func TestFetchUnknownChunk(t *testing.T) {
	mutex, _, cleanup := newTestMutex(t)
	defer cleanup()

	resp := callMutex(t, mutex, map[string]any{
		"mode":     "fetch",
		"chunk_id": "c_000000",
	})
	if resp.OK {
		t.Fatal("expected ok: false for unknown chunk")
	}
}

func TestFetchCrossSessionDenied(t *testing.T) {
	_, backendURL, shutdown := startBackendServer(t,
		mcp.NewTool("tool1", mcp.WithDescription("Tool 1")),
	)
	defer shutdown()

	registry := newTestRegistry()
	_, err := registry.Register(context.Background(), "svc1", &MCPClientConfigV2{
		URL: backendURL, Options: &OptionsV2{},
	})
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	defer registry.Deregister("svc1")

	queue := NewJobQueue(registry, 1*time.Hour, 0)

	job, _ := queue.Dispatch(context.Background(), "s1", "svc1.tool1", nil)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		j := queue.GetJob("s1", job.ID)
		if j != nil && j.Status == JobStatusDone {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	j := queue.GetJob("s1", job.ID)
	if j == nil || j.Status != JobStatusDone {
		t.Fatal("job did not complete")
	}

	// Cross-session access denied.
	chunk := queue.GetChunk("s2", j.ChunkID)
	if chunk != nil {
		t.Fatal("expected cross-session chunk access to be denied")
	}

	// Same-session access works.
	chunk = queue.GetChunk("s1", j.ChunkID)
	if chunk == nil {
		t.Fatal("expected chunk to be accessible from owning session")
	}
}

// --- alias tests ---

func TestAliasCollidingName(t *testing.T) {
	mutex, _, cleanup := newTestMutex(t)
	defer cleanup()

	// Alias name matches existing handler (contains dot, but let's try exact match).
	resp := callMutex(t, mutex, map[string]any{
		"mode":    "alias",
		"name":    "git-mcp.clone",
		"handler": "git-mcp.fetch",
	})
	if resp.OK {
		t.Fatal("expected ok: false for colliding alias name")
	}
}

// --- reload tests ---

func TestReload(t *testing.T) {
	mutex, _, cleanup := newTestMutex(t)
	defer cleanup()

	resp := callMutex(t, mutex, map[string]any{
		"mode": "reload",
	})
	if !resp.OK {
		t.Fatalf("expected ok, got error: %s", resp.Error)
	}
	data := resp.Data.(map[string]any)
	handlers := int(data["handlers"].(float64))
	if handlers != 3 {
		t.Fatalf("expected 3 handlers, got %d", handlers)
	}
	names := data["names"].([]any)
	if len(names) != 3 {
		t.Fatalf("expected 3 names, got %d", len(names))
	}
}

// --- expired chunk test ---

func TestExpiredChunkNotReturned(t *testing.T) {
	_, backendURL, shutdown := startBackendServer(t,
		mcp.NewTool("tool1", mcp.WithDescription("Tool 1")),
	)
	defer shutdown()

	registry := newTestRegistry()
	_, err := registry.Register(context.Background(), "svc1", &MCPClientConfigV2{
		URL: backendURL, Options: &OptionsV2{},
	})
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	defer registry.Deregister("svc1")

	queue := NewJobQueue(registry, 100*time.Millisecond, 0)
	job, _ := queue.Dispatch(context.Background(), "s1", "svc1.tool1", nil)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		j := queue.GetJob("s1", job.ID)
		if j != nil && j.Status == JobStatusDone {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	j := queue.GetJob("s1", job.ID)
	if j == nil || j.Status != JobStatusDone {
		t.Fatal("job did not complete")
	}

	time.Sleep(200 * time.Millisecond)

	chunk := queue.GetChunk("s1", j.ChunkID)
	if chunk != nil {
		t.Fatal("expected expired chunk to be nil")
	}
}

// --- job goroutine calls correct backend ---

func TestJobCallsCorrectBackend(t *testing.T) {
	mutex, _, cleanup := newTestMutex(t)
	defer cleanup()

	// Dispatch clone.
	dispResp := callMutex(t, mutex, map[string]any{
		"mode":    "dispatch",
		"handler": "git-mcp.clone",
	})
	jobID := dispResp.Data.(map[string]any)["job_id"].(string)

	// Wait for completion and fetch content.
	var chunkID string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		statusResp := callMutex(t, mutex, map[string]any{
			"mode":   "status",
			"job_id": jobID,
		})
		data := statusResp.Data.(map[string]any)
		if data["status"] == "done" {
			chunkID = data["chunk_id"].(string)
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if chunkID == "" {
		t.Fatal("job did not complete")
	}

	fetchResp := callMutex(t, mutex, map[string]any{
		"mode":     "fetch",
		"chunk_id": chunkID,
	})
	content := fetchResp.Data.(map[string]any)["content"].(string)
	if content != "called clone" {
		t.Fatalf("expected 'called clone', got %q", content)
	}
}

// --- session job limit tests ---

func TestDispatchRejectsWhenSessionLimitReached(t *testing.T) {
	// Create a slow backend so jobs stay active.
	backend, backendURL, shutdown := startBackendServer(t,
		mcp.NewTool("slow", mcp.WithDescription("Slow")),
	)
	backend.DeleteTools("slow")
	backend.AddTool(mcp.NewTool("slow", mcp.WithDescription("Slow")),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			time.Sleep(5 * time.Second)
			return &mcp.CallToolResult{
				Content: []mcp.Content{mcp.TextContent{Type: "text", Text: "done"}},
			}, nil
		})
	defer shutdown()

	registry := newTestRegistry()
	_, err := registry.Register(context.Background(), "slow-svc", &MCPClientConfigV2{
		URL: backendURL, Options: &OptionsV2{},
	})
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	defer registry.Deregister("slow-svc")

	// Limit to 2 active jobs per session.
	queue := NewJobQueue(registry, 1*time.Hour, 2)
	mutex := NewMutexTool(registry, queue)

	// Fill the limit.
	for i := 0; i < 2; i++ {
		resp := callMutex(t, mutex, map[string]any{
			"mode":    "dispatch",
			"handler": "slow-svc.slow",
		})
		if !resp.OK {
			t.Fatalf("dispatch %d failed: %s", i, resp.Error)
		}
	}

	// Wait for jobs to start running.
	time.Sleep(200 * time.Millisecond)

	// Third dispatch should be rejected.
	resp := callMutex(t, mutex, map[string]any{
		"mode":    "dispatch",
		"handler": "slow-svc.slow",
	})
	if resp.OK {
		t.Fatal("expected dispatch to be rejected when session limit reached")
	}
	if resp.Error != "session job limit reached" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}
}

// --- metrics tests ---

func TestMetricsReturnsQueueState(t *testing.T) {
	_, backendURL, shutdown := startBackendServer(t,
		mcp.NewTool("tool1", mcp.WithDescription("Tool 1")),
	)
	defer shutdown()

	registry := newTestRegistry()
	_, err := registry.Register(context.Background(), "svc1", &MCPClientConfigV2{
		URL: backendURL, Options: &OptionsV2{},
	})
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	defer registry.Deregister("svc1")

	queue := NewJobQueue(registry, 1*time.Hour, 10)

	// Dispatch a job and wait for completion.
	job, _ := queue.Dispatch(context.Background(), "s1", "svc1.tool1", nil)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		j := queue.GetJob("s1", job.ID)
		if j != nil && j.Status == JobStatusDone {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	m := queue.Metrics()

	if m.Sessions != 1 {
		t.Fatalf("expected 1 session, got %d", m.Sessions)
	}
	if m.MaxJobsPerSession != 10 {
		t.Fatalf("expected max 10, got %d", m.MaxJobsPerSession)
	}
	if m.JobsByStatus[JobStatusDone] != 1 {
		t.Fatalf("expected 1 done job, got %d", m.JobsByStatus[JobStatusDone])
	}
	if m.Chunks != 1 {
		t.Fatalf("expected 1 chunk, got %d", m.Chunks)
	}
}

func TestMetricsHTTPEndpoint(t *testing.T) {
	_, backendURL, shutdown := startBackendServer(t,
		mcp.NewTool("tool1", mcp.WithDescription("Tool 1")),
	)
	defer shutdown()

	registry := newTestRegistry()
	_, err := registry.Register(context.Background(), "svc1", &MCPClientConfigV2{
		URL: backendURL, Options: &OptionsV2{},
	})
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	defer registry.Deregister("svc1")

	queue := NewJobQueue(registry, 1*time.Hour, 10)
	handler := NewMetricsHandler(queue, registry)

	// Prometheus format.
	req, _ := http.NewRequest("GET", "/mgmt/metrics", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "mcpeto_sessions_total") {
		t.Fatal("expected mcpeto_sessions_total in prometheus output")
	}
	if !strings.Contains(body, "mcpeto_jobs_per_session_limit") {
		t.Fatal("expected mcpeto_jobs_per_session_limit in prometheus output")
	}
	if !strings.Contains(body, "mcpeto_handlers_total") {
		t.Fatal("expected mcpeto_handlers_total in prometheus output")
	}

	// JSON format.
	req2, _ := http.NewRequest("GET", "/mgmt/metrics?format=json", nil)
	rr2 := httptest.NewRecorder()
	handler.ServeHTTP(rr2, req2)

	if rr2.Code != 200 {
		t.Fatalf("expected 200, got %d", rr2.Code)
	}
	var jsonResp map[string]any
	if err := json.NewDecoder(rr2.Body).Decode(&jsonResp); err != nil {
		t.Fatalf("failed to decode JSON: %v", err)
	}
	if jsonResp["max_jobs_per_session"] != float64(10) {
		t.Fatalf("expected max_jobs_per_session=10, got %v", jsonResp["max_jobs_per_session"])
	}
}
