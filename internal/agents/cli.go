package agents

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/asynkron/Asynkron.SwarmGo/internal/config"
	"github.com/asynkron/Asynkron.SwarmGo/internal/events"
)

// CLI abstracts how to invoke each agent executable.
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
	Kind events.AgentMessageKind
	Text string
}

// SupervisorModeler allows a CLI to override the model used for the supervisor agent.
// If not implemented, the regular Model(index) method is used instead.
type SupervisorModeler interface {
	SupervisorModel() (apiModel string, display string)
}

// NewCLI returns an implementation for the given agent type.
func NewCLI(agent config.AgentType) CLI {
	switch agent {
	case config.AgentClaude:
		return claudeCLI{}
	case config.AgentCodex:
		return &codexCLI{}
	case config.AgentCopilot:
		return copilotCLI{}
	case config.AgentGemini:
		return geminiCLI{}
	default:
		return &codexCLI{}
	}
}

type codexCLI struct {
	doMode bool
}

func (*codexCLI) Name() string    { return "Codex" }
func (*codexCLI) Command() string { return "codex" }
func (*codexCLI) UseStdin() bool  { return false }
func (*codexCLI) SupervisorModel() (string, string) {
	return "gpt-5.1-codex-mini", "5.1-mini"
}
func (*codexCLI) Model(i int) (string, string) {
	models := []string{"gpt-5.2-codex", "gpt-5.1-codex-max", "gpt-5.2"}
	short := []string{"5.2-cdx", "5.1-max", "5.2"}
	idx := i % len(models)
	return models[idx], short[idx]
}
func (*codexCLI) BuildArgs(prompt string, model string) []string {
	args := []string{"exec", prompt, "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox"}
	if model != "" {
		args = append(args, "--model", model)
	}
	return args
}
func (c *codexCLI) Parse(line string) []ParsedMessage {
	if strings.TrimSpace(line) == "" {
		return nil
	}
	clean := stripANSI(line)
	trim := strings.TrimSpace(clean)
	switch trim {
	case "thinking":
		c.doMode = false
		return []ParsedMessage{{Kind: events.MessageSay, Text: "[thinking]"}}
	case "exec":
		c.doMode = true
		return []ParsedMessage{{Kind: events.MessageDo, Text: "[exec]"}}
	default:
		kind := codexClassify(trim)
		if c.doMode {
			kind = events.MessageDo
		}
		return []ParsedMessage{{Kind: kind, Text: clean}}
	}
}

func stripANSI(s string) string {
	if strings.IndexByte(s, '\x1b') == -1 {
		return s
	}
	return ansiRegexp.ReplaceAllString(s, "")
}

func codexClassify(trim string) events.AgentMessageKind {
	lower := strings.ToLower(trim)
	switch {
	case strings.HasPrefix(trim, "$ "):
		return events.MessageDo
	case strings.HasPrefix(lower, "stdout:"):
		return events.MessageSee
	case strings.HasPrefix(lower, "stderr:"):
		return events.MessageSee
	case strings.HasPrefix(lower, "exit code"):
		return events.MessageSee
	case strings.HasPrefix(lower, "result:"):
		return events.MessageSee
	case strings.HasPrefix(lower, "output:"):
		return events.MessageSee
	default:
		return events.MessageSay
	}
}

type claudeCLI struct{}

func (claudeCLI) Name() string               { return "Claude" }
func (claudeCLI) Command() string            { return "claude" }
func (claudeCLI) UseStdin() bool             { return true }
func (claudeCLI) Model(int) (string, string) { return "opus", "opus" }
func (claudeCLI) BuildArgs(prompt string, model string) []string {
	args := []string{"-p", "--dangerously-skip-permissions", "--tools", "default", "--output-format", "stream-json", "--verbose"}
	if model != "" {
		args = append(args, "--model", model)
	}
	// Bubble Tea provides its own prompt injection; Claude reads from stdin.
	return args
}
func (claudeCLI) Parse(line string) []ParsedMessage {
	if strings.TrimSpace(line) == "" {
		return nil
	}

	var root map[string]any
	if err := json.Unmarshal([]byte(line), &root); err != nil {
		return []ParsedMessage{{Kind: events.MessageSay, Text: line}}
	}

	typ, _ := root["type"].(string)
	switch typ {
	case "assistant":
		return parseClaudeAssistant(root)
	case "user":
		return parseClaudeToolResult(root)
	case "result":
		if result, ok := root["result"].(string); ok && strings.TrimSpace(result) != "" {
			return []ParsedMessage{{Kind: events.MessageSay, Text: result}}
		}
	}
	return nil
}

type copilotCLI struct{}

func (copilotCLI) Name() string               { return "Copilot" }
func (copilotCLI) Command() string            { return "copilot" }
func (copilotCLI) UseStdin() bool             { return false }
func (copilotCLI) Model(int) (string, string) { return "gpt-5", "gpt-5" }
func (copilotCLI) BuildArgs(prompt string, model string) []string {
	if model == "" {
		model = "gpt-5"
	}
	return []string{"-p", prompt, "--allow-all-tools", "--allow-all-paths", "--stream", "on", "--model", model}
}
func (copilotCLI) Parse(line string) []ParsedMessage {
	if strings.TrimSpace(line) == "" {
		return nil
	}
	return []ParsedMessage{{Kind: events.MessageSay, Text: line}}
}

type geminiCLI struct{}

func (geminiCLI) Name() string               { return "Gemini" }
func (geminiCLI) Command() string            { return "gemini" }
func (geminiCLI) UseStdin() bool             { return false }
func (geminiCLI) Model(int) (string, string) { return "", "" }
func (geminiCLI) BuildArgs(prompt string, model string) []string {
	args := []string{prompt, "--yolo", "--output-format", "stream-json"}
	if model != "" {
		args = append(args, "--model", model)
	}
	return args
}
func (geminiCLI) Parse(line string) []ParsedMessage {
	trim := strings.TrimSpace(line)
	if trim == "" {
		return nil
	}
	if !strings.HasPrefix(trim, "{") {
		return []ParsedMessage{{Kind: events.MessageSay, Text: line}}
	}
	var root map[string]any
	if err := json.Unmarshal([]byte(line), &root); err != nil {
		return []ParsedMessage{{Kind: events.MessageSay, Text: line}}
	}
	typ, _ := root["type"].(string)
	switch typ {
	case "message":
		if content, ok := root["content"].(string); ok && strings.TrimSpace(content) != "" {
			return []ParsedMessage{{Kind: events.MessageSay, Text: trimLines(content)}}
		}
	case "tool_use":
		name, _ := root["tool_name"].(string)
		params, _ := root["parameters"].(map[string]any)
		return []ParsedMessage{{Kind: events.MessageDo, Text: summarizeGeminiTool(name, params)}}
	case "tool_result":
		if out, ok := root["output"].(string); ok && strings.TrimSpace(out) != "" {
			return []ParsedMessage{{Kind: events.MessageSee, Text: strings.TrimSpace(out)}}
		}
		status, _ := root["status"].(string)
		toolID, _ := root["tool_id"].(string)
		if errObj, ok := root["error"].(map[string]any); ok {
			if msg, ok := errObj["message"].(string); ok {
				return []ParsedMessage{{Kind: events.MessageSee, Text: msg}}
			}
		}
		summary := "tool_result"
		if toolID != "" && status != "" {
			summary = fmt.Sprintf("tool_result %s (%s)", toolID, status)
		} else if toolID != "" {
			summary = fmt.Sprintf("tool_result %s", toolID)
		} else if status != "" {
			summary = fmt.Sprintf("tool_result (%s)", status)
		}
		return []ParsedMessage{{Kind: events.MessageSee, Text: summary}}
	case "result":
		if status, _ := root["status"].(string); status == "error" {
			if errObj, ok := root["error"].(map[string]any); ok {
				if msg, ok := errObj["message"].(string); ok {
					return []ParsedMessage{{Kind: events.MessageSay, Text: msg}}
				}
			}
		}
	}
	return []ParsedMessage{{Kind: events.MessageSay, Text: line}}
}

func summarizeClaudeTool(name string, input map[string]any) string {
	if name == "" {
		return "Unknown tool"
	}
	switch name {
	case "Bash":
		if cmd, ok := input["command"].(string); ok {
			return "$ " + cmd
		}
	case "Read", "Write", "Edit":
		if path, ok := input["file_path"].(string); ok {
			return strings.ToLower(name) + ": " + path
		}
	case "Glob", "Grep":
		if pattern, ok := input["pattern"].(string); ok {
			return strings.ToLower(name) + ": " + pattern
		}
	}
	return name
}

func summarizeGeminiTool(name string, params map[string]any) string {
	if name == "" {
		return "tool"
	}
	switch name {
	case "run_shell_command", "shell":
		if cmd, ok := params["command"].(string); ok {
			return "$ " + cmd
		}
	case "read_file", "write_file", "edit_file", "replace":
		if path, ok := params["file_path"].(string); ok {
			return strings.ReplaceAll(name, "_", " ") + ": " + path
		}
	case "glob", "grep":
		if pattern, ok := params["pattern"].(string); ok {
			return name + ": " + pattern
		}
	}
	return name
}

func trimLines(s string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	return strings.Join(lines, "\n")
}

func parseClaudeAssistant(root map[string]any) []ParsedMessage {
	msg, ok := root["message"].(map[string]any)
	if !ok {
		return nil
	}
	content, ok := msg["content"].([]any)
	if !ok {
		return nil
	}
	var out []ParsedMessage
	for _, item := range content {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		switch obj["type"] {
		case "text":
			if text, ok := obj["text"].(string); ok {
				trimmed := trimTrailingWhitespacePerLine(text)
				if strings.TrimSpace(trimmed) != "" {
					out = append(out, ParsedMessage{Kind: events.MessageSay, Text: trimmed})
				}
			}
		case "tool_use":
			name, _ := obj["name"].(string)
			input, _ := obj["input"].(map[string]any)
			out = append(out, ParsedMessage{Kind: events.MessageDo, Text: summarizeClaudeTool(name, input)})
		}
	}
	return out
}

func parseClaudeToolResult(root map[string]any) []ParsedMessage {
	result, ok := root["tool_use_result"].(map[string]any)
	if !ok {
		return nil
	}
	var out []ParsedMessage
	if stdout, ok := result["stdout"].(string); ok && strings.TrimSpace(stdout) != "" {
		out = append(out, ParsedMessage{Kind: events.MessageSee, Text: strings.TrimSpace(stdout)})
	}
	if stderr, ok := result["stderr"].(string); ok && strings.TrimSpace(stderr) != "" {
		out = append(out, ParsedMessage{Kind: events.MessageSee, Text: strings.TrimSpace(stderr)})
	}
	return out
}

func trimTrailingWhitespacePerLine(content string) string {
	lines := strings.Split(content, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	return strings.Join(lines, "\n")
}
