package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// registerRequest is the JSON body for POST /mgmt/servers.
type registerRequest struct {
	Name          string            `json:"name"`
	URL           string            `json:"url,omitempty"`
	Command       string            `json:"command,omitempty"`
	Args          []string          `json:"args,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	Headers       map[string]string `json:"headers,omitempty"`
	TransportType MCPClientType     `json:"transportType,omitempty"`
	Timeout       time.Duration     `json:"timeout,omitempty"`
}

// registerResponse is returned on successful registration.
type registerResponse struct {
	Name  string   `json:"name"`
	URL   string   `json:"url,omitempty"`
	Tools []string `json:"tools"`
}

// listServersResponse is returned by GET /mgmt/servers.
type listServersResponse struct {
	Servers []ServerInfo `json:"servers"`
}

// errorResponse is returned on errors.
type errorResponse struct {
	Error string `json:"error"`
}

// MgmtHandler provides HTTP handlers for the management API.
type MgmtHandler struct {
	registry *Registry
}

// NewMgmtHandler creates a new management API handler.
func NewMgmtHandler(registry *Registry) *MgmtHandler {
	return &MgmtHandler{registry: registry}
}

// ServeHTTP routes /mgmt/servers requests to the appropriate handler.
func (h *MgmtHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Strip the /mgmt/servers prefix to get the remainder.
	path := strings.TrimPrefix(r.URL.Path, "/mgmt/servers")
	path = strings.TrimPrefix(path, "/")

	switch r.Method {
	case http.MethodGet:
		if path == "" {
			h.handleList(w, r)
		} else {
			http.NotFound(w, r)
		}
	case http.MethodPost:
		if path == "" {
			h.handleRegister(w, r)
		} else {
			http.NotFound(w, r)
		}
	case http.MethodDelete:
		if path != "" {
			h.handleDeregister(w, r, path)
		} else {
			http.NotFound(w, r)
		}
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

func (h *MgmtHandler) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid JSON: " + err.Error()})
		return
	}

	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "name is required"})
		return
	}

	if !validServerName.MatchString(req.Name) {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "name must match [a-zA-Z0-9_-]+"})
		return
	}

	if req.URL == "" && req.Command == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "url or command is required"})
		return
	}

	if h.registry.HasServer(req.Name) {
		writeJSON(w, http.StatusConflict, errorResponse{Error: "server " + req.Name + " already registered"})
		return
	}

	conf := &MCPClientConfigV2{
		TransportType: req.TransportType,
		URL:           req.URL,
		Command:       req.Command,
		Args:          req.Args,
		Env:           req.Env,
		Headers:       req.Headers,
		Timeout:       req.Timeout,
		Options:       &OptionsV2{},
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	tools, err := h.registry.Register(ctx, req.Name, conf)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, errorResponse{Error: err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, registerResponse{
		Name:  req.Name,
		URL:   req.URL,
		Tools: tools,
	})
}

func (h *MgmtHandler) handleDeregister(w http.ResponseWriter, _ *http.Request, name string) {
	h.registry.Deregister(name)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *MgmtHandler) handleList(w http.ResponseWriter, _ *http.Request) {
	servers := h.registry.ListServers()
	writeJSON(w, http.StatusOK, listServersResponse{Servers: servers})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
