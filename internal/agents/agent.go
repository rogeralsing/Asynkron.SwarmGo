package agents

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/asynkron/Asynkron.SwarmGo/internal/events"
)

// Agent represents a running CLI process and streams its output to the UI.
type Agent struct {
	ID       string
	Name     string
	Prompt   string
	Workdir  string
	LogPath  string
	Model    string
	Display  string
	CLI      CLI
	events   chan<- events.Event
	restarts int

	cmd             *exec.Cmd
	logFile         *os.File
	mu              sync.Mutex
	tailCancel      context.CancelFunc
	tailWG          sync.WaitGroup
	isSupervisor    bool
	workerWorktrees []string
	workerLogPaths  []string
}

// Start launches the agent process and begins streaming output.
func (a *Agent) Start(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.cmd != nil {
		return fmt.Errorf("agent %s already running", a.ID)
	}

	if err := os.MkdirAll(filepath.Dir(a.LogPath), 0o755); err != nil {
		return err
	}
	logFile, err := os.Create(a.LogPath)
	if err != nil {
		return fmt.Errorf("create log: %w", err)
	}
	a.logFile = logFile

	_, _ = fmt.Fprintf(a.logFile, "[%s] %s starting\n", time.Now().Format(time.RFC3339), a.Name)
	_, _ = fmt.Fprintf(a.logFile, "[%s] workdir: %s\n", time.Now().Format(time.RFC3339), a.Workdir)

	args := a.CLI.BuildArgs(a.Prompt, a.Model)
	cmd := exec.CommandContext(ctx, a.CLI.Command(), args...)
	cmd.Dir = a.Workdir

	_, _ = fmt.Fprintf(a.logFile, "[%s] command: %s %s\n\n", time.Now().Format(time.RFC3339), a.CLI.Command(), strings.Join(args, " "))

	// Tail the log file so UI sees live output (mirrors the C# message stream).
	tailCtx, cancel := context.WithCancel(context.Background())
	a.tailCancel = cancel
	a.tailWG.Add(1)
	go a.tailFile(tailCtx)

	if a.CLI.UseStdin() {
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return fmt.Errorf("stdin pipe: %w", err)
		}
		go func() {
			_, _ = io.WriteString(stdin, a.Prompt)
			_ = stdin.Close()
		}()
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start agent: %w", err)
	}

	a.cmd = cmd
	display := a.Display
	if display == "" {
		display = a.Model
	}
	a.emit(events.AgentAdded{
		ID:       a.ID,
		Name:     a.Name,
		Kind:     a.CLI.Name(),
		Model:    display,
		LogPath:  a.LogPath,
		Worktree: a.Workdir,
	})

	go a.stream(stdout)
	go a.stream(stderr)
	go a.wait(ctx)

	return nil
}

// Stop terminates the process.
func (a *Agent) Stop() {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.cmd == nil || a.cmd.Process == nil {
		return
	}
	_ = a.cmd.Process.Kill()

	if a.tailCancel != nil {
		a.tailCancel()
	}
	a.tailWG.Wait()
}

func (a *Agent) stream(r io.Reader) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		_, _ = a.logFile.WriteString(line + "\n")
	}
}

func (a *Agent) wait(ctx context.Context) {
	err := a.cmd.Wait()
	exit := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exit = exitErr.ExitCode()
		} else {
			exit = 1
		}
	}

	select {
	case <-ctx.Done():
		// Context cancellation: still notify but no restart.
	default:
		a.emit(events.AgentStopped{ID: a.ID, ExitCode: exit})
	}

	a.mu.Lock()
	cmd := a.cmd
	a.cmd = nil
	logFile := a.logFile
	a.logFile = nil
	a.mu.Unlock()

	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Release()
	}
	if logFile != nil {
		_ = logFile.Close()
	}

	if a.tailCancel != nil {
		a.tailCancel()
	}
	a.tailWG.Wait()
}

func (a *Agent) emit(ev events.Event) {
	if a.events == nil {
		return
	}
	select {
	case a.events <- ev:
	default:
		// Drop if channel is full to keep agents flowing.
	}
}

// tailFile streams appended log content to the UI, similar to the original C# message stream.
func (a *Agent) tailFile(ctx context.Context) {
	defer a.tailWG.Done()

	const tailBytes = 64 * 1024

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		f, err := os.Open(a.LogPath)
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}

		reader := bufio.NewReader(f)
		if info, _ := f.Stat(); info != nil && info.Size() > tailBytes {
			_, _ = f.Seek(-tailBytes, io.SeekEnd)
			reader = bufio.NewReader(f)
			_, _ = reader.ReadString('\n') // drop partial line
		}

		for {
			select {
			case <-ctx.Done():
				_ = f.Close()
				return
			default:
			}

			line, err := reader.ReadString('\n')
			if line != "" {
				trimmed := strings.TrimRight(line, "\r\n")
				clean := cleanLine(trimmed)
				if strings.TrimSpace(clean) == "" {
					continue
				}
				msgs := a.CLI.Parse(clean)
				if msgs == nil {
					continue
				}
				for _, msg := range msgs {
					if a.isSupervisor {
						// Skip See/Do noise; summarize activity instead.
						if msg.Kind == events.MessageSee {
							continue
						}
						if msg.Kind == events.MessageDo {
							summary := a.supervisorSummary(msg.Text)
							if summary == "" {
								continue
							}
							msg.Text = summary
							msg.Kind = events.MessageSay
						}
					}
					a.emit(events.AgentLine{ID: a.ID, Kind: msg.Kind, Line: msg.Text})
				}
			}
			if err == nil {
				continue
			}
			if err == io.EOF {
				// Wait for more data in same file.
				time.Sleep(50 * time.Millisecond)
				continue
			}
			break
		}
		_ = f.Close()
		// Re-open on next loop iteration to mimic tail -F.
		time.Sleep(100 * time.Millisecond)
	}
}

var ansiRegexp = regexp.MustCompile("\x1B\\[[0-9;]*[A-Za-z]")

func cleanLine(input string) string {
	stripped := ansiRegexp.ReplaceAllString(input, "")
	var b strings.Builder
	for _, r := range stripped {
		switch r {
		case '\t':
			b.WriteString("    ")
		default:
			if r >= 32 {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

func (a *Agent) supervisorSummary(text string) string {
	lower := strings.ToLower(text)
	for i, path := range a.workerLogPaths {
		if strings.Contains(text, path) {
			return fmt.Sprintf("📜 Reading logs for Worker %d", i+1)
		}
	}

	for i, wt := range a.workerWorktrees {
		if !strings.Contains(text, wt) {
			continue
		}

		switch {
		case strings.Contains(lower, "git status"):
			return fmt.Sprintf("🔍 Checking git status for Worker %d", i+1)
		case strings.Contains(lower, "git diff"):
			return fmt.Sprintf("📄 Checking git diff for Worker %d", i+1)
		case strings.Contains(lower, "git log"):
			return fmt.Sprintf("🧭 Checking git log for Worker %d", i+1)
		case strings.Contains(lower, "git cherry-pick"):
			return fmt.Sprintf("🍒 Cherry-picking commits for Worker %d", i+1)
		case strings.Contains(lower, "git merge"):
			return fmt.Sprintf("🧵 Merging changes for Worker %d", i+1)
		case strings.Contains(lower, "glob"):
			return fmt.Sprintf("🔎 Searching files for Worker %d", i+1)
		case strings.Contains(lower, "grep"):
			return fmt.Sprintf("🔎 Searching code for Worker %d", i+1)
		case strings.Contains(lower, "test"):
			return fmt.Sprintf("🧪 Running tests for Worker %d", i+1)
		case strings.Contains(lower, "read"):
			return fmt.Sprintf("📖 Reading file for Worker %d", i+1)
		default:
			return fmt.Sprintf("👀 Inspecting for Worker %d", i+1)
		}
	}

	return ""
}
