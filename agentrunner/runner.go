package agentrunner

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
)

// Event is emitted for agent lifecycle and output.
type Event interface{ isEvent() }

type AgentAdded struct {
	ID       string
	Name     string
	Kind     string
	Model    string
	LogPath  string
	Worktree string
}

type AgentStopped struct {
	ID       string
	ExitCode int
}

type AgentLine struct {
	ID   string
	Kind MessageKind
	Line string
}

// MessageKind mirrors Say/Do/See categories.
type MessageKind int

const (
	MessageSay MessageKind = iota
	MessageDo
	MessageSee
)

func (AgentAdded) isEvent()   {}
func (AgentStopped) isEvent() {}
func (AgentLine) isEvent()    {}

// CLI abstracts how to invoke a CLI agent.
type CLI interface {
	Name() string
	Command() string
	UseStdin() bool
	BuildArgs(prompt string, model string) []string
	Model(index int) (apiModel string, display string)
	Parse(line string) []ParsedMessage
}

// ParsedMessage captures categorized output from an agent CLI.
type ParsedMessage struct {
	Kind MessageKind
	Text string
}

// Agent wraps a CLI process.
type Agent struct {
	ID      string
	Name    string
	Prompt  string
	Workdir string
	LogPath string
	Model   string
	Display string
	CLI     CLI
	Events  chan<- Event

	cmd        *exec.Cmd
	logFile    *os.File
	mu         sync.Mutex
	tailCancel context.CancelFunc
	tailWG     sync.WaitGroup
	done       chan struct{}
	lastExit   int
}

// Start launches the agent and begins streaming output/events.
func (a *Agent) Start(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.cmd != nil {
		return fmt.Errorf("agent %s already running", a.ID)
	}

	a.done = make(chan struct{})

	if err := os.MkdirAll(filepath.Dir(a.LogPath), 0o755); err != nil {
		return err
	}
	logFile, err := os.OpenFile(a.LogPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("create log: %w", err)
	}
	a.logFile = logFile

	if info, err := logFile.Stat(); err == nil && info.Size() > 0 {
		_, _ = fmt.Fprintln(a.logFile)
	}

	_, _ = fmt.Fprintf(a.logFile, "[%s] %s starting\n", time.Now().Format(time.RFC3339), a.Name)
	_, _ = fmt.Fprintf(a.logFile, "[%s] workdir: %s\n", time.Now().Format(time.RFC3339), a.Workdir)

	args := a.CLI.BuildArgs(a.Prompt, a.Model)
	cmd := exec.CommandContext(ctx, a.CLI.Command(), args...)
	cmd.Dir = a.Workdir

	_, _ = fmt.Fprintf(a.logFile, "[%s] command: %s %s\n\n", time.Now().Format(time.RFC3339), a.CLI.Command(), strings.Join(args, " "))

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
	added := AgentAdded{
		ID:       a.ID,
		Name:     a.Name,
		Kind:     a.CLI.Name(),
		Model:    display,
		LogPath:  a.LogPath,
		Worktree: a.Workdir,
	}
	a.emit(added)

	tailCtx, cancel := context.WithCancel(context.Background())
	a.tailCancel = cancel
	a.tailWG.Add(1)
	go a.tailFile(tailCtx)

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
	a.mu.Lock()
	a.lastExit = exit
	done := a.done
	a.mu.Unlock()

	select {
	case <-ctx.Done():
	default:
		a.emit(AgentStopped{ID: a.ID, ExitCode: exit})
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

	if done != nil {
		close(done)
	}
}

func (a *Agent) emit(ev Event) {
	if a.Events == nil {
		return
	}
	defer func() { _ = recover() }()
	a.Events <- ev
}

// tailFile streams appended log content to the UI-like event channel.
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
					if msg.Kind == MessageSay {
						a.emit(AgentLine{ID: a.ID, Kind: msg.Kind, Line: msg.Text})
						continue
					}
					for _, p := range strings.Split(msg.Text, "\n") {
						if strings.TrimRight(p, " \t\r") == "" {
							continue
						}
						a.emit(AgentLine{ID: a.ID, Kind: msg.Kind, Line: p})
					}
				}
			}
			if err == nil {
				continue
			}
			if err == io.EOF {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			break
		}
		_ = f.Close()
		time.Sleep(100 * time.Millisecond)
	}
}

var ansiRegexp = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

func stripANSI(s string) string {
	if strings.IndexByte(s, '\x1b') == -1 {
		return s
	}
	return ansiRegexp.ReplaceAllString(s, "")
}

func trimLines(s string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	return strings.Join(lines, "\n")
}

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
