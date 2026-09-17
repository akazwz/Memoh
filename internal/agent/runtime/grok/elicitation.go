package grok

import (
	"context"
	"encoding/json"

	acp "github.com/coder/acp-go-sdk"

	userinput "github.com/felinics/memoh/internal/agent/decision/input"
)

func (t *turnSession) elicit(ctx context.Context, raw json.RawMessage) (any, *acp.RequestError) {
	var p struct {
		ToolCallID string         `json:"toolCallId"`
		Mode       string         `json:"mode"`
		Message    string         `json:"message"`
		URL        string         `json:"url"`
		Schema     map[string]any `json:"requestedSchema"`
	}
	if json.Unmarshal(raw, &p) != nil || p.ToolCallID == "" {
		return nil, acp.NewInvalidParams(nil)
	}
	decline := map[string]any{"outcome": "decline"}
	canceled := map[string]any{"outcome": "cancel"}
	if t.driver.userInput == nil || !t.input.CanRequestUserInput {
		return decline, nil
	}
	var input any
	var mapping userinput.ElicitationFormMapping
	var err error
	switch p.Mode {
	case "form":
		input, mapping, err = userinput.ElicitationFormInput(p.Message, p.Schema)
	case "url":
		input, err = userinput.ElicitationURLInput(p.Message, p.URL)
	default:
		return decline, nil
	}
	if err != nil {
		return decline, nil
	}
	result, err := t.requestInput(ctx, p.ToolCallID, input, "grok_mcp_elicitation")
	if err != nil || ctx.Err() != nil {
		return canceled, nil
	}
	if result.Status != userinput.StatusSubmitted {
		if reason, _ := result.Result["reason"].(string); reason == "user_canceled" {
			return decline, nil
		}
		return canceled, nil
	}
	if p.Mode == "url" {
		answers := userinput.AnswersFromResult(result.Result)
		if len(answers) == 1 && !answers[0].Skipped && len(answers[0].Selected) == 1 && answers[0].Selected[0].ID == "q1.o1" {
			return map[string]any{"outcome": "accept"}, nil
		}
		return decline, nil
	}
	content, err := mapping.Content(result)
	if err != nil {
		return canceled, nil
	}
	return map[string]any{"outcome": "accept", "content": content}, nil
}
