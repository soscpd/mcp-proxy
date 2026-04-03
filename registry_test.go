package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// startBackendServer creates a real MCP SSE server with the given tools
// and returns the backend MCPServer, its URL, and a shutdown function.
func startBackendServer(t *testing.T, tools ...mcp.Tool) (*server.MCPServer, string, func()) {
	t.Helper()
	backend := server.NewMCPServer("test-backend", "1.0",
		server.WithToolCapabilities(true),
	)
	for _, tool := range tools {
		toolName := tool.Name
		backend.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{
					mcp.TextContent{
						Type: "text",
						Text: fmt.Sprintf("called %s", toolName),
					},
				},
			}, nil
		})
	}
	ts := server.NewTestServer(backend)
	return backend, ts.URL + "/sse", func() {
		ts.Close()
	}
}

// newTestRegistry creates a test Registry.
func newTestRegistry() *Registry {
	info := mcp.Implementation{Name: "test-proxy"}
	return NewRegistry(info)
}

func TestRegisterAddsNamespacedTools(t *testing.T) {
	_, backendURL, shutdown := startBackendServer(t,
		mcp.NewTool("read_file", mcp.WithDescription("Read a file")),
		mcp.NewTool("write_file", mcp.WithDescription("Write a file")),
	)

	registry := newTestRegistry()
	ctx := context.Background()

	tools, err := registry.Register(ctx, "git-mcp", &MCPClientConfigV2{
		URL:     backendURL,
		Options: &OptionsV2{},
	})
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}
	toolSet := make(map[string]bool)
	for _, name := range tools {
		toolSet[name] = true
	}
	if !toolSet["git-mcp.read_file"] || !toolSet["git-mcp.write_file"] {
		t.Fatalf("unexpected tools: %v", tools)
	}

	// Verify tools are in the internal catalog.
	allTools := registry.GetAllToolInfo()
	found := make(map[string]bool)
	for _, ti := range allTools {
		found[ti.Name] = true
	}
	if !found["git-mcp.read_file"] || !found["git-mcp.write_file"] {
		t.Error("tools not found in internal catalog")
	}

	registry.Deregister("git-mcp")
	shutdown()
}

func TestRegisterDuplicateReturnsError(t *testing.T) {
	_, backendURL, shutdown := startBackendServer(t,
		mcp.NewTool("do_thing", mcp.WithDescription("Do a thing")),
	)

	registry := newTestRegistry()
	ctx := context.Background()

	conf := &MCPClientConfigV2{URL: backendURL, Options: &OptionsV2{}}
	_, err := registry.Register(ctx, "my-server", conf)
	if err != nil {
		t.Fatalf("first register failed: %v", err)
	}

	_, err = registry.Register(ctx, "my-server", conf)
	if err == nil {
		t.Fatal("expected error for duplicate registration")
	}
	if !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("unexpected error: %v", err)
	}

	registry.Deregister("my-server")
	shutdown()
}

func TestDeregisterRemovesTools(t *testing.T) {
	_, backendURL, shutdown := startBackendServer(t,
		mcp.NewTool("search", mcp.WithDescription("Search")),
	)

	registry := newTestRegistry()
	ctx := context.Background()

	_, err := registry.Register(ctx, "search-svc", &MCPClientConfigV2{
		URL:     backendURL,
		Options: &OptionsV2{},
	})
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	if !registry.HasTool("search-svc.search") {
		t.Fatal("tool not found after registration")
	}

	registry.Deregister("search-svc")

	if registry.HasTool("search-svc.search") {
		t.Fatal("tool still present after deregistration")
	}

	shutdown()
}

func TestDeregisterNonExistentReturnsOk(t *testing.T) {
	registry := newTestRegistry()
	registry.Deregister("nonexistent")
}

func TestToolCallRoutesToCorrectBackend(t *testing.T) {
	_, backendURL, shutdown := startBackendServer(t,
		mcp.NewTool("greet", mcp.WithDescription("Greet"),
			mcp.WithString("name", mcp.Description("Name to greet"))),
	)

	registry := newTestRegistry()
	ctx := context.Background()

	_, err := registry.Register(ctx, "hello-svc", &MCPClientConfigV2{
		URL:     backendURL,
		Options: &OptionsV2{},
	})
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	handler := registry.GetToolHandler("hello-svc.greet")
	if handler == nil {
		t.Fatal("handler not found for hello-svc.greet")
	}

	result, err := handler(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "hello-svc.greet",
			Arguments: map[string]any{"name": "world"},
		},
	})
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	if len(result.Content) == 0 {
		t.Fatal("empty result")
	}
	tc, ok := result.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatal("expected TextContent")
	}
	if tc.Text != "called greet" {
		t.Fatalf("unexpected result: %s", tc.Text)
	}

	registry.Deregister("hello-svc")
	shutdown()
}

func TestUnknownToolReturnsNil(t *testing.T) {
	registry := newTestRegistry()
	handler := registry.GetToolHandler("nonexistent.tool")
	if handler != nil {
		t.Fatal("expected nil for unknown tool")
	}
}

func TestInvalidServerNameRejected(t *testing.T) {
	registry := newTestRegistry()
	ctx := context.Background()

	_, err := registry.Register(ctx, "bad name!", &MCPClientConfigV2{
		URL:     "http://localhost:1234",
		Options: &OptionsV2{},
	})
	if err == nil {
		t.Fatal("expected error for invalid name")
	}
	if !strings.Contains(err.Error(), "invalid server name") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// --- CRUD API tests ---

func newTestMgmtServer(t *testing.T) (*httptest.Server, *Registry, func()) {
	t.Helper()
	registry := newTestRegistry()
	mgmt := NewMgmtHandler(registry)

	mux := http.NewServeMux()
	mux.Handle("/mgmt/servers", mgmt)
	mux.Handle("/mgmt/servers/", mgmt)
	ts := httptest.NewServer(mux)
	return ts, registry, func() { ts.Close() }
}

func TestCRUDRegisterAndList(t *testing.T) {
	_, backendURL, shutdownBackend := startBackendServer(t,
		mcp.NewTool("clone", mcp.WithDescription("Clone a repo")),
	)

	ts, registry, shutdownMgmt := newTestMgmtServer(t)
	defer shutdownMgmt()

	body := fmt.Sprintf(`{"name":"git-mcp","url":"%s"}`, backendURL)
	resp, err := http.Post(ts.URL+"/mgmt/servers", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(b))
	}

	var regResp registerResponse
	if err := json.NewDecoder(resp.Body).Decode(&regResp); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if regResp.Name != "git-mcp" {
		t.Fatalf("expected name git-mcp, got %s", regResp.Name)
	}
	if len(regResp.Tools) != 1 || regResp.Tools[0] != "git-mcp.clone" {
		t.Fatalf("unexpected tools: %v", regResp.Tools)
	}

	resp2, err := http.Get(ts.URL + "/mgmt/servers")
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	defer resp2.Body.Close()

	var listResp listServersResponse
	if err := json.NewDecoder(resp2.Body).Decode(&listResp); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if len(listResp.Servers) != 1 {
		t.Fatalf("expected 1 server, got %d", len(listResp.Servers))
	}

	registry.Deregister("git-mcp")
	shutdownBackend()
}

func TestCRUDDuplicateReturns409(t *testing.T) {
	_, backendURL, shutdownBackend := startBackendServer(t,
		mcp.NewTool("tool1", mcp.WithDescription("Tool 1")),
	)

	ts, registry, shutdownMgmt := newTestMgmtServer(t)
	defer shutdownMgmt()

	body := fmt.Sprintf(`{"name":"dup-svc","url":"%s"}`, backendURL)
	resp, err := http.Post(ts.URL+"/mgmt/servers", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("first register expected 200, got %d", resp.StatusCode)
	}

	resp2, err := http.Post(ts.URL+"/mgmt/servers", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 409 {
		t.Fatalf("expected 409, got %d", resp2.StatusCode)
	}

	registry.Deregister("dup-svc")
	shutdownBackend()
}

func TestCRUDDeregister(t *testing.T) {
	_, backendURL, shutdownBackend := startBackendServer(t,
		mcp.NewTool("tool1", mcp.WithDescription("Tool 1")),
	)

	ts, registry, shutdownMgmt := newTestMgmtServer(t)
	defer shutdownMgmt()

	body := fmt.Sprintf(`{"name":"del-svc","url":"%s"}`, backendURL)
	resp, err := http.Post(ts.URL+"/mgmt/servers", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	resp.Body.Close()

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/mgmt/servers/del-svc", nil)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE failed: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp2.StatusCode)
	}

	if registry.HasServer("del-svc") {
		t.Fatal("server still registered after DELETE")
	}

	shutdownBackend()
}

func TestCRUDDeleteNonExistentReturns200(t *testing.T) {
	ts, _, shutdownMgmt := newTestMgmtServer(t)
	defer shutdownMgmt()

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/mgmt/servers/ghost", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestOnChangeCalledOnRegister(t *testing.T) {
	_, backendURL, shutdownBackend := startBackendServer(t,
		mcp.NewTool("tool1", mcp.WithDescription("Tool 1")),
	)

	registry := newTestRegistry()
	called := make(chan struct{}, 2)
	registry.SetOnChange(func() {
		select {
		case called <- struct{}{}:
		default:
		}
	})

	_, err := registry.Register(context.Background(), "notify-svc", &MCPClientConfigV2{
		URL:     backendURL,
		Options: &OptionsV2{},
	})
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("onChange not called on register")
	}

	registry.Deregister("notify-svc")

	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("onChange not called on deregister")
	}

	shutdownBackend()
}

func TestNotificationSentViaSSE(t *testing.T) {
	_, backendURL, shutdownBackend := startBackendServer(t,
		mcp.NewTool("tool1", mcp.WithDescription("Tool 1")),
	)

	aggServer := server.NewMCPServer("test-proxy", "1.0",
		server.WithToolCapabilities(true),
	)
	registry := newTestRegistry()
	registry.SetOnChange(func() {
		aggServer.SendNotificationToAllClients(
			mcp.MethodNotificationToolsListChanged, nil,
		)
	})

	ts := server.NewTestServer(aggServer)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	proxyClient, err := client.NewSSEMCPClient(ts.URL + "/sse")
	if err != nil {
		t.Fatalf("Failed to create proxy client: %v", err)
	}
	defer proxyClient.Close()

	if err := proxyClient.Start(ctx); err != nil {
		t.Fatalf("Failed to start proxy client: %v", err)
	}

	notifCh := make(chan struct{}, 1)
	proxyClient.OnNotification(func(notification mcp.JSONRPCNotification) {
		if notification.Method == mcp.MethodNotificationToolsListChanged {
			select {
			case notifCh <- struct{}{}:
			default:
			}
		}
	})

	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "test-client"}
	if _, err := proxyClient.Initialize(ctx, initReq); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	_, regErr := registry.Register(ctx, "notify-svc", &MCPClientConfigV2{
		URL:     backendURL,
		Options: &OptionsV2{},
	})
	if regErr != nil {
		t.Fatalf("Register failed: %v", regErr)
	}

	select {
	case <-notifCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for tools/list_changed notification")
	}

	registry.Deregister("notify-svc")
	shutdownBackend()
}

func TestUpstreamToolsListChangedTriggersRefresh(t *testing.T) {
	backend := server.NewMCPServer("dynamic-backend", "1.0",
		server.WithToolCapabilities(true),
	)
	backend.AddTool(mcp.NewTool("initial_tool", mcp.WithDescription("Initial")),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{mcp.TextContent{Type: "text", Text: "initial"}},
			}, nil
		})

	ts := server.NewTestServer(backend)
	defer ts.Close()

	registry := newTestRegistry()

	tools, err := registry.Register(context.Background(), "dynamic-svc", &MCPClientConfigV2{
		URL:     ts.URL + "/sse",
		Options: &OptionsV2{},
	})
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	if len(tools) != 1 || tools[0] != "dynamic-svc.initial_tool" {
		t.Fatalf("unexpected initial tools: %v", tools)
	}

	backend.AddTool(mcp.NewTool("new_tool", mcp.WithDescription("New")),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{mcp.TextContent{Type: "text", Text: "new"}},
			}, nil
		})

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if registry.HasTool("dynamic-svc.new_tool") {
			registry.Deregister("dynamic-svc")
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	registry.Deregister("dynamic-svc")
	t.Fatal("timeout waiting for upstream tools/list_changed to propagate")
}
