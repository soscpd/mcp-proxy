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
	mu               sync.Mutex
	jobs             map[string]*Job
	chunks           map[string]*Chunk
	sessions         map[string]*Session
	registry         *Registry
	chunkTTL         time.Duration
	maxJobsPerSession int // 0 = unlimited
}

// NewJobQueue creates a new job queue backed by the given registry.
func NewJobQueue(registry *Registry, chunkTTL time.Duration, maxJobsPerSession int) *JobQueue {
	if chunkTTL == 0 {
		chunkTTL = 1 * time.Hour
	}
	return &JobQueue{
		jobs:              make(map[string]*Job),
		chunks:            make(map[string]*Chunk),
		sessions:          make(map[string]*Session),
		registry:          registry,
		chunkTTL:          chunkTTL,
		maxJobsPerSession: maxJobsPerSession,
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

// ErrSessionJobLimit is returned when a session exceeds its job queue limit.
var ErrSessionJobLimit = errStr("session job limit reached")

// Dispatch enqueues a job and starts a worker goroutine.
// Returns the Job immediately with status "queued", or an error if
// the session has reached its active job limit.
func (q *JobQueue) Dispatch(ctx context.Context, sessionID, handler string, args map[string]any) (*Job, error) {
	q.mu.Lock()

	_ = q.getOrCreateSession(sessionID)

	// Enforce per-session limit on active (queued + running) jobs.
	if q.maxJobsPerSession > 0 {
		active := 0
		for _, j := range q.jobs {
			if j.SessionID == sessionID && (j.Status == JobStatusQueued || j.Status == JobStatusRunning) {
				active++
			}
		}
		if active >= q.maxJobsPerSession {
			q.mu.Unlock()
			return nil, ErrSessionJobLimit
		}
	}

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
	return job, nil
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

// QueueMetrics holds point-in-time metrics for the job queue.
type QueueMetrics struct {
	Sessions           int            `json:"sessions"`
	MaxJobsPerSession  int            `json:"max_jobs_per_session"`
	JobsByStatus       map[string]int `json:"jobs_by_status"`
	ActiveJobs         int            `json:"active_jobs"`          // queued + running
	PeakSessionActive  int            `json:"peak_session_active"`  // highest active count across sessions
	Chunks             int            `json:"chunks"`
	ChunksExpired      int            `json:"chunks_expired"`       // cleaned up this call
}

// Metrics computes and returns a snapshot of queue state.
// It also lazily cleans up expired chunks.
func (q *JobQueue) Metrics() QueueMetrics {
	q.mu.Lock()
	defer q.mu.Unlock()

	m := QueueMetrics{
		Sessions:          len(q.sessions),
		MaxJobsPerSession: q.maxJobsPerSession,
		JobsByStatus:      make(map[string]int),
	}

	// Count jobs by status and active jobs per session.
	sessionActive := make(map[string]int)
	for _, j := range q.jobs {
		m.JobsByStatus[j.Status]++
		if j.Status == JobStatusQueued || j.Status == JobStatusRunning {
			m.ActiveJobs++
			sessionActive[j.SessionID]++
		}
	}
	for _, count := range sessionActive {
		if count > m.PeakSessionActive {
			m.PeakSessionActive = count
		}
	}

	// Count chunks and clean expired.
	expired := 0
	for id, c := range q.chunks {
		if c.IsExpired() {
			delete(q.chunks, id)
			expired++
		}
	}
	m.Chunks = len(q.chunks)
	m.ChunksExpired = expired

	return m
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
