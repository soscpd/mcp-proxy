package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"golang.org/x/sync/errgroup"
)

type MiddlewareFunc func(http.Handler) http.Handler

func chainMiddleware(h http.Handler, middlewares ...MiddlewareFunc) http.Handler {
	for _, mw := range middlewares {
		h = mw(h)
	}
	return h
}

func newAuthMiddleware(tokens []string) MiddlewareFunc {
	tokenSet := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		tokenSet[token] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if len(tokens) != 0 {
				token := r.Header.Get("Authorization")
				token = strings.TrimSpace(strings.TrimPrefix(token, "Bearer "))
				if token == "" {
					http.Error(w, "Unauthorized", http.StatusUnauthorized)
					return
				}
				if _, ok := tokenSet[token]; !ok {
					http.Error(w, "Unauthorized", http.StatusUnauthorized)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

func loggerMiddleware(prefix string) MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			log.Printf("<%s> Request [%s] %s", prefix, r.Method, r.URL.Path)
			next.ServeHTTP(w, r)
		})
	}
}

func recoverMiddleware(prefix string) MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if err := recover(); err != nil {
					log.Printf("<%s> Recovered from panic: %v", prefix, err)
					http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

func startHTTPServer(config *Config) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create a single aggregated MCPServer.
	serverOpts := []server.ServerOption{
		server.WithResourceCapabilities(true, true),
		server.WithRecovery(),
		server.WithToolCapabilities(true),
	}
	if config.McpProxy.Options != nil && config.McpProxy.Options.LogEnabled.OrElse(false) {
		serverOpts = append(serverOpts, server.WithLogging())
	}

	mcpServer := server.NewMCPServer(
		config.McpProxy.Name,
		config.McpProxy.Version,
		serverOpts...,
	)

	info := mcp.Implementation{
		Name: config.McpProxy.Name,
	}

	registry := NewRegistry(info)

	// Create job queue and mutex tool.
	// maxJobsPerSession defaults to 64; configurable via config if needed.
	maxJobs := 64
	queue := NewJobQueue(registry, 1*time.Hour, maxJobs)
	mutexTool := NewMutexTool(registry, queue)

	// Register mutex as the ONLY tool exposed to clients.
	mcpServer.AddTool(mutexTool.Tool(), mutexTool.Handler())

	// When the internal tool catalog changes, notify connected clients
	// so they re-fetch tools/list (which still returns just "mutex",
	// but the model can use reload to see updated handlers).
	registry.SetOnChange(func() {
		mcpServer.SendNotificationToAllClients(
			mcp.MethodNotificationToolsListChanged, nil,
		)
	})

	// Create the MCP protocol handler (SSE or Streamable HTTP).
	var mcpHandler http.Handler
	switch config.McpProxy.Type {
	case MCPServerTypeSSE:
		mcpHandler = server.NewSSEServer(
			mcpServer,
			server.WithBaseURL(config.McpProxy.BaseURL),
		)
	case MCPServerTypeStreamable:
		mcpHandler = server.NewStreamableHTTPServer(
			mcpServer,
			server.WithStateLess(true),
		)
	default:
		return fmt.Errorf("unknown server type: %s", config.McpProxy.Type)
	}

	// Middleware chain.
	middlewares := []MiddlewareFunc{
		recoverMiddleware("mcp"),
	}
	if config.McpProxy.Options != nil {
		if config.McpProxy.Options.LogEnabled.OrElse(false) {
			middlewares = append(middlewares, loggerMiddleware("mcp"))
		}
		if len(config.McpProxy.Options.AuthTokens) > 0 {
			middlewares = append(middlewares, newAuthMiddleware(config.McpProxy.Options.AuthTokens))
		}
	}

	httpMux := http.NewServeMux()
	httpMux.Handle("/", chainMiddleware(mcpHandler, middlewares...))

	mgmtHandler := NewMgmtHandler(registry)
	httpMux.Handle("/mgmt/servers", mgmtHandler)
	httpMux.Handle("/mgmt/servers/", mgmtHandler)

	metricsHandler := NewMetricsHandler(queue, registry)
	httpMux.Handle("/mgmt/metrics", metricsHandler)

	httpServer := &http.Server{
		Addr:    config.McpProxy.Addr,
		Handler: httpMux,
	}

	// Register servers from config concurrently.
	var errorGroup errgroup.Group
	for name, clientConfig := range config.McpServers {
		if clientConfig.Options != nil && clientConfig.Options.Disabled {
			log.Printf("<%s> Disabled", name)
			continue
		}
		errorGroup.Go(func() error {
			log.Printf("<%s> Connecting", name)
			_, regErr := registry.Register(ctx, name, clientConfig)
			if regErr != nil {
				log.Printf("<%s> Failed to register: %v", name, regErr)
				if clientConfig.Options != nil && clientConfig.Options.PanicIfInvalid.OrElse(false) {
					return regErr
				}
				return nil
			}
			log.Printf("<%s> Connected", name)
			return nil
		})
	}

	go func() {
		err := errorGroup.Wait()
		if err != nil {
			log.Fatalf("Failed to register clients: %v", err)
		}
		log.Printf("All clients initialized")
	}()

	go func() {
		log.Printf("Starting %s server", config.McpProxy.Type)
		log.Printf("%s server listening on %s", config.McpProxy.Type, config.McpProxy.Addr)
		hErr := httpServer.ListenAndServe()
		if hErr != nil && !errors.Is(hErr, http.ErrServerClosed) {
			log.Fatalf("Failed to start server: %v", hErr)
		}
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	<-sigChan
	log.Println("Shutdown signal received")

	shutdownCtx, shutdownCancel := context.WithTimeout(ctx, 5*time.Second)
	defer shutdownCancel()

	err := httpServer.Shutdown(shutdownCtx)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
