# Dynamic Server Registration

The proxy supports runtime registration and deregistration of backend MCP
servers through an HTTP management API. Servers registered this way have their
tools namespaced and aggregated into a single MCP endpoint.

## Architecture

Instead of one MCP endpoint per backend server (the original design), the proxy
now runs a **single aggregated MCPServer**. All tools from all backends are
exposed through this one endpoint, namespaced as `{server_name}.{tool_name}`.

When a tool call arrives for `git-mcp.read_file`, the proxy strips the prefix,
identifies `git-mcp` as the target, and forwards the `read_file` call to that
backend.

## Management API

The management API is served on the same port under the `/mgmt` prefix.

### Register a server

```
POST /mgmt/servers
Content-Type: application/json

{
  "name": "git-mcp",
  "url": "http://172.16.4.23:8080/sse"
}
```

**Fields:**

| Field           | Required | Description                                      |
|-----------------|----------|--------------------------------------------------|
| `name`          | Yes      | Unique name, must match `[a-zA-Z0-9_-]+`        |
| `url`           | *        | URL for SSE/Streamable HTTP servers              |
| `command`       | *        | Command for stdio servers                        |
| `args`          | No       | Arguments for stdio servers                      |
| `env`           | No       | Environment variables for stdio servers          |
| `headers`       | No       | HTTP headers for SSE/Streamable HTTP servers     |
| `transportType` | No       | `"sse"`, `"streamable-http"`, or `"stdio"`       |
| `timeout`       | No       | Timeout for streamable HTTP servers              |

\* Either `url` or `command` is required.

**Responses:**

- `200 OK` — server registered, tools enumerated:
  ```json
  {
    "name": "git-mcp",
    "url": "http://172.16.4.23:8080/sse",
    "tools": ["git-mcp.clone", "git-mcp.commit", "git-mcp.push"]
  }
  ```
- `400 Bad Request` — invalid name or missing required fields
- `409 Conflict` — server name already registered
- `422 Unprocessable Entity` — connection or initialization failed

### Deregister a server

```
DELETE /mgmt/servers/{name}
```

Always returns `200 OK` regardless of whether the name exists.

### List registered servers

```
GET /mgmt/servers
```

```json
{
  "servers": [
    {
      "name": "git-mcp",
      "url": "http://172.16.4.23:8080/sse",
      "tool_count": 3,
      "connected": true
    }
  ]
}
```

## Tool Namespacing

Every tool is namespaced as `{server_name}.{tool_name}`:

- `tools/list` returns namespaced names
- `tools/call` accepts namespaced names and routes to the correct backend
- Server names must match `[a-zA-Z0-9_-]+`
- Namespacing prevents collision when two servers expose tools with the same name

## Notifications

The proxy forwards `notifications/tools/list_changed` to all connected MCP
clients when:

1. A new server is registered via the CRUD API
2. A server is deregistered via the CRUD API
3. A backend server sends `notifications/tools/list_changed` upstream

For case 3, the proxy re-enumerates tools from the changed backend, rebuilds
its namespaced entries, and then emits the notification downstream.

## In-Memory Only

The server registry lives in memory. On restart, all dynamically registered
servers are lost — clients must re-register. Servers loaded from the config
file are registered at startup as before.
