package demo

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const (
	mainSystem  = "You are Claude Code, Anthropic's official CLI for Claude. Help the user with software engineering tasks in their repository."
	subSystem   = "You are an Explore agent. Search the codebase and report findings concisely; do not modify files."
	titleSystem = "Generate a concise 5-word title for this conversation. Reply with the title only."
)

// scenario plays one VS Code session with three parallel subagents and,
// alongside it, a terminal CLI session whose prompt cache is invalidated.
func (s *sim) scenario(ctx context.Context, n int) error {
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		errs[0] = s.vscodeSession(ctx, n)
	}()
	go func() {
		defer wg.Done()
		if s.sleep(ctx, 4*time.Second) {
			errs[1] = s.cliSession(ctx, n)
		}
	}()
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}
	if errs[0] != nil {
		return errs[0]
	}
	return errs[1]
}

func (s *sim) vscodeSession(ctx context.Context, n int) error {
	sid := fmt.Sprintf("demo-vscode-%d", n)
	main := agentCall{session: sid, userAgent: vscodeUA, class: "main", system: mainSystem, model: mainModel, tools: mainTools}
	prompt := "Users get logged out at random. Find the token refresh race and fix it."
	ide := []string{
		"<ide_opened_file>The user opened the file " + workspace + "/internal/auth/refresh.go in the IDE. This may or may not be related to the current task.</ide_opened_file>",
		"<ide_selection>The user selected the lines 41 to 58 from " + workspace + "/internal/auth/refresh.go:\nfunc (s *Session) Refresh(ctx context.Context) error {\n\tif s.expired() {\n\t\treturn s.renew(ctx)\n\t}\n\treturn nil\n}</ide_selection>",
	}
	conv := []any{userText(append(ide, prompt)...)}

	title := agentCall{session: sid, userAgent: vscodeUA, class: "auxiliary", system: titleSystem, model: fastModel}
	if err := s.call(ctx, title, []any{userText(prompt)}, reply{
		ttft: 450 * time.Millisecond, text: "Fix token refresh race condition", uncached: 180, output: 9,
	}); err != nil {
		return err
	}

	areas := []struct{ desc, prompt, pattern, file string }{
		{"Find refresh callers", "Find every caller of Session.Refresh and note whether it holds the session lock.", "Refresh\\(", "/internal/api/middleware.go"},
		{"Inspect token store", "Inspect how tokens are persisted in internal/auth/store.go and whether writes are atomic.", "SaveToken", "/internal/auth/store.go"},
		{"Check refresh tests", "Find the tests covering concurrent refresh and report what they assert.", "TestRefresh", "/internal/auth/refresh_test.go"},
	}
	launch := reply{
		ttft: 900 * time.Millisecond, text: "I'll look at this from three angles in parallel.",
		uncached: 6, cacheWrite: 21_400, cacheRead: 0, output: 410,
	}
	subIDs := make([]string, len(areas))
	for i, a := range areas {
		subIDs[i] = fmt.Sprintf("ademo%02d%02d", n, i+1)
		launch.tools = append(launch.tools, toolCall{
			id: s.id("toolu"), name: "Agent",
			input:    map[string]any{"description": a.desc, "prompt": a.prompt, "subagent_type": "Explore"},
			runFor:   120 * time.Millisecond,
			response: map[string]any{"agentId": subIDs[i], "isAsync": true, "status": "async_launched"},
		})
	}
	if err := s.call(ctx, main, conv, launch); err != nil {
		return err
	}
	if err := s.runTools(ctx, main, launch); err != nil {
		return err
	}
	conv = append(conv, assistant(launch), results(launch, "Async agent launched successfully."))
	waiting := reply{ttft: 650 * time.Millisecond, text: "Three Explore agents are running; I'll combine their findings.", uncached: 3, cacheWrite: 900, cacheRead: 21_400, output: 38}
	if err := s.call(ctx, main, conv, waiting); err != nil {
		return err
	}
	conv = append(conv, assistant(waiting))

	var wg sync.WaitGroup
	errs := make([]error, len(areas))
	for i, a := range areas {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = s.subagent(ctx, sid, subIDs[i], a.prompt, a.pattern, a.file, i)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}

	conv = append(conv, userText("Agent reports: Refresh is called without the session lock in middleware.go; SaveToken is not atomic; no test covers concurrent refresh."))
	fix := reply{
		ttft: 800 * time.Millisecond, text: "The race: two requests refresh the same session without the lock. I'll take the lock in Refresh.",
		uncached: 4, cacheWrite: 2_300, cacheRead: 22_300, output: 520,
		tools: []toolCall{{
			id: s.id("toolu"), name: "Edit",
			input:    map[string]any{"file_path": workspace + "/internal/auth/refresh.go", "old_string": "if s.expired() {", "new_string": "s.mu.Lock()\n\tdefer s.mu.Unlock()\n\tif s.expired() {"},
			runFor:   140 * time.Millisecond,
			response: map[string]any{"filePath": workspace + "/internal/auth/refresh.go"},
		}},
	}
	if err := s.call(ctx, main, conv, fix); err != nil {
		return err
	}
	if err := s.runTools(ctx, main, fix); err != nil {
		return err
	}
	conv = append(conv, assistant(fix), results(fix, "The file has been updated."))
	return s.call(ctx, main, conv, reply{
		ttft: 700 * time.Millisecond, text: "Fixed: Refresh now holds the session lock, so concurrent requests renew the token once. I'd add a concurrent refresh test next.",
		uncached: 5, cacheWrite: 700, cacheRead: 24_600, output: 160,
	})
}

func (s *sim) subagent(ctx context.Context, sid, agentID, prompt, pattern, file string, i int) error {
	a := agentCall{session: sid, userAgent: vscodeUA, class: "subagent", agentID: agentID, agentType: "Explore", system: subSystem, model: fastModel, tools: subTools}
	conv := []any{userText(prompt)}
	steps := []reply{
		{ttft: 500 * time.Millisecond, uncached: 4, cacheWrite: 6_100, output: 60, tools: []toolCall{{
			id: s.id("toolu"), name: "Grep", input: map[string]any{"pattern": pattern, "path": workspace},
			runFor: time.Duration(90+40*i) * time.Millisecond, response: map[string]any{"numFiles": 3},
		}}},
		{ttft: 450 * time.Millisecond, uncached: 3, cacheWrite: 1_200, cacheRead: 6_100, output: 45, tools: []toolCall{{
			id: s.id("toolu"), name: "Read", input: map[string]any{"file_path": workspace + file},
			runFor: time.Duration(20+10*i) * time.Millisecond, response: map[string]any{"type": "text"},
		}}},
	}
	for _, step := range steps {
		if err := s.call(ctx, a, conv, step); err != nil {
			return err
		}
		if err := s.runTools(ctx, a, step); err != nil {
			return err
		}
		conv = append(conv, assistant(step), results(step, "(tool output)"))
	}
	if err := s.call(ctx, a, conv, reply{
		ttft: 550 * time.Millisecond, text: "Findings: " + prompt, uncached: 3, cacheWrite: 2_400, cacheRead: 7_300, output: 210,
	}); err != nil {
		return err
	}
	s.hook(map[string]any{"session_id": sid, "cwd": workspace, "hook_event_name": "SubagentStop", "agent_id": agentID, "agent_type": "Explore"})
	return nil
}

// cliSession runs a test from the terminal; its second turn finds the prompt
// cache invalidated.
func (s *sim) cliSession(ctx context.Context, n int) error {
	sid := fmt.Sprintf("demo-cli-%d", n)
	main := agentCall{session: sid, userAgent: cliUA, class: "main", system: mainSystem, model: mainModel, tools: mainTools}
	conv := []any{userText("Run the auth tests and tell me what fails.")}
	run := reply{
		ttft: 700 * time.Millisecond, text: "Running the auth tests.", uncached: 5, cacheWrite: 1_100, cacheRead: 60_200, output: 70,
		tools: []toolCall{{
			id: s.id("toolu"), name: "Bash", input: map[string]any{"command": "go test ./internal/auth/...", "description": "Run auth tests"},
			runFor: 2300 * time.Millisecond, response: map[string]any{"stdout": "FAIL TestRefreshConcurrent", "stderr": "", "interrupted": false},
		}},
	}
	if err := s.call(ctx, main, conv, run); err != nil {
		return err
	}
	if err := s.runTools(ctx, main, run); err != nil {
		return err
	}
	conv = append(conv, assistant(run), results(run, "--- FAIL: TestRefreshConcurrent (0.02s)"))
	// An edited CLAUDE.md changed the prompt prefix: most of the cache is gone.
	return s.call(ctx, main, conv, reply{
		ttft: 1400 * time.Millisecond, text: "TestRefreshConcurrent fails: two goroutines renew the same token. The prompt cache was rebuilt this turn.",
		uncached: 1_500, cacheWrite: 58_300, cacheRead: 2_100, output: 140,
	})
}
