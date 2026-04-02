package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// Job status constants.
const (
	JobStatusQueued  = "queued"
	JobStatusRunning = "running"
	JobStatusDone    = "done"
	JobStatusFailed  = "failed"
)

// Job represents a dispatched tool call.
type Job struct {
	ID          string
	SessionID   string
	Handler     string
	Args        map[string]any
	Status      string
	CreatedAt   time.Time
	CompletedAt *time.Time
	ChunkID     string // set when done
	Error       string // set when failed
}

// Chunk stores the output of a completed job.
type Chunk struct {
	ID        string
	JobID     string
	SessionID string
	Content   string
	Summary   string
	CreatedAt time.Time
	TTL       time.Duration
}

// IsExpired returns true if the chunk has exceeded its TTL.
func (c *Chunk) IsExpired() bool {
	return time.Since(c.CreatedAt) > c.TTL
}

// Session holds per-client state: aliases, job references, chunk references.
type Session struct {
	ID      string
	Aliases map[string]*Alias // name -> alias
}

// Alias is a named shortcut for a handler + partial args.
type Alias struct {
	Name    string
	Handler string
	Args    map[string]any
}

// JobQueue manages jobs, chunks, and sessions.
type JobQueue struct {
	mu       sync.Mutex
	jobs     map[string]*Job
	chunks   map[string]*Chunk
	sessions map[string]*Session
	registry *Registry
	chunkTTL time.Duration
}

// NewJobQueue creates a new job queue backed by the given registry.
func NewJobQueue(registry *Registry, chunkTTL time.Duration) *JobQueue {
	if chunkTTL == 0 {
		chunkTTL = 1 * time.Hour
	}
	return &JobQueue{
		jobs:     make(map[string]*Job),
		chunks:   make(map[string]*Chunk),
		sessions: make(map[string]*Session),
		registry: registry,
		chunkTTL: chunkTTL,
	}
}

// getOrCreateSession returns the session for the given ID, creating it if needed.
func (q *JobQueue) getOrCreateSession(sessionID string) *Session {
	s, ok := q.sessions[sessionID]
	if !ok {
		s = &Session{
			ID:      sessionID,
			Aliases: make(map[string]*Alias),
		}
		q.sessions[sessionID] = s
	}
	return s
}

// Dispatch enqueues a job and starts a worker goroutine.
// Returns the Job immediately with status "queued".
func (q *JobQueue) Dispatch(ctx context.Context, sessionID, handler string, args map[string]any) *Job {
	q.mu.Lock()

	_ = q.getOrCreateSession(sessionID)

	job := &Job{
		ID:        genID("j"),
		SessionID: sessionID,
		Handler:   handler,
		Args:      args,
		Status:    JobStatusQueued,
		CreatedAt: time.Now(),
	}
	q.jobs[job.ID] = job
	q.mu.Unlock()

	go q.runJob(ctx, job)
	return job
}

// runJob executes the tool call and writes the result.
func (q *JobQueue) runJob(ctx context.Context, job *Job) {
	q.mu.Lock()
	job.Status = JobStatusRunning
	q.mu.Unlock()

	handler := q.registry.GetToolHandler(job.Handler)
	if handler == nil {
		q.mu.Lock()
		now := time.Now()
		job.Status = JobStatusFailed
		job.Error = "handler not found"
		job.CompletedAt = &now
		q.mu.Unlock()
		return
	}

	result, err := handler(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      job.Handler,
			Arguments: job.Args,
		},
	})

	q.mu.Lock()
	defer q.mu.Unlock()

	now := time.Now()
	job.CompletedAt = &now

	if err != nil {
		job.Status = JobStatusFailed
		job.Error = err.Error()
		return
	}

	content := extractContent(result)
	chunk := &Chunk{
		ID:        genID("c"),
		JobID:     job.ID,
		SessionID: job.SessionID,
		Content:   content,
		Summary:   makeSummary(content),
		CreatedAt: now,
		TTL:       q.chunkTTL,
	}
	q.chunks[chunk.ID] = chunk
	job.Status = JobStatusDone
	job.ChunkID = chunk.ID
}

// GetJob returns a job by ID, scoped to the session.
func (q *JobQueue) GetJob(sessionID, jobID string) *Job {
	q.mu.Lock()
	defer q.mu.Unlock()

	job, ok := q.jobs[jobID]
	if !ok || job.SessionID != sessionID {
		return nil
	}
	return job
}

// GetChunk returns a chunk by ID, scoped to the session.
func (q *JobQueue) GetChunk(sessionID, chunkID string) *Chunk {
	q.mu.Lock()
	defer q.mu.Unlock()

	chunk, ok := q.chunks[chunkID]
	if !ok || chunk.SessionID != sessionID {
		return nil
	}
	if chunk.IsExpired() {
		delete(q.chunks, chunkID)
		return nil
	}
	return chunk
}

// RegisterAlias creates a named shortcut in the session namespace.
func (q *JobQueue) RegisterAlias(sessionID string, a *Alias) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	if !validServerName.MatchString(a.Name) {
		return errInvalidAliasName
	}

	// Check collision with existing handlers.
	if q.registry.HasTool(a.Name) {
		return errAliasCollision
	}

	sess := q.getOrCreateSession(sessionID)
	if _, exists := sess.Aliases[a.Name]; exists {
		return errAliasCollision
	}

	sess.Aliases[a.Name] = a
	return nil
}

// ResolveAlias looks up an alias in the session, returning the underlying
// handler and merged args. Returns nil if not found.
func (q *JobQueue) ResolveAlias(sessionID, name string) (handler string, args map[string]any, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	sess, exists := q.sessions[sessionID]
	if !exists {
		return "", nil, false
	}
	a, exists := sess.Aliases[name]
	if !exists {
		return "", nil, false
	}
	return a.Handler, a.Args, true
}

// GetSessionAliasCount returns the number of aliases for a session.
func (q *JobQueue) GetSessionAliasCount(sessionID string) int {
	q.mu.Lock()
	defer q.mu.Unlock()

	sess, exists := q.sessions[sessionID]
	if !exists {
		return 0
	}
	return len(sess.Aliases)
}

// --- helpers ---

func genID(prefix string) string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}

// extractContent serialises a CallToolResult's content into a single string.
func extractContent(result *mcp.CallToolResult) string {
	if result == nil {
		return ""
	}
	var parts []string
	for _, c := range result.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// makeSummary returns the first non-empty line, truncated to 120 chars.
func makeSummary(content string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) > 120 {
			return line[:120] + "..."
		}
		return line
	}
	return ""
}

// sentinel errors
var (
	errInvalidAliasName = errStr("alias name must match [a-zA-Z0-9_-]+")
	errAliasCollision   = errStr("alias name collides with existing handler or alias")
)

type errStr string

func (e errStr) Error() string { return string(e) }

// extractHandlerCallSignature returns a brief calling instruction for a handler.
func extractHandlerCallSignature(handler string) string {
	return "mutex({mode: dispatch, handler: " + handler + ", args: {...}})"
}
