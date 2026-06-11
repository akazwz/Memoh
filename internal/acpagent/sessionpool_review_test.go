package acpagent

import (
	"fmt"
	"sync"
	"testing"

	"github.com/memohai/memoh/internal/acpclient"
)

// orderRecordingSink records the order in which the live sink observes events.
type orderRecordingSink struct {
	mu     sync.Mutex
	deltas []string
}

func (o *orderRecordingSink) EmitACPEvent(e acpclient.StreamEvent) {
	o.mu.Lock()
	o.deltas = append(o.deltas, e.Delta)
	o.mu.Unlock()
}

func (o *orderRecordingSink) snapshot() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.deltas...)
}

// TestPromptToolEventSinkLiveOrderMatchesSnapshot proves the live sink (next)
// observes events in the SAME order as the persisted snapshot (Events()) even
// when multiple goroutines (the session delta stream + the MCP tool-event
// stream) race this merge point. Run under -race. Before the fix the emit to
// `next` happened outside the lock, so the two orders could diverge.
func TestPromptToolEventSinkLiveOrderMatchesSnapshot(t *testing.T) {
	rec := &orderRecordingSink{}
	sink := newPromptToolEventSink(rec)

	const goroutines, perG = 4, 200
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				sink.EmitACPEvent(acpclient.StreamEvent{
					Type:  acpclient.StreamEventTextDelta,
					Delta: fmt.Sprintf("g%d-%d", g, i),
				})
			}
		}(g)
	}
	wg.Wait()

	snapshot := sink.Events()
	live := rec.snapshot()
	if len(snapshot) != goroutines*perG {
		t.Fatalf("snapshot has %d events, want %d", len(snapshot), goroutines*perG)
	}
	if len(live) != len(snapshot) {
		t.Fatalf("live sink received %d events, snapshot has %d", len(live), len(snapshot))
	}
	for i := range snapshot {
		if snapshot[i].Delta != live[i] {
			t.Fatalf("order divergence at index %d: snapshot=%q live=%q", i, snapshot[i].Delta, live[i])
		}
	}
}

// TestTeardownClearsTurnSoStatusNeverReportsClosedRunning locks the invariant
// that a torn-down runtime never surfaces a running turn. Before the fix
// teardown left h.turn populated while flipping state to closed, and statusOf
// remapped closed->idle while still reading h.turn — so a concurrent
// RuntimeStatus could observe the impossible "idle + running turn".
func TestTeardownClearsTurnSoStatusNeverReportsClosedRunning(t *testing.T) {
	p := &SessionPool{
		runtimes:  map[string]*runtimeHandle{},
		bySession: map[string]string{},
	}
	h := &runtimeHandle{
		id:           "rt-1",
		botID:        "bot-1",
		agentID:      "codex",
		projectPath:  "/data/project",
		status:       stateActive,
		boundSession: "sess-1",
		turn:         &runtimeTurn{id: "turn-1", sessionID: "sess-1", state: turnStateRunning},
	}
	p.runtimes[h.id] = h
	p.bySession[h.boundSession] = h.id

	if before := p.statusOf(h); before.TurnID != "turn-1" || before.TurnState != turnStateRunning {
		t.Fatalf("pre-teardown status = %+v, want running turn turn-1", before)
	}

	if err := p.teardown(h, "test"); err != nil {
		t.Fatalf("teardown: %v", err)
	}

	if h.turn != nil {
		t.Fatalf("teardown did not clear h.turn")
	}
	after := p.statusOf(h)
	if after.TurnID != "" || after.TurnState != "" {
		t.Fatalf("post-teardown status still reports a turn: %+v", after)
	}
}
