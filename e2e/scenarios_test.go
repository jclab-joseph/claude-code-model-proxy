//go:build e2e

package e2e

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
)

// TestScenarios runs every scenario against every main model.
func TestScenarios(t *testing.T) {
	scenarios := []struct {
		name string
		run  func(t *testing.T, model string)
	}{
		{"Reply", testReply},
		{"FixWithTools", testFixWithTools},
		{"ParallelReads", testParallelReads},
		{"Subagent", testSubagent},
		{"SubagentOnClaude", testSubagentOnClaude},
		{"SwitchFromClaude", testSwitchFromClaude},
		{"Compact", testCompact},
	}
	for _, model := range mainModels() {
		t.Run(model, func(t *testing.T) {
			for _, scenario := range scenarios {
				t.Run(scenario.name, func(t *testing.T) {
					t.Parallel()
					requireReady(t, model)
					scenario.run(t, model)
				})
			}
		})
	}
}

// testReply is the smallest round trip: no tools, one answer.
func testReply(t *testing.T, model string) {
	s := newSession(t)
	tr := s.ask(model, "Reply with exactly the single word: pong")
	mustContain(t, tr, tr.answer(), "pong")
	assertMainModel(t, tr, model)
}

// testFixWithTools needs Read, Edit and Bash in sequence, over several turns.
func testFixWithTools(t *testing.T, model string) {
	s := newSession(t)
	s.write("calc.py", "def add(a, b):\n    return a - b\n")
	tr := s.ask(model, `calc.py has a bug. Read it, fix it with the Edit tool, then run `+
		`python3 -c "import calc; print(calc.add(2, 3))" with Bash and report the output.`)
	if got := s.read("calc.py"); !strings.Contains(got, "a + b") {
		t.Fatalf("calc.py was not fixed:\n%s\nevents: %s", got, tr.path)
	}
	mustContain(t, tr, tr.answer(), "5")
}

// testParallelReads checks that results of parallel tool calls stay attached
// to the call they answer, even when they come back out of order.
func testParallelReads(t *testing.T, model string) {
	s := newSession(t)
	contents := map[string]string{}
	var names []string
	for i := 1; i <= 6; i++ {
		name := fmt.Sprintf("f%d.txt", i)
		contents[name] = fmt.Sprintf("token-%d-%06d", i, rand.IntN(1000000))
		names = append(names, name)
		s.write(name, contents[name]+"\n")
	}

	tr := s.ask(model, "Read "+strings.Join(names, ", ")+" in parallel (all six Read calls in a single turn), "+
		"then list every file name with its exact content, one per line, as `name: content`.")
	if parallel := tr.mostParallel("Read"); parallel < 2 {
		t.Logf("the model read the files one at a time; result ordering was not exercised")
	}
	for _, name := range names {
		found := false
		for _, line := range strings.Split(tr.answer(), "\n") {
			if strings.Contains(line, name) && strings.Contains(line, contents[name]) {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s should be reported as %s:\n%s\nevents: %s", name, contents[name], tr.answer(), tr.path)
		}
	}
}

// testSubagent delegates to a general-purpose subagent, which runs on the
// main model's provider too.
func testSubagent(t *testing.T, model string) {
	s := newSession(t)
	for i := 1; i <= 4; i++ {
		s.write(fmt.Sprintf("n%d.txt", i), "x\n")
	}
	tr := s.ask(model, "Use the Agent tool with subagent_type general-purpose to have a subagent count the .txt files "+
		"in the current directory (not subdirectories). Then tell me the number it reports.")
	if len(tr.toolUses("Agent")) == 0 {
		t.Fatalf("the main agent did not delegate\nevents: %s", tr.path)
	}
	if len(tr.assistant(true)) == 0 {
		t.Fatalf("no subagent messages were streamed\nevents: %s", tr.path)
	}
	mustContain(t, tr, tr.answer(), "4")
}

// testSubagentOnClaude runs the main agent on the added model and a subagent
// on a Claude model: two providers in one turn.
func testSubagentOnClaude(t *testing.T, model string) {
	s := newSession(t)
	for i := 1; i <= 3; i++ {
		s.write(fmt.Sprintf("n%d.txt", i), "x\n")
	}
	s.write(".claude/agents/claude-helper.md", `---
name: claude-helper
description: Answers questions about the files in the working directory. Use it whenever asked to delegate to claude-helper.
model: `+subagentModel()+`
tools: Bash, Glob, Read
---
You are a careful helper. Answer the question you are given, using the tools when needed, in one line.
`)

	tr := s.ask(model, "Delegate to the claude-helper subagent (Agent tool, subagent_type claude-helper): "+
		"ask it how many .txt files are in the current directory, not counting subdirectories. Then tell me its answer.")
	assertMainModel(t, tr, model)

	subagent := tr.assistant(true)
	if len(subagent) == 0 {
		t.Fatalf("no subagent messages were streamed\nevents: %s", tr.path)
	}
	for _, message := range subagent {
		if got := message.Get("message.model").String(); !strings.Contains(strings.ToLower(got), "claude") {
			t.Fatalf("subagent message from %q, want a Claude model\nevents: %s", got, tr.path)
		}
	}
	mustContain(t, tr, tr.answer(), "3")
}

// testSwitchFromClaude starts on a Claude model, continues on the added model
// and returns to Claude. Claude Code sends Anthropic message-thread continues
// after a Claude turn, which the added model's provider cannot serve; this
// catches the turn losing its tools and history.
func testSwitchFromClaude(t *testing.T, model string) {
	s := newSession(t)
	s.write("notes.txt", "line 1\n")

	s.ask(claudeModel(), "Read notes.txt and tell me its content.", "--effort", "high")

	tr := s.ask(model, "Append the line 'line X' to notes.txt using Bash, then read the file with the Read tool and show it.")
	if got := s.read("notes.txt"); !strings.Contains(got, "line X") {
		t.Fatalf("notes.txt was not appended to:\n%s\nevents: %s", got, tr.path)
	}
	if len(tr.toolUses("Read")) == 0 {
		t.Fatalf("the %s turn did not use Read; it may have lost its tools\n%s\nevents: %s", model, tr.answer(), tr.path)
	}
	mustContain(t, tr, tr.answer(), "line X")

	back := s.ask(claudeModel(), "Without using any tools: which lines does notes.txt contain now?")
	mustContain(t, back, back.answer(), "line X")
}

// testCompact summarises the session on the added model and keeps going.
func testCompact(t *testing.T, model string) {
	s := newSession(t)
	s.ask(model, "Create a file named compact-marker.txt containing the word giraffe, using the Write tool.")
	if got := s.read("compact-marker.txt"); !strings.Contains(got, "giraffe") {
		t.Fatalf("compact-marker.txt = %q", got)
	}
	s.ask(model, "/compact")
	tr := s.ask(model, "Without using any tools: what is the name of the file you created, and which word is in it?")
	mustContain(t, tr, tr.answer(), "compact-marker.txt", "giraffe")
}

// assertMainModel checks that the main agent's messages report the model the
// session asked for, which is what the client sees for an added model.
func assertMainModel(t *testing.T, tr *turn, model string) {
	t.Helper()
	for _, message := range tr.assistant(false) {
		if got := message.Get("message.model").String(); !strings.EqualFold(got, model) {
			t.Fatalf("main agent message from %q, want %q\nevents: %s", got, model, tr.path)
		}
	}
}
