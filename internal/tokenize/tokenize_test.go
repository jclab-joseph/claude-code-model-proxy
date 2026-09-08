package tokenize

import (
	"strings"
	"testing"
)

func TestEstimateGrowsWithContent(t *testing.T) {
	small := EstimateClaudeRequest([]byte(`{"messages":[{"role":"user","content":"hi"}]}`))
	large := EstimateClaudeRequest([]byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("word ", 1000) + `"}]}`))

	if small < 1 {
		t.Fatalf("estimate = %d, want at least 1", small)
	}
	if large <= small*10 {
		t.Fatalf("estimate did not scale with content: %d then %d", small, large)
	}
}

func TestEstimateCountsToolsSystemAndToolResults(t *testing.T) {
	body := `{
		"system":[{"type":"text","text":"` + strings.Repeat("s", 350) + `"}],
		"messages":[
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"` + strings.Repeat("t", 350) + `"},
				{"type":"tool_use","name":"Read","input":{"file_path":"/x"}}
			]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"1","content":[{"type":"text","text":"` + strings.Repeat("r", 350) + `"}]}]}
		],
		"tools":[{"name":"Read","description":"` + strings.Repeat("d", 350) + `","input_schema":{"type":"object","properties":{}}}]
	}`

	got := EstimateClaudeRequest([]byte(body))
	// Four 350-character fields at ~3.5 characters per token is around 400.
	if got < 300 || got > 600 {
		t.Fatalf("estimate = %d, want roughly 400 for ~1400 characters", got)
	}
}

func TestEstimateHandlesEmptyRequest(t *testing.T) {
	if got := EstimateClaudeRequest([]byte(`{}`)); got != 1 {
		t.Fatalf("estimate = %d, want the 1-token floor", got)
	}
}
