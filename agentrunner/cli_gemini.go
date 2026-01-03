package agentrunner

import (
	"encoding/json"
	"fmt"
	"strings"
)

type geminiCLI struct{}

func GeminiCLI() CLI { return geminiCLI{} }

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
		return []ParsedMessage{{Kind: MessageSay, Text: line}}
	}
	var root map[string]any
	if err := json.Unmarshal([]byte(line), &root); err != nil {
		return []ParsedMessage{{Kind: MessageSay, Text: line}}
	}
	typ, _ := root["type"].(string)
	switch typ {
	case "message":
		if content, ok := root["content"].(string); ok && strings.TrimSpace(content) != "" {
			return []ParsedMessage{{Kind: MessageSay, Text: trimLines(content)}}
		}
	case "tool_use":
		name, _ := root["tool_name"].(string)
		params, _ := root["parameters"].(map[string]any)
		return []ParsedMessage{{Kind: MessageDo, Text: summarizeGeminiTool(name, params)}}
	case "tool_result":
		if out, ok := root["output"].(string); ok && strings.TrimSpace(out) != "" {
			return []ParsedMessage{{Kind: MessageSee, Text: strings.TrimSpace(out)}}
		}
		status, _ := root["status"].(string)
		toolID, _ := root["tool_id"].(string)
		if errObj, ok := root["error"].(map[string]any); ok {
			if msg, ok := errObj["message"].(string); ok {
				return []ParsedMessage{{Kind: MessageSee, Text: msg}}
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
		return []ParsedMessage{{Kind: MessageSee, Text: summary}}
	case "result":
		if status, _ := root["status"].(string); status == "error" {
			if errObj, ok := root["error"].(map[string]any); ok {
				if msg, ok := errObj["message"].(string); ok {
					return []ParsedMessage{{Kind: MessageSay, Text: msg}}
				}
			}
		}
	}
	return []ParsedMessage{{Kind: MessageSay, Text: line}}
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
