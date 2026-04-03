# mcpeto

MCP tool aggregator with a single-tool interface. Proxies multiple MCP backend
servers behind one endpoint, exposing a unified `mutex` tool that handles
discovery, async dispatch, result retrieval, aliasing, and handler reload.

Built for environments where an LLM agent (e.g. nanobot) needs access to a
fleet of MCP servers without seeing hundreds of individual tool schemas.

## How it works

```
nanobot ──SSE──▶ mcpeto (mutex tool) ──▶ backend MCP servers
                      │
                      ├── discover: BM25 search over all handlers
                      ├── dispatch: enqueue job, return immediately
                      ├── status:   poll job, get summary + chunk_id
                      ├── fetch:    retrieve full output by chunk_id
                      ├── alias:    register shortcuts
                      └── reload:   refresh handler list
```

Backends are registered at startup via config or at runtime via the
`/mgmt/servers` CRUD API. Tools are namespaced as `{server_name}.{tool_name}`.

## Quick start

```bash
git clone https://github.com/soscpd/mcpeto.git
cd mcpeto
make build
./build/mcpeto --config config.json
```

### Go install

```bash
go install github.com/soscpd/mcpeto@latest
mcpeto --config config.json
```

### Docker

```bash
docker run -v $(pwd)/config.json:/config/config.json \
  registry.skull.everyof.net/mcpeto:latest
```

### Helm

```bash
helm install mcpeto charts/mcpeto
```

## Management API

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/mgmt/servers` | Register a backend MCP server |
| `DELETE` | `/mgmt/servers/{name}` | Deregister a server |
| `GET` | `/mgmt/servers` | List registered servers |
| `GET` | `/mgmt/metrics` | Prometheus metrics |
| `GET` | `/mgmt/metrics?format=json` | JSON metrics |

## Metrics

Key signals for autoscaling:

- `mcpeto_jobs_active` — queued + running jobs
- `mcpeto_jobs_per_session_peak` — highest active count per session
- `mcpeto_jobs_per_session_limit` — configured cap (default 64)

## Configuration

See [docs/CONFIGURATION.md](docs/CONFIGURATION.md) for the config file format.

See [docs/dynamic-registration.md](docs/dynamic-registration.md) for the runtime
registration API.

See [docs/system-prompt.md](docs/system-prompt.md) for the model system prompt.

## License

[MIT](LICENSE)

---

*Originally forked from [TBXark/mcp-proxy](https://github.com/TBXark/mcp-proxy).*
