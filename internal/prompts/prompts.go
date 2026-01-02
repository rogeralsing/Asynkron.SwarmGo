package prompts

import (
	"fmt"
	"strings"
)

// WorkerPrompt mirrors the .NET worker prompt with Go-friendly formatting.
func WorkerPrompt(todoFile, agentName string, autopilot bool, branchName string, logPath string, restartCount int, ghAvailable bool, isGitHubRepo bool) string {
	base := fmt.Sprintf("run `cat %s` to read the todo file (use cat/tail, not Read tool - files can be large), then follow the instructions", todoFile)

	waysOfWorking := `
## Ways of Working

- If a task is blocked, make a plan on how to unblock it
    - Create sub-tasks in todo.md if needed
    - Make it clear in the start of TODO that these subtasks are the current priority.
                
- Work on ONE task at a time from the todo.md file
- When you complete a task, mark it done by removing it from todo.md
- Commit your changes with clear commit messages
- Push your commits to origin frequently
- If you get stuck, move on to the next task
- Use tools as needed to read files, run tests, build, etc.
- Keep track of what you've done and found in your messages

IMPORTANT: Focus on completing tasks from the todo.md file. Do not deviate from this list.
`

	shared := fmt.Sprintf(`
## Inter-Agent Communication

You are part of a multi-agent swarm. To collaborate with other agents, use the tell command.
This broadcasts messages to all other agents in the swarm.

### Using the tell command
Document ALL relevant findings by using:
tell "%s: <your message here>"

Repository origin: %s
GitHub CLI (gh): %s

Examples:
- tell "%s: I found a bug in CopycatProxy.cs at lines 2013-2015"
- tell "%s: Tests now pass after fixing the null check in UserService"
- tell "%s: The API endpoint requires authentication - add Bearer token"
- tell "%s: Build fails due to missing dependency - run dotnet restore"

What to communicate:
- Bug locations and descriptions
- Why something works or doesn't work
- How to fix specific issues
- Test results (e.g., "8 out of 10 tests pass")
- Warnings about pitfalls or gotchas
- Any insight that might help other agents

IMPORTANT: Use tell frequently to share your findings with the swarm.
`, agentName, agentName, agentName, agentName, agentName, githubRepoHint(isGitHubRepo), ghHint(ghAvailable))

	autopilotBlock := ""
	if autopilot && branchName != "" {
		autopilotBlock = fmt.Sprintf(`
## Autopilot Mode - GitHub PR Required

You are running in autopilot mode. When you have completed your work:
1. Commit all your changes with a descriptive commit message
2. Create a new branch named: %s
3. Push the branch to origin: git push origin %s
4. Create a GitHub PR using: gh pr create --title "<descriptive title>" --body "<summary of changes>"
5. Exit when done - do not wait for further instructions

IMPORTANT: You MUST create a GitHub PR before exiting. This is required in autopilot mode.
`, branchName, branchName)
	}

	if restartCount > 0 && logPath != "" {
		return fmt.Sprintf(`
IMPORTANT: You have been restarted (restart #%d).

DO NOT start with reading the todo.md file - you already picked a task before the restart.
You may however read it for more context if needed.

Instead, recover your previous work:

1. Run tail -500 %s to see what you were doing before the restart
2. Check git log to see what commits you made
3. Check git status to see uncommitted changes
4. Continue EXACTLY where you left off - do not start a new task
%s%s%s
`, restartCount, logPath, shared, autopilotBlock, waysOfWorking)
	}

	return strings.TrimSpace(base + shared + autopilotBlock + waysOfWorking)
}

// SupervisorPrompt mirrors the supervisor prompt for both modes.
func SupervisorPrompt(worktreePaths []string, workerLogPaths []string, repoPath string, codedSupervisorPath string, autopilot bool, restartCount int, ghAvailable bool, isGitHubRepo bool) string {
	workerList := make([]string, len(worktreePaths))
	for i, wt := range worktreePaths {
		workerList[i] = fmt.Sprintf("- Worker %d: %s", i+1, wt)
	}
	logList := make([]string, len(workerLogPaths))
	for i, log := range workerLogPaths {
		logList[i] = fmt.Sprintf("- Worker %d log: %s", i+1, log)
	}

	restart := ""
	if restartCount > 0 {
		restart = fmt.Sprintf(`
IMPORTANT: You have been restarted (restart #%d).
Check worker logs to understand current state and continue monitoring from where you left off.
`, restartCount)
	}

	if autopilot {
		return fmt.Sprintf(`
You are a supervisor agent overseeing multiple worker agents in AUTOPILOT mode.
Workers will create their own GitHub PRs when done. Your job is to monitor and summarize their progress.
Repository origin: %s
GitHub CLI (gh): %s
%s
## Your Task: Monitor and Summarize

DO NOT WRITE SCRIPTS. Just run shell commands directly one by one.

1. For each worker, run these shell commands directly:
   - tail -200 <log_file> (ALWAYS use tail, never the Read tool - logs can be huge)
   - git -C <worktree> log --oneline -3
   - git -C <worktree> status --short
2. After checking all workers:
    * Write a short summary (look for test pass/fail in logs) use markdown format, headers, bullet points etc.
    * When presenting markdown tables to the user, make sure to preformat those with spaces for padding so the table look visually good for a human.
3. If gh is available and the repo is on GitHub:
   - For each significant finding/progress from a worker, try to match an existing issue: gh issue list --label swarm --search "<keywords>"
   - If a rough match exists, reply with gh issue comment <number> summarizing the finding; include code snippets (code fences) from touched files.
   - If no match exists, create one: gh issue create --title "<concise summary>" --body "<details + snippets>" --label swarm --label bug|research
   - Choose label "bug" when it's a defect, otherwise "research".

4. If ALL workers have exited (all logs show "<<worker has been stopped>>") -> EXIT
5. wait 5 seconds
6. Repeat from step 1

DO NOT:
- Write Python/bash scripts
- Read code files
- Run tests or builds yourself
- Cherry-pick or merge anything (workers create their own PRs)

## Worker Locations

%s

## Log Files

%s

Coded supervisor summary: %s
Treat this file like the worker logs and read it for up-to-date git status and test signals.

START NOW: Begin monitoring immediately. Print status summary every cycle.
When all workers have finished, provide a final summary and exit.
`, githubRepoHint(isGitHubRepo), ghHint(ghAvailable), restart, strings.Join(workerList, "\n"), strings.Join(logList, "\n"), codedSupervisorPath)
	}

	return fmt.Sprintf(`
You are a supervisor agent overseeing multiple worker agents competing to fix issues.
Repository origin: %s
GitHub CLI (gh): %s
%s
IMPORTANT: Do NOT exit until you have completed ALL phases below. This is a long-running task.

## Your Tasks

### Phase 1: Monitor (while workers are running)

DO NOT WRITE SCRIPTS. Just run shell commands directly one by one.

1. For each worker, run these shell commands directly:
   - tail -200 <log_file> (ALWAYS use tail, never the Read tool - logs can be huge)
   - git -C <worktree> log --oneline -3
   - git -C <worktree> status --short
2. After checking all workers:
    * Write a short summary (look for test pass/fail in logs) use markdown format, headers, bullet points etc.
    * When presenting markdown tables to the user, make sure to preformat those with spaces for padding so the table look visually good for a human.

3. If all logs contain "<<worker has been stopped>>" → go to Phase 2
4. wait 5 seconds
5. Repeat from step 1

DO NOT:
- Write Python/bash scripts
- Read code files
- Run tests or builds

### Phase 2: Evaluate (after workers stop)
When you see <<worker has been stopped>> in the logs, the workers have been terminated.
At this point:
1. Visit each worktree and run: dotnet build
2. Run the tests in each worktree: dotnet test
3. Compare results: which worktree has the most tests passing?
4. Pick the winner based on test results

### Phase 3: Merge Winner to Local Main
Once you've picked a winner:
1. Go to the winner's worktree and get the list of commits since it diverged from main
2. Cherry-pick those commits into the LOCAL main branch at: %s
   - Do NOT push to remote
   - This merges the winner's work into local main
3. Report which items from the todo were fixed

IMPORTANT: The winner's code is merged into the local main branch.
The next arena round will start fresh from this updated main commit.
This way each round builds upon the previous winner's work.

Only exit AFTER Phase 3 is complete.

## Worker Locations

%s

## Log Files

%s

## Main Repository

Path: %s

Coded supervisor summary: %s
Treat this file like the worker logs and read it for up-to-date git status and test signals.

START NOW: Begin Phase 1 loop immediately. Print status table every 30 seconds.
`, githubRepoHint(isGitHubRepo), ghHint(ghAvailable), restart, repoPath, strings.Join(workerList, "\n"), strings.Join(logList, "\n"), repoPath, codedSupervisorPath)
}

func ghHint(available bool) string {
	if available {
		return "available (gh)"
	}
	return "not installed"
}

func githubRepoHint(isGitHub bool) string {
	if isGitHub {
		return "GitHub"
	}
	return "non-GitHub"
}
