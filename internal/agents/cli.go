package agents

import (
	"encoding/json"
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

// NewCLI returns an implementation for the given agent type.
func NewCLI(agent config.AgentType) CLI {
	switch agent {
	case config.AgentClaude:
		return claudeCLI{}
	case config.AgentCodex:
		return codexCLI{}
	case config.AgentCopilot:
		return copilotCLI{}
	case config.AgentGemini:
		return geminiCLI{}
	default:
		return codexCLI{}
	}
}

type codexCLI struct{}

func (codexCLI) Name() string    { return "Codex" }
func (codexCLI) Command() string { return "codex" }
func (codexCLI) UseStdin() bool  { return false }
func (codexCLI) Model(i int) (string, string) {
	models := []string{"gpt-5.2-codex", "gpt-5.1-codex-max", "gpt-5.2"}
	short := []string{"5.2-cdx", "5.1-max", "5.2"}
	idx := i % len(models)
	return models[idx], short[idx]
}
func (c codexCLI) BuildArgs(prompt string, model string) []string {
	args := []string{"exec", prompt, "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox"}
	if model != "" {
		args = append(args, "--model", model)
	}
	return args
}
func (codexCLI) Parse(line string) []ParsedMessage {
	if strings.TrimSpace(line) == "" {
		return nil
	}
	trim := strings.TrimSpace(line)
	switch trim {
	case "thinking":
		return []ParsedMessage{{Kind: events.MessageSay, Text: "[thinking]"}}
	case "exec":
		return []ParsedMessage{{Kind: events.MessageDo, Text: "[exec]"}}
	default:
		return []ParsedMessage{{Kind: events.MessageSay, Text: line}}
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
	case "assistant":
		msg, _ := root["message"].(map[string]any)
		content, _ := msg["content"].([]any)
		var out []ParsedMessage
		for _, item := range content {
			obj, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch obj["type"] {
			case "text":
				if text, ok := obj["text"].(string); ok && strings.TrimSpace(text) != "" {
					out = append(out, ParsedMessage{Kind: events.MessageSay, Text: trimLines(text)})
				}
			case "tool_use":
				name, _ := obj["name"].(string)
				input, _ := obj["input"].(map[string]any)
				out = append(out, ParsedMessage{Kind: events.MessageDo, Text: summarizeClaudeTool(name, input)})
			}
		}
		return out
	case "user":
		toolResult, _ := root["tool_use_result"].(map[string]any)
		if stdout, ok := toolResult["stdout"].(string); ok && strings.TrimSpace(stdout) != "" {
			return []ParsedMessage{{Kind: events.MessageSee, Text: strings.TrimSpace(stdout)}}
		}
		if stderr, ok := toolResult["stderr"].(string); ok && strings.TrimSpace(stderr) != "" {
			return []ParsedMessage{{Kind: events.MessageSee, Text: strings.TrimSpace(stderr)}}
		}
	case "result":
		if result, ok := root["result"].(string); ok && strings.TrimSpace(result) != "" {
			return []ParsedMessage{{Kind: events.MessageSay, Text: trimLines(result)}}
		}
	}
	return []ParsedMessage{{Kind: events.MessageSay, Text: line}}
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
func (geminiCLI) Model(int) (string, string) { return "gemini-2.0-flash-exp", "flash" }
func (geminiCLI) BuildArgs(prompt string, model string) []string {
	if model == "" {
		model = "gemini-2.0-flash-exp"
	}
	return []string{prompt, "--yolo", "--output-format", "stream-json", "--model", model}
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
		return "tool"
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
