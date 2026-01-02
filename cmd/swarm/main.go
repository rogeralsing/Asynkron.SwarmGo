package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/asynkron/Asynkron.SwarmGo/internal/config"
	"github.com/asynkron/Asynkron.SwarmGo/internal/detector"
	"github.com/asynkron/Asynkron.SwarmGo/internal/events"
	"github.com/asynkron/Asynkron.SwarmGo/internal/orchestrator"
	"github.com/asynkron/Asynkron.SwarmGo/internal/session"
	"github.com/asynkron/Asynkron.SwarmGo/internal/ui"
	tea "github.com/charmbracelet/bubbletea"
)

func main() {
	opts, supervisorFlag := parseFlags()

	if opts.Detect {
		runDetect()
		return
	}

	supervisorType, err := parseAgentType(supervisorFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid --supervisor value: %v\n", err)
		os.Exit(1)
	}
	opts.Supervisor = supervisorType

	if err := opts.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if !opts.SkipDetect {
		if err := ensureAgentsInstalled(opts); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}

	if opts.Resume != "" {
		fmt.Fprintf(os.Stderr, "resume is not implemented in the Go port yet\n")
		os.Exit(1)
	}

	sess, err := session.New(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create session: %v\n", err)
		os.Exit(1)
	}

	eventCh := make(chan events.Event, 256)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		orch := orchestrator.New(sess, opts, eventCh)
		if err := orch.Run(ctx); err != nil && ctx.Err() == nil {
			eventCh <- events.StatusMessage{Message: fmt.Sprintf("orchestrator error: %v", err)}
		}
		close(eventCh)
	}()

	program := tea.NewProgram(ui.New(sess, opts, eventCh), tea.WithAltScreen(), tea.WithMouseCellMotion())
	if _, err := program.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "ui error: %v\n", err)
	}

	cancel()
	wg.Wait()
}

func parseFlags() (config.Options, string) {
	var opts config.Options
	var supervisor string

	flag.IntVar(&opts.ClaudeWorkers, "claude", 0, "number of Claude worker agents")
	flag.IntVar(&opts.CodexWorkers, "codex", 0, "number of Codex worker agents")
	flag.IntVar(&opts.CopilotWorkers, "copilot", 0, "number of Copilot worker agents")
	flag.IntVar(&opts.GeminiWorkers, "gemini", 0, "number of Gemini worker agents")
	flag.StringVar(&opts.Repo, "repo", "", "path to git repository (defaults to current repo)")
	flag.StringVar(&opts.Todo, "todo", "todo.md", "path to todo file relative to repo")
	flag.IntVar(&opts.Minutes, "minutes", 15, "minutes to run before stopping workers")
	flag.BoolVar(&opts.Arena, "arena", false, "arena mode (multiple timed rounds)")
	flag.BoolVar(&opts.Autopilot, "autopilot", true, "autopilot mode (workers create PR branches)")
	flag.IntVar(&opts.MaxRounds, "max-rounds", 10, "maximum number of rounds in arena mode")
	flag.StringVar(&opts.Resume, "resume", "", "resume a previous session (not yet implemented)")
	flag.BoolVar(&opts.Detect, "detect", false, "detect installed CLI agents and exit")
	flag.BoolVar(&opts.SkipDetect, "skip-detect", false, "skip agent detection")
	flag.StringVar(&supervisor, "supervisor", "claude", "supervisor agent type (claude|codex|copilot|gemini)")

	flag.Parse()
	return opts, supervisor
}

func parseAgentType(value string) (config.AgentType, error) {
	switch strings.ToLower(value) {
	case "claude":
		return config.AgentClaude, nil
	case "codex":
		return config.AgentCodex, nil
	case "copilot":
		return config.AgentCopilot, nil
	case "gemini":
		return config.AgentGemini, nil
	default:
		return "", fmt.Errorf("unknown agent %q", value)
	}
}

func runDetect() {
	statuses := detector.DetectAll()
	fmt.Println("Detecting CLI agents...")
	for _, s := range statuses {
		installed := "no"
		if s.Installed {
			installed = "yes"
		}
		version := s.Version
		if version == "" {
			version = "-"
		}
		fmt.Printf("%-8s installed=%-3s version=%s", title(string(s.Type)), installed, version)
		if s.Error != "" {
			fmt.Printf(" (%s)", s.Error)
		}
		fmt.Println()
	}
}

func ensureAgentsInstalled(opts config.Options) error {
	statuses := detector.DetectAll()
	required := map[config.AgentType]bool{}
	if opts.ClaudeWorkers > 0 {
		required[config.AgentClaude] = true
	}
	if opts.CodexWorkers > 0 {
		required[config.AgentCodex] = true
	}
	if opts.CopilotWorkers > 0 {
		required[config.AgentCopilot] = true
	}
	if opts.GeminiWorkers > 0 {
		required[config.AgentGemini] = true
	}
	required[opts.Supervisor] = true

	missing := []string{}
	for _, st := range statuses {
		if required[st.Type] && !st.Installed {
			missing = append(missing, string(st.Type))
		}
	}

	if len(missing) > 0 {
		return fmt.Errorf("required agents not installed: %s", strings.Join(missing, ", "))
	}
	return nil
}

func title(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
