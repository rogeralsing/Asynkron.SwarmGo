package agentrunner

import "strings"

type codexCLI struct {
	doMode bool
}

func CodexCLI() CLI { return &codexCLI{} }

func (*codexCLI) Name() string    { return "Codex" }
func (*codexCLI) Command() string { return "codex" }
func (*codexCLI) UseStdin() bool  { return false }
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
		return []ParsedMessage{{Kind: MessageSay, Text: "[thinking]"}}
	case "exec":
		c.doMode = true
		return []ParsedMessage{{Kind: MessageDo, Text: "[exec]"}}
	default:
		kind := codexClassify(trim)
		if c.doMode {
			kind = MessageDo
		}
		return []ParsedMessage{{Kind: kind, Text: clean}}
	}
}

func codexClassify(trim string) MessageKind {
	lower := strings.ToLower(trim)
	switch {
	case strings.HasPrefix(trim, "$ "):
		return MessageDo
	case strings.HasPrefix(lower, "stdout:"):
		return MessageSee
	case strings.HasPrefix(lower, "stderr:"):
		return MessageSee
	case strings.HasPrefix(lower, "exit code"):
		return MessageSee
	case strings.HasPrefix(lower, "result:"):
		return MessageSee
	case strings.HasPrefix(lower, "output:"):
		return MessageSee
	default:
		return MessageSay
	}
}
