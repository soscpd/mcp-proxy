package main

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"sync"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

var validServerName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// registeredServer holds a connected backend MCP server and its tools.
type registeredServer struct {
	Name   string
	Config *MCPClientConfigV2
	Client *client.Client
	Tools  []string // namespaced tool names currently registered
	cancel context.CancelFunc
}

// Registry is a thread-safe in-memory registry of backend MCP servers.
// It manages namespaced tools on a single aggregated MCPServer.
type Registry struct {
	mu        sync.Mutex
	servers   map[string]*registeredServer
	mcpServer *server.MCPServer
	info      mcp.Implementation
}

// NewRegistry creates a new registry backed by the given aggregated MCPServer.
func NewRegistry(mcpServer *server.MCPServer, info mcp.Implementation) *Registry {
	return &Registry{
		servers:   make(map[string]*registeredServer),
		mcpServer: mcpServer,
		info:      info,
	}
}

// namespacedToolName returns "{serverName}.{toolName}".
func namespacedToolName(serverName, toolName string) string {
	return serverName + "." + toolName
}

// Register connects to a backend MCP server, enumerates its tools,
// and adds them (namespaced) to the aggregated MCPServer.
// Returns the list of namespaced tool names on success.
func (r *Registry) Register(ctx context.Context, name string, conf *MCPClientConfigV2) ([]string, error) {
	if !validServerName.MatchString(name) {
		return nil, fmt.Errorf("invalid server name %q: must match [a-zA-Z0-9_-]+", name)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.servers[name]; exists {
		return nil, fmt.Errorf("server %q already registered", name)
	}

	// Create and initialize the MCP client.
	mcpClient, err := newMCPClient(name, conf)
	if err != nil {
		return nil, fmt.Errorf("failed to create client for %q: %w", name, err)
	}

	if mcpClient.needManualStart {
		if err := mcpClient.client.Start(ctx); err != nil {
			return nil, fmt.Errorf("failed to start client for %q: %w", name, err)
		}
	}

	initRequest := mcp.InitializeRequest{}
	initRequest.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initRequest.Params.ClientInfo = r.info
	initRequest.Params.Capabilities = mcp.ClientCapabilities{
		Experimental: make(map[string]any),
	}
	if _, err := mcpClient.client.Initialize(ctx, initRequest); err != nil {
		_ = mcpClient.Close()
		return nil, fmt.Errorf("failed to initialize client for %q: %w", name, err)
	}
	log.Printf("<%s> Successfully initialized MCP client", name)

	// Enumerate tools from the backend.
	tools, err := listAllTools(ctx, mcpClient.client)
	if err != nil {
		_ = mcpClient.Close()
		return nil, fmt.Errorf("failed to list tools for %q: %w", name, err)
	}

	// Add namespaced tools to the aggregated server.
	nsNames := make([]string, 0, len(tools))
	for _, tool := range tools {
		nsName := namespacedToolName(name, tool.Name)
		nsNames = append(nsNames, nsName)
		nsTool := tool
		nsTool.Name = nsName
		r.mcpServer.AddTools(server.ServerTool{
			Tool:    nsTool,
			Handler: makeToolHandler(mcpClient.client, tool.Name),
		})
		log.Printf("<%s> Added tool %s", name, nsName)
	}

	// Set up upstream notification handler for tools/list_changed.
	// Run in a goroutine to avoid deadlock since Register holds r.mu.
	mcpClient.client.OnNotification(func(notification mcp.JSONRPCNotification) {
		if notification.Method == mcp.MethodNotificationToolsListChanged {
			log.Printf("<%s> Received upstream tools/list_changed", name)
			go r.refreshServerTools(context.Background(), name)
		}
	})

	// Create a per-server cancellable context for background goroutines.
	srvCtx, srvCancel := context.WithCancel(context.Background())

	if mcpClient.needPing {
		go mcpClient.startPingTask(srvCtx)
	}

	r.servers[name] = &registeredServer{
		Name:   name,
		Config: conf,
		Client: mcpClient.client,
		Tools:  nsNames,
		cancel: srvCancel,
	}

	log.Printf("<%s> Registered with %d tools", name, len(nsNames))
	return nsNames, nil
}

// Deregister removes a backend server and all its namespaced tools.
func (r *Registry) Deregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	srv, exists := r.servers[name]
	if !exists {
		return
	}

	// Remove all namespaced tools.
	if len(srv.Tools) > 0 {
		r.mcpServer.DeleteTools(srv.Tools...)
		log.Printf("<%s> Removed %d tools", name, len(srv.Tools))
	}

	srv.cancel()
	_ = srv.Client.Close()
	delete(r.servers, name)
	log.Printf("<%s> Deregistered", name)
}

// ListServers returns info about all registered servers.
func (r *Registry) ListServers() []ServerInfo {
	r.mu.Lock()
	defer r.mu.Unlock()

	result := make([]ServerInfo, 0, len(r.servers))
	for _, srv := range r.servers {
		result = append(result, ServerInfo{
			Name:      srv.Name,
			URL:       srv.Config.URL,
			Command:   srv.Config.Command,
			ToolCount: len(srv.Tools),
			Connected: true,
		})
	}
	return result
}

// HasServer checks if a server name is already registered.
func (r *Registry) HasServer(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, exists := r.servers[name]
	return exists
}

// refreshServerTools re-enumerates tools for a single backend server
// and updates the aggregated MCPServer. Called when an upstream
// notifications/tools/list_changed is received.
func (r *Registry) refreshServerTools(ctx context.Context, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	srv, exists := r.servers[name]
	if !exists {
		return
	}

	// Remove old tools.
	if len(srv.Tools) > 0 {
		r.mcpServer.DeleteTools(srv.Tools...)
	}

	// Re-enumerate.
	tools, err := listAllTools(ctx, srv.Client)
	if err != nil {
		log.Printf("<%s> Failed to re-enumerate tools: %v", name, err)
		srv.Tools = nil
		return
	}

	nsNames := make([]string, 0, len(tools))
	for _, tool := range tools {
		nsName := namespacedToolName(name, tool.Name)
		nsNames = append(nsNames, nsName)
		nsTool := tool
		nsTool.Name = nsName
		r.mcpServer.AddTools(server.ServerTool{
			Tool:    nsTool,
			Handler: makeToolHandler(srv.Client, tool.Name),
		})
	}
	srv.Tools = nsNames
	log.Printf("<%s> Refreshed tools: %d tools", name, len(nsNames))
}

// listAllTools paginates through all tools from a backend client.
func listAllTools(ctx context.Context, c *client.Client) ([]mcp.Tool, error) {
	var allTools []mcp.Tool
	req := mcp.ListToolsRequest{}
	for {
		resp, err := c.ListTools(ctx, req)
		if err != nil {
			return nil, err
		}
		if resp == nil || len(resp.Tools) == 0 {
			break
		}
		allTools = append(allTools, resp.Tools...)
		if resp.NextCursor == "" {
			break
		}
		req.Params.Cursor = resp.NextCursor
	}
	return allTools, nil
}

// makeToolHandler creates a handler that strips the namespace prefix
// and forwards the call to the correct backend client.
func makeToolHandler(c *client.Client, originalToolName string) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// Rewrite the tool name back to the original.
		request.Params.Name = originalToolName
		return c.CallTool(ctx, request)
	}
}

// ServerInfo is returned by the management API.
type ServerInfo struct {
	Name      string `json:"name"`
	URL       string `json:"url,omitempty"`
	Command   string `json:"command,omitempty"`
	ToolCount int    `json:"tool_count"`
	Connected bool   `json:"connected"`
}
