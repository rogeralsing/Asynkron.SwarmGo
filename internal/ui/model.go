package ui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/asynkron/Asynkron.SwarmGo/internal/config"
	"github.com/asynkron/Asynkron.SwarmGo/internal/control"
	"github.com/asynkron/Asynkron.SwarmGo/internal/events"
	"github.com/asynkron/Asynkron.SwarmGo/internal/session"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
)

// Model owns the Bubble Tea state for the swarm UI.
type Model struct {
	session *session.Session
	opts    config.Options
	events  <-chan events.Event
	control chan<- control.Command

	width        int
	height       int
	phase        string
	remaining    time.Duration
	status       []string
	selected     int
	itemOrder    []string // "session", "todo", agent IDs
	agents       map[string]*agentView
	logs         map[string]*logBuffer
	todoPath     string
	todo         string
	view         viewport.Model
	spinner      spinner.Model
	ready        bool
	styles       theme
	listWidth    int
	mouseOverLog bool
	mouseEnabled bool
	hasCoded     bool
	eventsClosed bool
	pendingView  bool
	scrollBottom bool
	mdRenderer   *glamour.TermRenderer
	mdMu         sync.Mutex
	todoCache    todoCache
	inputActive  bool
	inputField   textarea.Model
	inputTarget  string
}

type agentView struct {
	ID       string
	Name     string
	Kind     string
	Model    string
	LogPath  string
	Running  bool
	ExitCode int
	Spinner  int
}

type logBuffer struct {
	lines []logEntry
	limit int
	// cached rendered content to avoid re-rendering large logs on every selection
	rendered string
	dirty    bool
	doMode   bool
}

type todoCache struct {
	path     string
	modTime  time.Time
	width    int
	rendered string
}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

const (
	logBufferLimit        = 300
	markdownRenderTimeout = 75 * time.Millisecond
	markdownRenderBudget  = 150 * time.Millisecond
)

type logEntry struct {
	Kind events.AgentMessageKind
	Text string
	// cached render to avoid re-rendering on scroll
	rendered       string
	renderMarkdown bool
}

// New returns a ready-to-run UI model.
func New(sess *session.Session, opts config.Options, events <-chan events.Event, control chan<- control.Command) Model {
	view := viewport.New(80, 20)
	view.MouseWheelEnabled = true
	theme := defaultTheme()
	sp := spinner.New()
	sp.Style = lipgloss.NewStyle().Foreground(theme.accent)

	ti := textarea.New()
	ti.Prompt = ""
	ti.Placeholder = "Enter note (Enter=send, Alt+Enter=newline, Esc=cancel)"
	ti.SetWidth(view.Width)
	ti.SetHeight(5)
	ti.ShowLineNumbers = false
	ti.Focus()

	m := Model{
		session:      sess,
		opts:         opts,
		events:       events,
		itemOrder:    []string{"session", "todo", "coded"},
		agents:       make(map[string]*agentView),
		logs:         make(map[string]*logBuffer),
		view:         view,
		styles:       theme,
		mouseEnabled: true,
		hasCoded:     true,
		spinner:      sp,
		control:      control,
		inputField:   ti,
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
	return tea.Batch(waitForEvent(m.events), tea.EnableMouseCellMotion, spinner.Tick)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	skipViewport := false
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.resize()
	case tea.MouseMsg:
		if !m.mouseEnabled {
			break
		}
		m.mouseOverLog = msg.X > m.listWidth
		switch msg.Type {
		case tea.MouseWheelUp:
			if msg.X <= m.listWidth {
				if m.selected > 0 {
					m.selected--
					m.updateViewport()
				}
				skipViewport = true
			} else {
				m.view.LineUp(3)
				skipViewport = true
			}
		case tea.MouseWheelDown:
			if msg.X <= m.listWidth {
				if m.selected < len(m.itemOrder)-1 {
					m.selected++
					m.updateViewport()
				}
				skipViewport = true
			} else {
				m.view.LineDown(3)
				skipViewport = true
			}
		case tea.MouseMotion:
			// update hover only
			skipViewport = true
		}
	case tea.KeyMsg:
		if m.inputActive {
			switch {
			case msg.Type == tea.KeyEsc:
				m.inputActive = false
				m.inputTarget = ""
				m.inputField.Reset()
				return m, nil
			case msg.Type == tea.KeyEnter && msg.Alt:
				m.inputField.SetValue(m.inputField.Value() + "\n")
				return m, nil
			case msg.Type == tea.KeyEnter:
				target := m.inputTarget
				value := m.inputField.Value()
				m.inputActive = false
				m.inputTarget = ""
				m.inputField.Reset()
				if target != "" && m.control != nil {
					go func() { m.control <- control.RestartAgent{AgentID: target, Message: value} }()
					m.status = append(m.status, fmt.Sprintf("Restart requested for %s", target))
					m.trimStatus()
				}
				return m, nil
			}
			var cmd tea.Cmd
			m.inputField, cmd = m.inputField.Update(msg)
			return m, cmd
		}
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "up", "k":
			if m.selected > 0 {
				m.selected--
				skipViewport = true
			}
		case "down", "j":
			if m.selected < len(m.itemOrder)-1 {
				m.selected++
				skipViewport = true
			}
		case "pgup":
			m.view.LineUp(10)
		case "pgdown":
			m.view.LineDown(10)
		case "enter":
			m.startInjectPrompt()
		case "m":
			m.mouseEnabled = !m.mouseEnabled
			if m.mouseEnabled {
				cmds := []tea.Cmd{tea.EnableMouseCellMotion, waitForEvent(m.events)}
				return m, tea.Batch(cmds...)
			}
			cmds := []tea.Cmd{tea.DisableMouse, waitForEvent(m.events)}
			return m, tea.Batch(cmds...)
		}
	case events.Event:
		var ecmd tea.Cmd
		m, ecmd = m.handleEvent(msg)
		if ecmd != nil {
			cmds = append(cmds, ecmd)
		}
	case eventsClosedMsg:
		m.eventsClosed = true
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		if m.anyPendingDo() {
			m.markDoBuffersDirty()
			m.updateViewport()
		}
		return m, cmd
	case viewportUpdateMsg:
		m.pendingView = false
		m.updateViewport()
		if m.scrollBottom {
			if len(m.itemOrder) > 0 && m.selected < len(m.itemOrder) {
				if id := m.itemOrder[m.selected]; strings.HasPrefix(id, "worker") || id == "prep" || id == "supervisor" {
					m.view.GotoBottom()
				}
			}
			m.scrollBottom = false
		}
	}

	if !m.eventsClosed {
		cmds = append(cmds, waitForEvent(m.events))
	}
	var cmd tea.Cmd
	if !skipViewport {
		m.view, cmd = m.view.Update(msg)
		cmds = append(cmds, cmd)
	}
	if skipViewport {
		if c := m.requestViewportUpdate(); c != nil {
			cmds = append(cmds, c)
		}
	}
	return m, tea.Batch(cmds...)
}

func (m Model) View() string {
	if !m.ready {
		msg := lipgloss.NewStyle().Foreground(m.styles.accent).Render("Booting swarm... " + m.spinner.View())
		return lipgloss.NewStyle().Width(m.width).Height(m.height).Align(lipgloss.Center).Render(msg)
	}

	header := m.renderHeader()
	list := m.renderList()
	log := m.renderLog()
	status := m.renderStatus()

	body := lipgloss.JoinHorizontal(lipgloss.Top, list, log)
	if m.inputActive {
		body = lipgloss.JoinHorizontal(lipgloss.Top, list, m.renderInputOverlay())
	}
	return lipgloss.JoinVertical(lipgloss.Left, header, body, status)
}

func (m *Model) handleEvent(ev events.Event) (Model, tea.Cmd) {
	switch e := ev.(type) {
	case events.AgentAdded:
		if ag, exists := m.agents[e.ID]; exists {
			ag.Name = e.Name
			ag.Kind = e.Kind
			ag.Model = e.Model
			ag.LogPath = e.LogPath
			ag.Running = true
		} else {
			m.agents[e.ID] = &agentView{
				ID:      e.ID,
				Name:    e.Name,
				Kind:    e.Kind,
				Model:   e.Model,
				LogPath: e.LogPath,
				Running: true,
			}
			m.itemOrder = append(m.itemOrder, e.ID)
		}
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
		if ag, ok := m.agents[e.ID]; ok {
			ag.Spinner = (ag.Spinner + 1) % len(spinnerFrames)
		}
		buf := m.ensureLog(e.ID)
		kind := e.Kind
		if ag, ok := m.agents[e.ID]; ok && ag.Kind == "Codex" {
			switch strings.TrimSpace(e.Line) {
			case "[exec]":
				buf.doMode = true
			case "[thinking]":
				buf.doMode = false
			}
			if buf.doMode {
				kind = events.MessageDo
			}
		}
		trimmed := buf.append(logEntry{Kind: kind, Text: e.Line})
		if trimmed && m.selected < len(m.itemOrder) && m.itemOrder[m.selected] == e.ID {
			m.clampViewport()
		}
		if m.selected < len(m.itemOrder) && m.itemOrder[m.selected] == e.ID && !m.mouseOverLog && m.isAtBottom() {
			m.scrollBottom = true
		}
		if c := m.requestViewportUpdate(); c != nil {
			return *m, c
		}
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
		m.todoCache = todoCache{}
		if c := m.requestViewportUpdate(); c != nil {
			return *m, c
		}
	}

	m.trimStatus()
	m.ready = true
	return *m, nil
}

func (m *Model) ensureLog(id string) *logBuffer {
	if buf, ok := m.logs[id]; ok {
		return buf
	}
	buf := &logBuffer{limit: logBufferLimit}
	m.logs[id] = buf
	return buf
}

func (m *Model) rebuildOrder() {
	order := []string{"session", "todo"}
	if m.hasCoded {
		order = append(order, "coded")
	}
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
	m.listWidth = listWidth
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
	style := lipgloss.NewStyle().Width(m.view.Width)
	switch id {
	case "session":
		content := m.renderSessionInfo()
		m.view.SetContent(style.Render(content))
	case "todo":
		m.view.SetContent(style.Render(m.renderTodo()))
	case "coded":
		m.view.SetContent(style.Render(m.renderMetrics()))
	default:
		if buf, ok := m.logs[id]; ok {
			m.view.SetContent(style.Render(m.renderAgentLog(id, buf)))
		} else {
			m.view.SetContent(style.Render("waiting for output..."))
		}
	}
	m.clampViewport()
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
		case "coded":
			rows = append(rows, m.renderRow("Metrics", filepath.Base(m.session.CodedSupervisorPath()), selected, ""))
		default:
			ag := m.agents[id]
			var state string
			if ag.Running {
				frame := spinnerFrames[ag.Spinner%len(spinnerFrames)]
				if selected {
					state = frame
				} else {
					state = lipgloss.NewStyle().Foreground(m.styles.running).Render(frame)
				}
			} else {
				if selected {
					state = "○"
				} else {
					state = lipgloss.NewStyle().Foreground(m.styles.error).Render("○")
				}
			}
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
	if selected {
		if meta != "" {
			row += "  " + meta
		}
		if prefix != "" {
			row = prefix + " " + row
		}
		return lipgloss.NewStyle().Bold(true).Background(m.styles.focus).Foreground(lipgloss.Color("#000000")).Render(row)
	}
	if meta != "" {
		row += "  " + lipgloss.NewStyle().Foreground(m.styles.dim).Render(meta)
	}
	if prefix != "" {
		row = prefix + " " + row
	}
	return row
}

func (m Model) renderLog() string {
	if len(m.itemOrder) == 0 {
		return ""
	}

	selectedID := m.itemOrder[m.selected]
	header := title(selectedID)
	content := lipgloss.NewStyle().Width(m.view.Width).Height(m.view.Height).Render(m.view.View())

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(m.styles.border).
		Padding(0, 1).
		Width(m.view.Width + 2).
		Height(m.view.Height + 2)
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

func (b *logBuffer) append(entry logEntry) bool {
	trimmed := false
	b.lines = append(b.lines, entry)
	if len(b.lines) > b.limit && b.limit > 0 {
		b.lines = b.lines[len(b.lines)-b.limit:]
		trimmed = true
	}
	b.dirty = true
	return trimmed
}

func (b *logBuffer) content() string {
	out := make([]string, 0, len(b.lines))
	for _, l := range b.lines {
		out = append(out, l.Text)
	}
	return strings.Join(out, "\n")
}

func (m *Model) todoFilePath() string {
	path := m.todoPath
	if path == "" {
		path = filepath.Join(m.session.Path, m.opts.Todo)
	}
	if m.opts.Repo != "" {
		path = filepath.Join(m.opts.Repo, m.opts.Todo)
	}
	return path
}

func (m *Model) renderTodo() string {
	path := m.todoFilePath()
	width := m.view.Width
	if width <= 0 {
		width = 80
	}

	info, err := os.Stat(path)
	if err != nil {
		return fmt.Sprintf("todo not found: %s (%v)", path, err)
	}

	if m.todoCache.path == path &&
		m.todoCache.width == width &&
		!info.ModTime().After(m.todoCache.modTime) &&
		m.todoCache.rendered != "" {
		return m.todoCache.rendered
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("todo not found: %s (%v)", path, err)
	}

	rendered := m.renderMarkdown(string(content))
	m.todoCache = todoCache{
		path:     path,
		modTime:  info.ModTime(),
		width:    width,
		rendered: rendered,
	}

	return rendered
}

func (m *Model) loadCodedSupervisor() string {
	path := m.session.CodedSupervisorPath()
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("coded supervisor not found: %s (%v)", path, err)
	}
	return string(content)
}

func (m *Model) renderMetrics() string {
	path := m.session.CodedSupervisorPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("metrics not found: %s (%v)", path, err)
	}
	var snap codedSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return fmt.Sprintf("failed to parse metrics (%v)", err)
	}
	if len(snap.Workers) == 0 {
		return "no metrics yet"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Updated: %s\n\n", snap.UpdatedAt.Format("15:04:05"))
	for _, w := range snap.Workers {
		fmt.Fprintf(&b, "Worker %d\n", w.WorkerNumber)
		if w.Git.Error != "" {
			fmt.Fprintf(&b, "  Git error: %s\n\n", w.Git.Error)
			continue
		}
		fmt.Fprintf(&b, "  Branch: %s\n", w.Git.Branch)
		writeChanges := func(title string, list []fileChange) {
			if len(list) == 0 {
				return
			}
			fmt.Fprintf(&b, "  %s:\n", title)
			for _, fc := range list {
				fmt.Fprintf(&b, "    * %s  +%d -%d\n", fc.File, fc.Added, fc.Deleted)
			}
		}
		writeChanges("Staged", w.Git.Staged)
		writeChanges("Unstaged", w.Git.Unstaged)
		if len(w.Git.Untracked) > 0 {
			fmt.Fprintf(&b, "  Untracked:\n")
			for _, f := range w.Git.Untracked {
				fmt.Fprintf(&b, "    * %s\n", f)
			}
		}
		if len(w.Git.RecentCommits) > 0 {
			fmt.Fprintf(&b, "  Recent commits:\n")
			for _, c := range w.Git.RecentCommits {
				fmt.Fprintf(&b, "    * %s\n", c)
			}
		}
		if w.Logs.LastPass != nil || w.Logs.LastFail != nil {
			fmt.Fprintf(&b, "  Tests:\n")
			if w.Logs.LastPass != nil {
				fmt.Fprintf(&b, "    last pass: %s\n", w.Logs.LastPass.Message)
			}
			if w.Logs.LastFail != nil {
				fmt.Fprintf(&b, "    last fail: %s\n", w.Logs.LastFail.Message)
			}
		}
		fmt.Fprintln(&b)
	}

	return strings.TrimRight(b.String(), "\n")
}

type codedSnapshot struct {
	UpdatedAt time.Time     `json:"updatedAt"`
	Workers   []codedWorker `json:"workers"`
}

type codedWorker struct {
	WorkerNumber int           `json:"workerNumber"`
	Git          codedGit      `json:"git"`
	Logs         codedLogState `json:"logs"`
	LastUpdated  time.Time     `json:"lastUpdated"`
}

type codedGit struct {
	Branch        string       `json:"branch"`
	Staged        []fileChange `json:"staged"`
	Unstaged      []fileChange `json:"unstaged"`
	Untracked     []string     `json:"untracked"`
	RecentCommits []string     `json:"recentCommits"`
	Error         string       `json:"error"`
}

type fileChange struct {
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
	File    string `json:"file"`
}

type codedLogState struct {
	LastPass *codedLogEvent  `json:"lastPass"`
	LastFail *codedLogEvent  `json:"lastFail"`
	Recent   []codedLogEvent `json:"recent"`
}

type codedLogEvent struct {
	Timestamp time.Time `json:"timestamp"`
	Kind      string    `json:"kind"`
	Message   string    `json:"message"`
}

func (m *Model) renderAgentLog(id string, buf *logBuffer) string {
	tailDo := len(buf.lines) > 0 && buf.lines[len(buf.lines)-1].Kind == events.MessageDo
	if tailDo {
		buf.dirty = true
	}
	if !buf.dirty && buf.rendered != "" {
		return buf.rendered
	}

	renderDeadline := time.Now().Add(markdownRenderBudget)
	// Render markdown for all agents once (no reflow) to improve readability.
	markdownAllowed := true
	lines := make([]string, 0, len(buf.lines))
	for i := range buf.lines {
		l := &buf.lines[i]
		useMarkdown := markdownAllowed && time.Now().Before(renderDeadline)
		if l.rendered == "" || l.renderMarkdown != useMarkdown {
			l.rendered = m.renderLogEntry(*l, useMarkdown)
			l.renderMarkdown = useMarkdown
		}
		lines = append(lines, l.rendered)
	}
	if tailDo {
		lines = append(lines, lipgloss.NewStyle().Foreground(m.styles.do).Render(m.spinner.View()+" running..."))
	}
	if len(lines) == 0 {
		buf.rendered = "waiting for output..."
	} else {
		buf.rendered = strings.Join(lines, "\n")
	}
	buf.dirty = false
	return buf.rendered
}

func (m *Model) renderLogEntry(l logEntry, markdown bool) string {
	switch l.Kind {
	case events.MessageDo:
		return lipgloss.NewStyle().Foreground(m.styles.do).Render("→ " + l.Text)
	case events.MessageSee:
		return lipgloss.NewStyle().Foreground(m.styles.see).Render(l.Text)
	default:
		if markdown {
			return m.renderMarkdown(l.Text)
		}
		return l.Text
	}
}

func waitForEvent(ch <-chan events.Event) tea.Cmd {
	return func() tea.Msg {
		if ch == nil {
			return eventsClosedMsg{}
		}
		ev, ok := <-ch
		if !ok {
			return eventsClosedMsg{}
		}
		return ev
	}
}

type eventsClosedMsg struct{}

func (m *Model) renderMarkdown(text string) string {
	// Guard against pathological cases: very large content or slow renderer.
	if len(text) > 8000 {
		return text
	}

	if m.mdRenderer == nil {
		r, err := glamour.NewTermRenderer(
			glamour.WithAutoStyle(),
			glamour.WithWordWrap(0), // no reflow; render once per entry
		)
		if err == nil {
			m.mdRenderer = r
		}
	}
	if m.mdRenderer == nil {
		return text
	}
	done := make(chan string, 1)
	go func() {
		m.mdMu.Lock()
		defer m.mdMu.Unlock()
		out, err := m.mdRenderer.Render(text)
		if err != nil {
			done <- text
			return
		}
		done <- strings.TrimRight(out, "\n")
	}()

	select {
	case out := <-done:
		return out
	case <-time.After(markdownRenderTimeout):
		// Fall back to plain text if rendering is slow to avoid UI stalls.
		return text
	}
}

func (m *Model) requestViewportUpdate() tea.Cmd {
	if m.pendingView {
		return nil
	}
	m.pendingView = true
	const delay = 300 * time.Millisecond
	return tea.Tick(delay, func(time.Time) tea.Msg { return viewportUpdateMsg{} })
}

type viewportUpdateMsg struct{}

func (m *Model) isAtBottom() bool {
	if m.view.Height == 0 {
		return true
	}
	return m.view.AtBottom() || m.view.PastBottom()
}

func (m *Model) anyPendingDo() bool {
	for _, buf := range m.logs {
		if len(buf.lines) > 0 && buf.lines[len(buf.lines)-1].Kind == events.MessageDo {
			return true
		}
	}
	return false
}

func (m *Model) markDoBuffersDirty() {
	for _, buf := range m.logs {
		if len(buf.lines) > 0 && buf.lines[len(buf.lines)-1].Kind == events.MessageDo {
			buf.dirty = true
		}
	}
}

func (m *Model) clampViewport() {
	if m.view.PastBottom() {
		m.view.GotoBottom()
	}
}

func (m *Model) startInjectPrompt() {
	if m.inputActive {
		return
	}
	if m.selected >= len(m.itemOrder) {
		return
	}
	id := m.itemOrder[m.selected]
	if id == "session" || id == "todo" || id == "coded" {
		return
	}
	m.inputField.SetWidth(m.view.Width)
	m.inputActive = true
	m.inputTarget = id
	m.inputField.Reset()
	m.inputField.Focus()
}

func (m Model) renderInputOverlay() string {
	label := fmt.Sprintf("Inject & restart %s", title(m.inputTarget))
	warn := "Note: agent restarts fresh; context comes from its log."
	body := fmt.Sprintf("%s\n%s\n\n%s", label, warn, m.inputField.View())
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(m.styles.border).
		Padding(1, 2).
		Width(m.view.Width + 2).
		Height(m.view.Height + 2)
	return box.Render(body)
}

func title(s string) string {
	runes := []rune(s)
	if len(runes) == 0 {
		return s
	}
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}
