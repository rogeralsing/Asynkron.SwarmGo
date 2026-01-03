package agentrunner

import (
	"encoding/json"
	"strings"
)

type claudeCLI struct{}

func ClaudeCLI() CLI { return claudeCLI{} }

func (claudeCLI) Name() string               { return "Claude" }
func (claudeCLI) Command() string            { return "claude" }
func (claudeCLI) UseStdin() bool             { return true }
func (claudeCLI) Model(int) (string, string) { return "opus", "opus" }
func (claudeCLI) BuildArgs(prompt string, model string) []string {
	args := []string{"-p", "--dangerously-skip-permissions", "--tools", "default", "--output-format", "stream-json", "--verbose"}
	if model != "" {
		args = append(args, "--model", model)
	}
	return args
}
func (claudeCLI) Parse(line string) []ParsedMessage {
	if strings.TrimSpace(line) == "" {
		return nil
	}

	var root map[string]any
	if err := json.Unmarshal([]byte(line), &root); err != nil {
		return []ParsedMessage{{Kind: MessageSay, Text: line}}
	}

	typ, _ := root["type"].(string)
	switch typ {
	case "assistant":
		return parseClaudeAssistant(root)
	case "user":
		return parseClaudeToolResult(root)
	case "result":
		if result, ok := root["result"].(string); ok && strings.TrimSpace(result) != "" {
			return []ParsedMessage{{Kind: MessageSay, Text: result}}
		}
	}
	return nil
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
					out = append(out, ParsedMessage{Kind: MessageSay, Text: trimmed})
				}
			}
		case "tool_use":
			name, _ := obj["name"].(string)
			input, _ := obj["input"].(map[string]any)
			out = append(out, ParsedMessage{Kind: MessageDo, Text: summarizeClaudeTool(name, input)})
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
		out = append(out, ParsedMessage{Kind: MessageSee, Text: strings.TrimSpace(stdout)})
	}
	if stderr, ok := result["stderr"].(string); ok && strings.TrimSpace(stderr) != "" {
		out = append(out, ParsedMessage{Kind: MessageSee, Text: strings.TrimSpace(stderr)})
	}
	return out
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

func trimTrailingWhitespacePerLine(content string) string {
	lines := strings.Split(content, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	return strings.Join(lines, "\n")
}
