package grok

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/felinics/memoh/internal/agent/runtime/agentstate"
	"github.com/felinics/memoh/internal/agent/runtime/external"
)

type promptBoundary struct {
	SessionID       string `json:"session_id"`
	PromptID        string `json:"prompt_id"`
	NextPromptIndex int    `json:"next_prompt_index"`
}
type forkSeed struct {
	SourceSessionID string `json:"source_session_id"`
	SourceRunID     string `json:"source_run_id"`
	TargetSessionID string `json:"target_session_id"`
	NextPromptIndex int    `json:"next_prompt_index"`
}

func (l *lease) promptCount(ctx context.Context) (int, error) {
	response, err := call[struct {
		PromptStarts []int `json:"promptStarts"`
	}](ctx, l.t, "_x.ai/session/updates", map[string]any{"sessionId": l.t.sessionID, "cwd": l.cwd, "offset": 0, "limit": 0})
	return len(response.PromptStarts), err
}

func encodeBoundary(sessionID, promptID string, next int) string {
	if !safeSessionID(sessionID) || promptID == "" || next < 1 {
		return ""
	}
	raw, _ := json.Marshal(promptBoundary{sessionID, promptID, next})
	return "grok:" + base64.RawURLEncoding.EncodeToString(raw)
}

func decodeBoundary(value string) (promptBoundary, error) {
	var boundary promptBoundary
	if len(value) > 4096 || len(value) < 6 || value[:5] != "grok:" {
		return boundary, errors.New("missing Grok prompt boundary")
	}
	raw, err := base64.RawURLEncoding.DecodeString(value[5:])
	if err != nil || json.Unmarshal(raw, &boundary) != nil || !safeSessionID(boundary.SessionID) || boundary.PromptID == "" || boundary.NextPromptIndex < 1 {
		return boundary, errors.New("invalid Grok prompt boundary")
	}
	return boundary, nil
}

// The SQL fork transaction copies the expected published snapshot into a
// target-owned seed alongside visible history. Native forking happens on the
// target's first lease, so neither the source's files nor its head advance.
func (d *Driver) ForkThread(ctx context.Context, botID, botAgentID string, metadata map[string]any, lastTurnID string) (map[string]any, error) {
	if _, err := d.agents.Get(ctx, botID, botAgentID); err != nil {
		return nil, err
	}
	boundary, err := decodeBoundary(lastTurnID)
	if err != nil {
		return nil, external.ErrThreadUnavailable
	}
	threadID := metaString(metadata, "grok_thread_id")
	head, found, err := d.stateStore.Head(ctx, botID, threadID)
	if err != nil {
		return nil, runtimeError(err)
	}
	if !found || head.Kind != agentstate.SessionPublicationCheckpoint || boundary.SessionID != metaString(metadata, "grok_session_id") {
		return nil, external.ErrThreadUnavailable
	}
	seed := forkSeed{SourceSessionID: boundary.SessionID, SourceRunID: head.RunID, TargetSessionID: uuid.NewString(), NextPromptIndex: boundary.NextPromptIndex}
	return map[string]any{"grok_fork": seed, "grok_session_id": seed.TargetSessionID, "grok_thread_id": ""}, nil
}

func decodeFork(metadata map[string]any) (forkSeed, bool, error) {
	var seed forkSeed
	value, ok := metadata["grok_fork"]
	if !ok || value == nil {
		return seed, false, nil
	}
	raw, err := json.Marshal(value)
	if err != nil || json.Unmarshal(raw, &seed) != nil || !safeSessionID(seed.SourceSessionID) || !safeSessionID(seed.TargetSessionID) || seed.SourceSessionID == seed.TargetSessionID || seed.NextPromptIndex < 1 {
		return seed, true, errors.New("invalid Grok fork seed")
	}
	if _, err = uuid.Parse(seed.SourceRunID); err != nil {
		return seed, true, err
	}
	return seed, true, nil
}

func (l *lease) fork(ctx context.Context, seed forkSeed) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	response, err := call[struct {
		NewSessionID string `json:"newSessionId"`
	}](ctx, l.t, "_x.ai/session/fork", map[string]any{"sourceSessionId": seed.SourceSessionID, "sourceCwd": l.cwd, "newCwd": l.cwd, "newSessionId": seed.TargetSessionID, "targetPromptIndex": seed.NextPromptIndex})
	if err != nil {
		return "", err
	}
	if response.NewSessionID != seed.TargetSessionID {
		return "", errors.New("grok fork identity mismatch")
	}
	return response.NewSessionID, nil
}
