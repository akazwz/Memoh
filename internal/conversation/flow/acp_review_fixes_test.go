package flow

import (
	"testing"
	"time"
)

// TestACPTurnStreamEvictedAfterTTL locks the registry-leak fix: a finished
// turn's snapshot stays readable for a grace window, then is evicted so the
// registry doesn't retain a per-session snapshot for the whole process life.
func TestACPTurnStreamEvictedAfterTTL(t *testing.T) {
	old := acpTurnSnapshotTTL
	acpTurnSnapshotTTL = 50 * time.Millisecond
	defer func() { acpTurnSnapshotTTL = old }()

	r := &Resolver{}
	s := r.beginACPTurnStream("bot-1", "sess-evict")
	s.turnStarted("turn-1")
	if _, ok := r.ACPTurnSnapshot("sess-evict"); !ok {
		t.Fatal("snapshot not registered after turnStarted")
	}

	s.finish("end")
	// Readable immediately after finish (the reconnect grace window).
	if _, ok := r.ACPTurnSnapshot("sess-evict"); !ok {
		t.Fatal("snapshot evicted before its TTL grace window")
	}

	// After the TTL it must be gone.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := r.ACPTurnSnapshot("sess-evict"); !ok {
			return // evicted as expected
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("snapshot was not evicted after the TTL")
}

// TestACPContextFileTitleIsRuneSafe locks the rune-safe title casing. The prior
// implementation sliced bytes (field[:1]/field[1:]), which split a multi-byte
// rune and injected a mojibake "## <title>" heading into the agent's context
// for any non-ASCII workspace filename.
func TestACPContextFileTitleIsRuneSafe(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"CONTRIBUTING.md", "Contributing"},
		{"design-notes.md", "Design Notes"},
		{"runbook_prod.md", "Runbook Prod"},
		{"über.md", "Über"},          // multi-byte leading rune
		{"naïve_plan.md", "Naïve Plan"}, // multi-byte interior rune
		{"API.md", "API"},           // short acronym kept as-is
		{"TODO.md", "TODO"},
	}
	for _, c := range cases {
		if got := acpContextFileTitle(c.in); got != c.want {
			t.Errorf("acpContextFileTitle(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
