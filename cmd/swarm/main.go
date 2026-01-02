package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/asynkron/Asynkron.SwarmGo/internal/config"
	"github.com/asynkron/Asynkron.SwarmGo/internal/control"
	"github.com/asynkron/Asynkron.SwarmGo/internal/detector"
	"github.com/asynkron/Asynkron.SwarmGo/internal/events"
	"github.com/asynkron/Asynkron.SwarmGo/internal/orchestrator"
	"github.com/asynkron/Asynkron.SwarmGo/internal/session"
	"github.com/asynkron/Asynkron.SwarmGo/internal/ui"
	tea "github.com/charmbracelet/bubbletea"
)

func main() {
	opts, supervisorFlag, prepAgentFlag, minutesOverride, minutesSet := parseFlags()

	if opts.Detect {
		runDetect()
		return
	}

	var (
		sess   *session.Session
		resume bool
		err    error
	)

	if opts.Resume != "" {
		sess, err = session.Load(opts.Resume)
		if err != nil {
			fmt.Fprintf(os.Stderr, "load session: %v\n", err)
			os.Exit(1)
		}
		opts = sess.Options
		// Allow overriding minutes on resume to extend/shorten the run.
		if minutesSet {
			opts.Minutes = minutesOverride
		}
		resume = true
	} else {
		supervisorType, err := parseAgentType(supervisorFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid --supervisor value: %v\n", err)
			os.Exit(1)
		}
		opts.Supervisor = supervisorType

		prepAgent, err := parseAgentType(prepAgentFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid --prep-agent value: %v\n", err)
			os.Exit(1)
		}
		opts.PrepAgent = prepAgent
	}

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

	if sess == nil {
		sess, err = session.New(opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "create session: %v\n", err)
			os.Exit(1)
		}
	}

	eventCh := make(chan events.Event, 512)
	ctrlCh := make(chan control.Command, 16)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		orch := orchestrator.New(sess, opts, resume, eventCh, ctrlCh)
		if err := orch.Run(ctx); err != nil && ctx.Err() == nil {
			eventCh <- events.StatusMessage{Message: fmt.Sprintf("orchestrator error: %v", err)}
		}
		close(eventCh)
	}()

	program := tea.NewProgram(
		ui.New(sess, opts, eventCh, ctrlCh),
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
		tea.WithContext(ctx),
	)
	if _, err := program.Run(); err != nil && ctx.Err() == nil {
		fmt.Fprintf(os.Stderr, "ui error: %v\n", err)
	}

	cancel()
	wg.Wait()

	fmt.Printf("\nSession complete. To resume use: swarm --resume %s\n", sess.ID)
}

func parseFlags() (config.Options, string, string, int, bool) {
	var opts config.Options
	var supervisor string
	var prepAgent string
	minutesFlag := &intFlag{value: 15}

	flag.IntVar(&opts.ClaudeWorkers, "claude", 0, "number of Claude worker agents")
	flag.IntVar(&opts.CodexWorkers, "codex", 0, "number of Codex worker agents")
	flag.IntVar(&opts.CopilotWorkers, "copilot", 0, "number of Copilot worker agents")
	flag.IntVar(&opts.GeminiWorkers, "gemini", 0, "number of Gemini worker agents")
	flag.StringVar(&opts.Repo, "repo", "", "path to git repository (defaults to current repo)")
	flag.StringVar(&opts.Todo, "todo", "todo.md", "path to todo file relative to repo")
	flag.Var(minutesFlag, "minutes", "minutes to run before stopping workers")
	flag.BoolVar(&opts.Arena, "arena", false, "arena mode (multiple timed rounds)")
	flag.BoolVar(&opts.Autopilot, "autopilot", true, "autopilot mode (workers create PR branches)")
	flag.IntVar(&opts.MaxRounds, "max-rounds", 10, "maximum number of rounds in arena mode")
	flag.StringVar(&opts.Resume, "resume", "", "resume a previous session by its ID")
	flag.BoolVar(&opts.Detect, "detect", false, "detect installed CLI agents and exit")
	flag.BoolVar(&opts.SkipDetect, "skip-detect", false, "skip agent detection")
	flag.StringVar(&supervisor, "supervisor", "claude", "supervisor agent type (claude|codex|copilot|gemini)")
	flag.StringVar(&prepAgent, "prep-agent", "claude", "agent type for prep (claude|codex|copilot|gemini)")

	flag.Parse()

	opts.Minutes = minutesFlag.value
	return opts, supervisor, prepAgent, minutesFlag.value, minutesFlag.set
}

// intFlag tracks whether the flag was explicitly set.
type intFlag struct {
	value int
	set   bool
}

func (f *intFlag) String() string {
	return fmt.Sprintf("%d", f.value)
}

func (f *intFlag) Set(s string) error {
	v, err := strconv.Atoi(s)
	if err != nil {
		return err
	}
	f.value = v
	f.set = true
	return nil
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
	required[opts.PrepAgent] = true

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
