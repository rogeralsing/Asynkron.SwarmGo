package agents

import (
	"fmt"
	"time"

	"github.com/asynkron/Asynkron.SwarmGo/internal/events"
	"github.com/asynkron/Asynkron.SwarmGo/internal/prompts"
)

// NewWorker builds a configured Agent representing a worker.
func NewWorker(index int, worktree string, todoFile string, cli CLI, logPath string, autopilot bool, branchName string, events chan<- events.Event) *Agent {
	// Pick display model based on worker index for a bit of variety.
	apiModel, displayModel := cli.Model(index)
	prompt := prompts.WorkerPrompt(todoFile, fmt.Sprintf("Worker %d", index+1), autopilot, branchName, logPath, 0)

	return &Agent{
		ID:      fmt.Sprintf("worker-%d", index+1),
		Name:    fmt.Sprintf("Worker %d", index+1),
		Prompt:  prompt,
		Workdir: worktree,
		LogPath: logPath,
		Model:   apiModel,
		CLI:     cli,
		Display: displayModel,
		events:  events,
	}
}

// NewSupervisor builds the supervisor agent.
func NewSupervisor(worktrees []string, workerLogs []string, repoPath string, codedPath string, cli CLI, logPath string, autopilot bool, events chan<- events.Event) *Agent {
	prompt := prompts.SupervisorPrompt(worktrees, workerLogs, repoPath, codedPath, autopilot, 0)
	apiModel, displayModel := cli.Model(int(time.Now().UnixNano()))
	return &Agent{
		ID:              "supervisor",
		Name:            "Supervisor",
		Prompt:          prompt,
		Workdir:         repoPath,
		LogPath:         logPath,
		Model:           apiModel,
		Display:         displayModel,
		CLI:             cli,
		events:          events,
		isSupervisor:    true,
		workerWorktrees: worktrees,
		workerLogPaths:  workerLogs,
	}
}
