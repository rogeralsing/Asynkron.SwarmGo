package session

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/asynkron/Asynkron.SwarmGo/internal/config"
)

// Session represents a swarm run and contains derived paths.
type Session struct {
	ID      string
	Path    string
	Options config.Options
	Created time.Time
}

// New creates a fresh session stored under the system temp directory.
func New(opts config.Options) (*Session, error) {
	id, err := generateID()
	if err != nil {
		return nil, err
	}

	path := filepath.Join(os.TempDir(), "swarmgo", id)
	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, fmt.Errorf("create session dir: %w", err)
	}

	return &Session{
		ID:      id,
		Path:    path,
		Options: opts,
		Created: time.Now(),
	}, nil
}

// WorktreePath returns the path for a worker's git worktree.
func (s *Session) WorktreePath(worker int) string {
	return filepath.Join(s.Path, fmt.Sprintf("wt%d", worker))
}

// WorkerLogPath returns the log file path for a worker.
func (s *Session) WorkerLogPath(worker int) string {
	return filepath.Join(s.Path, fmt.Sprintf("worker%d.log", worker))
}

// SupervisorLogPath returns the log file path for the supervisor.
func (s *Session) SupervisorLogPath() string {
	return filepath.Join(s.Path, "supervisor.log")
}

// CodedSupervisorPath returns the aggregated supervisor JSON path.
func (s *Session) CodedSupervisorPath() string {
	return filepath.Join(s.Path, "coded-supervisor.json")
}

func generateID() (string, error) {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate session id: %w", err)
	}

	timestamp := time.Now().UTC().Format("20060102150405")
	return fmt.Sprintf("%s%s", timestamp, hex.EncodeToString(buf[:])), nil
}
