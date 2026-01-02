package supervisor

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/asynkron/Asynkron.SwarmGo/internal/agents"
	"github.com/asynkron/Asynkron.SwarmGo/internal/config"
)

// CodedSupervisor collects lightweight signals from worker worktrees and logs, writing them
// to a JSON file the supervisor agent can read.
type CodedSupervisor struct {
	outputPath string
	workers    []workerInfo
	interval   time.Duration

	mu     sync.Mutex
	state  map[int]*workerState
	offset map[int]int64

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type workerInfo struct {
	Number   int
	Worktree string
	LogPath  string
	CLI      agents.CLI
}

type workerState struct {
	Git         gitSnapshot
	Logs        []logEvent
	LastUpdated time.Time
}

type gitSnapshot struct {
	Branch        string       `json:"branch"`
	Staged        []fileChange `json:"staged"`
	Unstaged      []fileChange `json:"unstaged"`
	Untracked     []string     `json:"untracked"`
	RecentCommits []string     `json:"recentCommits"`
	Error         string       `json:"error,omitempty"`
	UpdatedAt     time.Time    `json:"updatedAt"`
}

type fileChange struct {
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
	File    string `json:"file"`
}

type logEvent struct {
	Timestamp time.Time `json:"timestamp"`
	Kind      string    `json:"kind"`
	Message   string    `json:"message"`
}

type logSummary struct {
	LastPass *logEvent  `json:"lastPass,omitempty"`
	LastFail *logEvent  `json:"lastFail,omitempty"`
	Recent   []logEvent `json:"recent"`
}

type workerSnapshot struct {
	WorkerNumber int         `json:"workerNumber"`
	Git          gitSnapshot `json:"git"`
	Logs         logSummary  `json:"logs"`
	LastUpdated  time.Time   `json:"lastUpdated"`
}

type snapshot struct {
	UpdatedAt time.Time        `json:"updatedAt"`
	Workers   []workerSnapshot `json:"workers"`
}

var (
	passRegex = regexp.MustCompile(`(?i)\b(pass(ed)?|success|succeeded|ok|all tests passed|tests passed)\b`)
	failRegex = regexp.MustCompile(`(?i)\b(fail(ed)?|error|exception|traceback|stacktrace|panic|assert|test[s]? failed)\b`)
)

const (
	defaultInterval = 5 * time.Second
	maxLogEvents    = 50
)

// NewCodedSupervisor returns a background collector. Call Start to begin polling, and Close to stop.
func NewCodedSupervisor(outputPath string, worktrees []string, workerLogs []string, agentTypes []config.AgentType, interval time.Duration) *CodedSupervisor {
	if interval <= 0 {
		interval = defaultInterval
	}

	limit := len(worktrees)
	if len(workerLogs) < limit {
		limit = len(workerLogs)
	}
	if len(agentTypes) < limit {
		limit = len(agentTypes)
	}

	workers := make([]workerInfo, 0, limit)
	for i := 0; i < limit; i++ {
		cli := agents.NewCLI(agentTypes[i])
		workers = append(workers, workerInfo{
			Number:   i + 1,
			Worktree: worktrees[i],
			LogPath:  workerLogs[i],
			CLI:      cli,
		})
	}

	ctx, cancel := context.WithCancel(context.Background())

	state := make(map[int]*workerState)
	offsets := make(map[int]int64)
	for _, w := range workers {
		state[w.Number] = &workerState{LastUpdated: time.Now()}
		offsets[w.Number] = 0
	}

	return &CodedSupervisor{
		outputPath: outputPath,
		workers:    workers,
		interval:   interval,
		state:      state,
		offset:     offsets,
		ctx:        ctx,
		cancel:     cancel,
	}
}

// Start begins the background polling loop.
func (c *CodedSupervisor) Start() {
	c.wg.Add(1)
	go c.loop()
}

// Close stops the polling loop and writes a final snapshot.
func (c *CodedSupervisor) Close() {
	c.cancel()
	c.wg.Wait()
	_ = c.writeSnapshot()
}

func (c *CodedSupervisor) loop() {
	defer c.wg.Done()

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			c.pollOnce()
		}
	}
}

func (c *CodedSupervisor) pollOnce() {
	for _, w := range c.workers {
		c.collectGit(w)
		c.collectLogs(w)
	}
	_ = c.writeSnapshot()
}

func (c *CodedSupervisor) collectGit(w workerInfo) {
	ctx, cancel := context.WithTimeout(c.ctx, 10*time.Second)
	defer cancel()

	snap := gitSnapshot{UpdatedAt: time.Now()}

	if out, err := runGit(ctx, w.Worktree, "rev-parse --abbrev-ref HEAD"); err == nil {
		snap.Branch = strings.TrimSpace(out)
	} else {
		snap.Error = err.Error()
		c.storeGit(w.Number, snap)
		return
	}

	staged, err := runGit(ctx, w.Worktree, "diff --cached --numstat")
	if err == nil {
		snap.Staged = parseNumstat(staged)
	}
	unstaged, err := runGit(ctx, w.Worktree, "diff --numstat")
	if err == nil {
		snap.Unstaged = parseNumstat(unstaged)
	}
	untracked, err := runGit(ctx, w.Worktree, "ls-files --others --exclude-standard")
	if err == nil {
		snap.Untracked = splitLines(untracked)
	}
	commits, err := runGit(ctx, w.Worktree, "log --oneline -5")
	if err == nil {
		snap.RecentCommits = splitLines(commits)
	}

	c.storeGit(w.Number, snap)
}

func (c *CodedSupervisor) storeGit(worker int, snap gitSnapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.state[worker]; ok {
		s.Git = snap
		s.LastUpdated = time.Now()
	}
}

func (c *CodedSupervisor) collectLogs(w workerInfo) {
	data, newOffset, err := c.readNewLogData(w)
	if err != nil || len(data) == 0 {
		return
	}
	lines := strings.Split(data, "\n")

	c.mu.Lock()
	defer c.mu.Unlock()

	state, ok := c.state[w.Number]
	if !ok {
		return
	}
	now := time.Now()
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		msgs := w.CLI.Parse(line)
		if msgs == nil {
			continue
		}
		for _, msg := range msgs {
			text := trimLine(msg.Text)
			switch {
			case passRegex.MatchString(text):
				state.Logs = append(state.Logs, logEvent{Timestamp: now, Kind: "pass", Message: text})
			case failRegex.MatchString(text):
				state.Logs = append(state.Logs, logEvent{Timestamp: now, Kind: "fail", Message: text})
			default:
				continue
			}
		}
		if len(state.Logs) > maxLogEvents {
			state.Logs = state.Logs[len(state.Logs)-maxLogEvents:]
		}
		state.LastUpdated = now
	}
	c.offset[w.Number] = newOffset
}

func (c *CodedSupervisor) readNewLogData(w workerInfo) (string, int64, error) {
	f, err := os.Open(w.LogPath)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	offset := c.offset[w.Number]
	if offset > info.Size() {
		offset = info.Size()
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return "", offset, err
	}

	data, err := io.ReadAll(bufio.NewReader(f))
	if err != nil {
		return "", offset, err
	}
	return string(data), offset + int64(len(data)), nil
}

func (c *CodedSupervisor) writeSnapshot() error {
	snap := c.buildSnapshot()
	if snap == nil {
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(c.outputPath), 0o755); err != nil {
		return err
	}

	f, err := os.Create(c.outputPath)
	if err != nil {
		return err
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(snap)
}

func (c *CodedSupervisor) buildSnapshot() *snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.state) == 0 {
		return nil
	}

	workers := make([]workerSnapshot, 0, len(c.state))
	for _, w := range c.workers {
		state, ok := c.state[w.Number]
		if !ok {
			continue
		}
		lastPass := lastEvent(state.Logs, "pass")
		lastFail := lastEvent(state.Logs, "fail")
		workers = append(workers, workerSnapshot{
			WorkerNumber: w.Number,
			Git:          state.Git,
			Logs: logSummary{
				LastPass: lastPass,
				LastFail: lastFail,
				Recent:   append([]logEvent(nil), state.Logs...),
			},
			LastUpdated: state.LastUpdated,
		})
	}

	return &snapshot{
		UpdatedAt: time.Now(),
		Workers:   workers,
	}
}

func lastEvent(events []logEvent, kind string) *logEvent {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind == kind {
			return &events[i]
		}
	}
	return nil
}

func runGit(ctx context.Context, dir string, args string) (string, error) {
	fields := strings.Fields(args)
	if len(fields) == 0 {
		return "", fmt.Errorf("empty git args")
	}
	cmd := exec.CommandContext(ctx, "git", fields...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %v", args, err)
	}
	return string(out), nil
}

func parseNumstat(input string) []fileChange {
	lines := splitLines(input)
	changes := make([]fileChange, 0, len(lines))
	for _, line := range lines {
		parts := strings.Split(line, "\t")
		if len(parts) < 3 {
			continue
		}
		changes = append(changes, fileChange{
			Added:   parseCount(parts[0]),
			Deleted: parseCount(parts[1]),
			File:    parts[2],
		})
	}
	return changes
}

func parseCount(val string) int {
	i, _ := strconv.Atoi(val)
	return i
}

func splitLines(input string) []string {
	var lines []string
	scanner := bufio.NewScanner(strings.NewReader(input))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func trimLine(line string) string {
	const max = 500
	if len(line) <= max {
		return line
	}
	return line[:max]
}

func summarizeLogLine(line string) string {
	var root map[string]any
	if err := json.Unmarshal([]byte(line), &root); err != nil {
		return line
	}
	switch root["type"] {
	case "assistant":
		msg, _ := root["message"].(map[string]any)
		content, _ := msg["content"].([]any)
		var parts []string
		for _, c := range content {
			obj, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if obj["type"] == "text" {
				if txt, ok := obj["text"].(string); ok {
					parts = append(parts, strings.TrimSpace(txt))
				}
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, " ")
		}
	case "user":
		if tool, ok := root["tool_use_result"].(map[string]any); ok {
			if out, ok := tool["stdout"].(string); ok && strings.TrimSpace(out) != "" {
				return strings.TrimSpace(out)
			}
			if errStr, ok := tool["stderr"].(string); ok && strings.TrimSpace(errStr) != "" {
				return strings.TrimSpace(errStr)
			}
		}
		msg, _ := root["message"].(map[string]any)
		content, _ := msg["content"].([]any)
		var parts []string
		for _, c := range content {
			obj, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if obj["type"] == "text" {
				if txt, ok := obj["text"].(string); ok {
					parts = append(parts, strings.TrimSpace(txt))
				}
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, " ")
		}
	}
	return line
}
