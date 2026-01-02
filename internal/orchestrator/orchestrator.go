package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/asynkron/Asynkron.SwarmGo/internal/agents"
	"github.com/asynkron/Asynkron.SwarmGo/internal/config"
	"github.com/asynkron/Asynkron.SwarmGo/internal/events"
	"github.com/asynkron/Asynkron.SwarmGo/internal/session"
	"github.com/asynkron/Asynkron.SwarmGo/internal/worktree"
)

// Orchestrator coordinates workers, supervisor, and UI events.
type Orchestrator struct {
	session *session.Session
	opts    config.Options
	events  chan<- events.Event

	mu      sync.Mutex
	agents  []*agents.Agent
	started bool
}

// New constructs a new Orchestrator.
func New(sess *session.Session, opts config.Options, events chan<- events.Event) *Orchestrator {
	return &Orchestrator{
		session: sess,
		opts:    opts,
		events:  events,
	}
}

// Run executes a single swarm round. It blocks until the context is canceled or the round completes.
func (o *Orchestrator) Run(ctx context.Context) error {
	if o.started {
		return fmt.Errorf("orchestrator already running")
	}
	o.started = true

	o.emit(events.StatusMessage{Message: fmt.Sprintf("Session: %s", o.session.ID)})
	o.emit(events.StatusMessage{Message: fmt.Sprintf("Repository: %s", o.opts.Repo)})
	o.emit(events.StatusMessage{Message: fmt.Sprintf("Workers: Claude %d, Codex %d, Copilot %d, Gemini %d", o.opts.ClaudeWorkers, o.opts.CodexWorkers, o.opts.CopilotWorkers, o.opts.GeminiWorkers)})

	// Prime todo content
	o.loadTodo()

	// Create worktrees
	worktrees := o.buildWorktreePaths()
	o.emit(events.PhaseChanged{Phase: "Creating worktrees..."})
	if err := worktree.Create(ctx, o.opts.Repo, worktrees); err != nil {
		return err
	}

	// Start agents
	o.emit(events.PhaseChanged{Phase: "Starting workers..."})
	workers, workerLogs, err := o.startWorkers(ctx, worktrees)
	if err != nil {
		o.stopAll()
		return err
	}

	o.emit(events.PhaseChanged{Phase: "Starting supervisor..."})
	supervisor, err := o.startSupervisor(ctx, worktrees, workerLogs)
	if err != nil {
		o.stopAll()
		return err
	}
	o.emit(events.PhaseChanged{Phase: "Workers running..."})

	// Tick remaining time
	deadline := time.Now().Add(o.opts.Duration())
	timeout := time.NewTimer(o.opts.Duration())
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	defer timeout.Stop()

loop:
	for {
		select {
		case <-ctx.Done():
			o.emit(events.StatusMessage{Message: "Cancellation requested, stopping agents..."})
			o.stopAll()
			return ctx.Err()
		case <-timeout.C:
			o.emit(events.StatusMessage{Message: "Time limit reached, stopping workers..."})
			o.emit(events.PhaseChanged{Phase: "Stopping workers..."})
			for _, w := range workers {
				w.Stop()
			}
			// Wait a short grace period for supervisor to finish.
			go func() {
				time.Sleep(30 * time.Second)
				supervisor.Stop()
			}()
			break loop
		case <-ticker.C:
			remaining := time.Until(deadline)
			if remaining < 0 {
				remaining = 0
			}
			o.emit(events.RemainingTime{Duration: remaining})
		}
	}

	o.emit(events.RemainingTime{Duration: 0})
	o.emit(events.PhaseChanged{Phase: "Round finished"})
	o.emit(events.StatusMessage{Message: "Round finished"})
	return nil
}

func (o *Orchestrator) startWorkers(ctx context.Context, worktrees []string) ([]*agents.Agent, []string, error) {
	var workers []*agents.Agent
	var logs []string

	timestamp := time.Now().Format("20060102-150405")

	for i := range worktrees {
		agentType := o.agentTypeForIndex(i)
		cli := agents.NewCLI(agentType)
		branchName := ""
		if o.opts.Autopilot {
			branchName = fmt.Sprintf("autopilot/worker%d-%s", i+1, timestamp)
		}

		logPath := o.session.WorkerLogPath(i + 1)
		worker := agents.NewWorker(i, worktrees[i], o.opts.Todo, cli, logPath, o.opts.Autopilot, branchName, o.events)
		if err := worker.Start(ctx); err != nil {
			return nil, nil, err
		}

		workers = append(workers, worker)
		logs = append(logs, logPath)
		o.track(worker)
		o.emit(events.StatusMessage{Message: fmt.Sprintf("Started %s (%s) -> %s", worker.Name, cli.Name(), worktrees[i])})
	}

	return workers, logs, nil
}

func (o *Orchestrator) startSupervisor(ctx context.Context, worktrees, workerLogs []string) (*agents.Agent, error) {
	cli := agents.NewCLI(o.opts.Supervisor)
	supervisor := agents.NewSupervisor(worktrees, workerLogs, o.opts.Repo, cli, o.session.SupervisorLogPath(), o.opts.Autopilot, o.events)
	if err := supervisor.Start(ctx); err != nil {
		return nil, err
	}

	o.track(supervisor)
	o.emit(events.StatusMessage{Message: fmt.Sprintf("Started supervisor (%s)", cli.Name())})
	return supervisor, nil
}

func (o *Orchestrator) stopAll() {
	o.mu.Lock()
	defer o.mu.Unlock()

	for _, a := range o.agents {
		a.Stop()
	}
}

func (o *Orchestrator) track(a *agents.Agent) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.agents = append(o.agents, a)
}

func (o *Orchestrator) buildWorktreePaths() []string {
	paths := make([]string, 0, o.opts.TotalWorkers())
	for i := 0; i < o.opts.TotalWorkers(); i++ {
		paths = append(paths, o.session.WorktreePath(i+1))
	}
	return paths
}

func (o *Orchestrator) agentTypeForIndex(index int) config.AgentType {
	if index < o.opts.ClaudeWorkers {
		return config.AgentClaude
	}
	if index < o.opts.ClaudeWorkers+o.opts.CodexWorkers {
		return config.AgentCodex
	}
	if index < o.opts.ClaudeWorkers+o.opts.CopilotWorkers {
		return config.AgentCopilot
	}
	return config.AgentGemini
}

func (o *Orchestrator) loadTodo() {
	todoPath := filepath.Join(o.opts.Repo, o.opts.Todo)
	content, err := os.ReadFile(todoPath)
	if err != nil {
		return
	}
	o.emit(events.TodoLoaded{Content: string(content), Path: todoPath})
}

func (o *Orchestrator) emit(ev events.Event) {
	if o.events == nil {
		return
	}
	select {
	case o.events <- ev:
	default:
	}
}
