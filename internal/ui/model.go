package ui

import (
	"fmt"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/asynkron/Asynkron.SwarmGo/internal/config"
	"github.com/asynkron/Asynkron.SwarmGo/internal/events"
	"github.com/asynkron/Asynkron.SwarmGo/internal/session"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Model owns the Bubble Tea state for the swarm UI.
type Model struct {
	session *session.Session
	opts    config.Options
	events  <-chan events.Event

	width     int
	height    int
	phase     string
	remaining time.Duration
	status    []string
	selected  int
	itemOrder []string // "session", "todo", agent IDs
	agents    map[string]*agentView
	logs      map[string]*logBuffer
	todoPath  string
	todo      string
	view      viewport.Model
	ready     bool
	styles    theme
}

type agentView struct {
	ID       string
	Name     string
	Kind     string
	Model    string
	LogPath  string
	Running  bool
	ExitCode int
}

type logBuffer struct {
	lines []string
	limit int
}

// New returns a ready-to-run UI model.
func New(sess *session.Session, opts config.Options, events <-chan events.Event) Model {
	view := viewport.New(80, 20)
	view.HighPerformanceRendering = true

	m := Model{
		session:   sess,
		opts:      opts,
		events:    events,
		itemOrder: []string{"session", "todo"},
		agents:    make(map[string]*agentView),
		logs:      make(map[string]*logBuffer),
		view:      view,
		styles:    defaultTheme(),
	}
	// Default to showing the todo panel first so something useful is visible.
	if len(m.itemOrder) > 1 {
		m.selected = 1
	} else {
		m.selected = 0
	}
	m.width = 80
	m.height = 24
	m.resize()
	m.updateViewport()
	return m
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(waitForEvent(m.events))
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.resize()
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "up", "k":
			if m.selected > 0 {
				m.selected--
				m.updateViewport()
			}
		case "down", "j":
			if m.selected < len(m.itemOrder)-1 {
				m.selected++
				m.updateViewport()
			}
		case "pgup":
			m.view.LineUp(10)
		case "pgdown":
			m.view.LineDown(10)
		}
	case events.Event:
		m = m.handleEvent(msg)
	}

	cmds := []tea.Cmd{waitForEvent(m.events)}
	var cmd tea.Cmd
	m.view, cmd = m.view.Update(msg)
	cmds = append(cmds, cmd)
	return m, tea.Batch(cmds...)
}

func (m Model) View() string {
	if !m.ready {
		return "Starting swarm..."
	}

	header := m.renderHeader()
	list := m.renderList()
	log := m.renderLog()
	status := m.renderStatus()

	body := lipgloss.JoinHorizontal(lipgloss.Top, list, log)
	return lipgloss.JoinVertical(lipgloss.Left, header, body, status)
}

func (m *Model) handleEvent(ev events.Event) Model {
	switch e := ev.(type) {
	case events.AgentAdded:
		m.agents[e.ID] = &agentView{
			ID:      e.ID,
			Name:    e.Name,
			Kind:    e.Kind,
			Model:   e.Model,
			Running: true,
		}
		m.itemOrder = append(m.itemOrder, e.ID)
		m.status = append(m.status, fmt.Sprintf("Started %s (%s)", e.Name, e.Kind))
		m.ensureLog(e.ID)
		m.updateViewport()
	case events.AgentRemoved:
		delete(m.agents, e.ID)
		delete(m.logs, e.ID)
		m.rebuildOrder()
		m.updateViewport()
	case events.AgentStopped:
		if ag, ok := m.agents[e.ID]; ok {
			ag.Running = false
			ag.ExitCode = e.ExitCode
		}
		m.status = append(m.status, fmt.Sprintf("%s exited (%d)", e.ID, e.ExitCode))
	case events.AgentLine:
		buf := m.ensureLog(e.ID)
		buf.append(e.Line)
		m.updateViewport()
	case events.StatusMessage:
		m.status = append(m.status, e.Message)
	case events.PhaseChanged:
		m.phase = e.Phase
		m.status = append(m.status, e.Phase)
	case events.RemainingTime:
		m.remaining = e.Duration
	case events.TodoLoaded:
		m.todo = e.Content
		m.todoPath = e.Path
		m.updateViewport()
	}

	m.trimStatus()
	m.ready = true
	return *m
}

func (m *Model) ensureLog(id string) *logBuffer {
	if buf, ok := m.logs[id]; ok {
		return buf
	}
	buf := &logBuffer{limit: 2000}
	m.logs[id] = buf
	return buf
}

func (m *Model) rebuildOrder() {
	order := []string{"session", "todo"}
	for id := range m.agents {
		order = append(order, id)
	}
	m.itemOrder = order
	if m.selected >= len(order) {
		m.selected = len(order) - 1
	}
}

func (m *Model) resize() {
	if m.width == 0 || m.height == 0 {
		return
	}
	listWidth := 36
	logWidth := m.width - listWidth - 2
	logHeight := m.height - 6
	if logWidth < 20 {
		logWidth = 20
	}
	if logHeight < 5 {
		logHeight = 5
	}
	m.view.Width = logWidth
	m.view.Height = logHeight
	m.updateViewport()
}

func (m *Model) updateViewport() {
	if len(m.itemOrder) == 0 {
		return
	}
	id := m.itemOrder[m.selected]
	switch id {
	case "session":
		content := m.renderSessionInfo()
		m.view.SetContent(content)
	case "todo":
		m.view.SetContent(m.loadTodoContent())
	default:
		if buf, ok := m.logs[id]; ok {
			m.view.SetContent(buf.content())
		} else {
			m.view.SetContent("waiting for output...")
		}
	}
}

func (m Model) renderHeader() string {
	session := lipgloss.NewStyle().Bold(true).Foreground(m.styles.header).Render("SWARM")
	id := lipgloss.NewStyle().Foreground(m.styles.dim).Render(m.session.ID)
	mode := ""
	if m.opts.Arena {
		mode = lipgloss.NewStyle().Foreground(m.styles.accent).Render("Arena")
	} else if m.opts.Autopilot {
		mode = lipgloss.NewStyle().Foreground(m.styles.accent).Render("Autopilot")
	}
	timeText := ""
	if m.remaining > 0 {
		timeText = lipgloss.NewStyle().Foreground(m.styles.accent).Render(m.remaining.Round(time.Second).String())
	}
	phase := ""
	if m.phase != "" {
		phase = lipgloss.NewStyle().Foreground(m.styles.dim).Render(m.phase)
	}
	parts := []string{session, id}
	if mode != "" {
		parts = append(parts, mode)
	}
	if timeText != "" {
		parts = append(parts, timeText)
	}
	if phase != "" {
		parts = append(parts, phase)
	}
	return strings.Join(parts, "  ")
}

func (m Model) renderList() string {
	var rows []string
	for i, id := range m.itemOrder {
		selected := i == m.selected
		switch id {
		case "session":
			rows = append(rows, m.renderRow("Session", m.session.ID, selected, ""))
		case "todo":
			rows = append(rows, m.renderRow("Todo", m.opts.Todo, selected, ""))
		default:
			ag := m.agents[id]
			state := "●"
			color := m.styles.running
			if !ag.Running {
				state = "○"
				color = m.styles.error
			}
			state = lipgloss.NewStyle().Foreground(color).Render(state)
			meta := fmt.Sprintf("%s %s", ag.Kind, ag.Model)
			rows = append(rows, m.renderRow(ag.Name, meta, selected, state))
		}
	}
	list := strings.Join(rows, "\n")
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(m.styles.border).
		Width(34).
		Render(list)
}

func (m Model) renderRow(name, meta string, selected bool, prefix string) string {
	row := name
	if meta != "" {
		row += "  " + lipgloss.NewStyle().Foreground(m.styles.dim).Render(meta)
	}
	if prefix != "" {
		row = prefix + " " + row
	}
	if selected {
		return lipgloss.NewStyle().Bold(true).Background(m.styles.focus).Foreground(lipgloss.Color("#000000")).Render(row)
	}
	return row
}

func (m Model) renderLog() string {
	header := "Log"
	if m.selected < len(m.itemOrder) {
		header = title(m.itemOrder[m.selected])
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(m.styles.border).
		Padding(0, 1).
		Width(m.view.Width + 2).
		Height(m.view.Height + 2)
	content := lipgloss.NewStyle().Height(m.view.Height).Render(m.view.View())
	return box.Render(header + "\n" + content)
}

func (m Model) renderSessionInfo() string {
	lines := []string{
		fmt.Sprintf("Session ID: %s", m.session.ID),
		fmt.Sprintf("Path: %s", m.session.Path),
		fmt.Sprintf("Repository: %s", m.opts.Repo),
		fmt.Sprintf("Todo: %s", m.opts.Todo),
		fmt.Sprintf("Created: %s", m.session.Created.Format("2006-01-02 15:04:05")),
		fmt.Sprintf("Workers: Claude %d, Codex %d, Copilot %d, Gemini %d",
			m.opts.ClaudeWorkers, m.opts.CodexWorkers, m.opts.CopilotWorkers, m.opts.GeminiWorkers),
		fmt.Sprintf("Supervisor: %s", title(string(m.opts.Supervisor))),
	}
	return strings.Join(lines, "\n")
}

func (m Model) renderStatus() string {
	if len(m.status) == 0 {
		return ""
	}
	lines := m.status
	if len(lines) > 4 {
		lines = lines[len(lines)-4:]
	}
	return lipgloss.NewStyle().
		Foreground(m.styles.dim).
		Render(strings.Join(lines, "   "))
}

func (m *Model) trimStatus() {
	const max = 20
	if len(m.status) > max {
		m.status = m.status[len(m.status)-max:]
	}
}

func (b *logBuffer) append(line string) {
	if b.lines == nil {
		b.lines = []string{}
	}
	b.lines = append(b.lines, line)
	if len(b.lines) > b.limit && b.limit > 0 {
		b.lines = b.lines[len(b.lines)-b.limit:]
	}
}

func (b *logBuffer) content() string {
	return strings.Join(b.lines, "\n")
}

func (m *Model) loadTodoContent() string {
	// Prefer live read from disk so updates show without a restart.
	path := m.todoPath
	if path == "" {
		path = fmt.Sprintf("%s/%s", m.session.Path, m.opts.Todo)
	}
	if m.opts.Repo != "" {
		path = fmt.Sprintf("%s/%s", strings.TrimRight(m.opts.Repo, "/"), m.opts.Todo)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("todo not found: %s (%v)", path, err)
	}
	return string(content)
}

func waitForEvent(ch <-chan events.Event) tea.Cmd {
	return func() tea.Msg {
		if ch == nil {
			return nil
		}
		ev, ok := <-ch
		if !ok {
			return nil
		}
		return ev
	}
}

func title(s string) string {
	runes := []rune(s)
	if len(runes) == 0 {
		return s
	}
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}
