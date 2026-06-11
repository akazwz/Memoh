package flow

import (
	"strings"
	"testing"

	"github.com/memohai/memoh/internal/conversation"
)

func TestRenderACPHistoryMarkdown(t *testing.T) {
	t.Parallel()

	messages := []messageWithUsage{
		{Message: conversation.ModelMessage{Role: "user", Content: conversation.NewTextContent("build the exporter")}},
		{Message: conversation.ModelMessage{Role: "assistant", Content: conversation.NewTextContent("done: exporter written to cmd/export")}},
		{Message: conversation.ModelMessage{Role: "tool", Content: conversation.NewTextContent("raw tool output noise")}},
		{Message: conversation.ModelMessage{Role: "assistant", Content: conversation.NewTextContent("")}},
	}
	got := renderACPHistoryMarkdown(messages)
	if !strings.Contains(got, "# Previous Conversation") {
		t.Fatalf("history markdown missing header: %q", got)
	}
	if !strings.Contains(got, "### User\n\nbuild the exporter") {
		t.Fatalf("history markdown missing user turn: %q", got)
	}
	if !strings.Contains(got, "### Assistant\n\ndone: exporter written to cmd/export") {
		t.Fatalf("history markdown missing assistant turn: %q", got)
	}
	if strings.Contains(got, "raw tool output noise") {
		t.Fatalf("history markdown leaked tool output: %q", got)
	}
}

func TestRenderACPHistoryMarkdownEmpty(t *testing.T) {
	t.Parallel()

	if got := renderACPHistoryMarkdown(nil); got != "" {
		t.Fatalf("empty history rendered %q, want empty", got)
	}
	onlyNoise := []messageWithUsage{
		{Message: conversation.ModelMessage{Role: "tool", Content: conversation.NewTextContent("noise")}},
		{Message: conversation.ModelMessage{Role: "user", Content: conversation.NewTextContent("   ")}},
	}
	if got := renderACPHistoryMarkdown(onlyNoise); got != "" {
		t.Fatalf("noise-only history rendered %q, want empty", got)
	}
}
