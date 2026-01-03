package agentrunner

import "strings"

type copilotCLI struct{}

func CopilotCLI() CLI { return copilotCLI{} }

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
	return []ParsedMessage{{Kind: MessageSay, Text: line}}
}
