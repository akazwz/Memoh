package agentsession

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/felinics/memoh/internal/agent/runtime/agentstate"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
	postgresstore "github.com/felinics/memoh/internal/db/postgres/store"
	"github.com/felinics/memoh/internal/runtimefence"
)

func TestPostgresGrokImmutableRevisionsAndIndependentForkSeed(t *testing.T) {
	ctx := context.Background()
	pool := openACPStatePostgresPool(t, ctx)
	bot, session := createACPStatePostgresFixture(t, ctx, pool)
	q := dbsqlc.New(pool)
	store := NewStateStore(postgresstore.NewQueriesWithPool(pool, q))
	if _, err := pool.Exec(ctx, "UPDATE bot_sessions SET runtime_type='grok' WHERE id=$1", session); err != nil {
		t.Fatal(err)
	}
	finish := func(run string) {
		t.Helper()
		if _, err := pool.Exec(ctx, "UPDATE session_runs SET state='completed' WHERE run_id=$1", run); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := func(run string, token int64, content string) (agentstate.PersistedSessionState, agentstate.SessionStateRecordReader) {
		state, reader := multiRecordACPState(t, run, token, "grok-snapshot-v1.jsonl", 0, content)
		state.AgentID = "grok"
		state.AgentSessionID = "native-source"
		state.StorageRevision = run
		return state, reader
	}
	load := func(thread, expectedRun, content string) {
		t.Helper()
		found, err := store.Load(ctx, bot, thread, func(ctx context.Context, state agentstate.PersistedSessionState, next agentstate.SessionStateRecordReader) error {
			if state.ThroughRunID != expectedRun || state.StorageRevision != expectedRun {
				return fmt.Errorf("wrong revision: %#v", state)
			}
			record, err := next(ctx)
			if err != nil {
				return err
			}
			if string(record.Content) != content {
				return fmt.Errorf("wrong bytes %s", record.Content)
			}
			if _, err = next(ctx); !errors.Is(err, io.EOF) {
				return fmt.Errorf("expected EOF: %w", err)
			}
			return nil
		})
		if err != nil || !found {
			t.Fatalf("load %s: %t, %v", thread, found, err)
		}
	}
	run1, turn1, fence1 := createACPStateRun(t, ctx, q, pool, bot, session)
	state, reader := snapshot(run1, fence1.Token, `{"native":"before compaction"}`)
	if err := store.Replace(runtimefence.WithContext(ctx, fence1), bot, session, state, reader); err != nil {
		t.Fatal(err)
	}
	persistACPStateWatermark(t, ctx, pool, bot, session, run1, turn1)
	// Even a still-running publisher may not rewrite its published revision.
	state, reader = snapshot(run1, fence1.Token, `{"native":"corrupt"}`)
	if err := store.Replace(runtimefence.WithContext(ctx, fence1), bot, session, state, reader); !errors.Is(err, agentstate.ErrSessionStateDivergent) {
		t.Fatalf("rewrote published revision: %v", err)
	}
	finish(run1)
	run2, turn2, fence2 := createACPStateRun(t, ctx, q, pool, bot, session)
	state, reader = snapshot(run2, fence2.Token, `{"native":"compacted"}`)
	if err := store.Replace(runtimefence.WithContext(ctx, fence2), bot, session, state, reader); err != nil {
		t.Fatal(err)
	}
	load(session, run1, `{"native":"before compaction"}`)
	// The fork transaction selects the expected published revision, not the
	// newer unpublished candidate. Its bytes must survive source deletion.
	raw, _ := json.Marshal(map[string]any{"grok_session_id": "native-target", "grok_fork": map[string]any{"source_run_id": run1}})
	params := dbsqlc.ForkSessionFromAssistantTurnParams{SessionID: mustACPStateUUID(t, session), BotID: mustACPStateUUID(t, bot), TurnID: mustACPStateUUID(t, turn1), Metadata: []byte(`{}`), RuntimeMetadataOverride: raw, Title: "Grok fork"}
	target, err := q.ForkSessionFromAssistantTurn(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	load(target.ID.String(), run1, `{"native":"before compaction"}`)
	persistACPStateWatermark(t, ctx, pool, bot, session, run2, turn2)
	finish(run2)
	load(session, run2, `{"native":"compacted"}`)
	// A source that advanced after preparation cannot create a fork with a
	// mismatched visible-history/native-state cut.
	if _, err = q.ForkSessionFromAssistantTurn(ctx, params); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale fork accepted: %v", err)
	}
	if _, err = pool.Exec(ctx, "DELETE FROM bot_sessions WHERE id=$1", session); err != nil {
		t.Fatal(err)
	}
	load(target.ID.String(), run1, `{"native":"before compaction"}`)
	// First target publication shadows its seed; the next stage prunes the
	// seed without deleting any lines referenced by the current target head.
	run3, turn3, fence3 := createACPStateRun(t, ctx, q, pool, bot, target.ID.String())
	state, reader = snapshot(run3, fence3.Token, `{"native":"fork continued"}`)
	if err = store.Replace(runtimefence.WithContext(ctx, fence3), bot, target.ID.String(), state, reader); err != nil {
		t.Fatal(err)
	}
	load(target.ID.String(), run1, `{"native":"before compaction"}`)
	persistACPStateWatermark(t, ctx, pool, bot, target.ID.String(), run3, turn3)
	finish(run3)
	run4, _, fence4 := createACPStateRun(t, ctx, q, pool, bot, target.ID.String())
	state, reader = snapshot(run4, fence4.Token, `{"native":"next candidate"}`)
	if err = store.Replace(runtimefence.WithContext(ctx, fence4), bot, target.ID.String(), state, reader); err != nil {
		t.Fatal(err)
	}
	load(target.ID.String(), run3, `{"native":"fork continued"}`)
	var seeds int
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM agent_session_fork_states WHERE session_id=$1", target.ID).Scan(&seeds); err != nil || seeds != 0 {
		t.Fatalf("fork seed not pruned: %d %v", seeds, err)
	}
}

func TestPostgresGrokFailedStageKeepsPublishedBytes(t *testing.T) {
	ctx := context.Background()
	pool := openACPStatePostgresPool(t, ctx)
	bot, session := createACPStatePostgresFixture(t, ctx, pool)
	q := dbsqlc.New(pool)
	store := NewStateStore(postgresstore.NewQueriesWithPool(pool, q))
	run1, turn1, fence1 := createACPStateRun(t, ctx, q, pool, bot, session)
	state, reader := multiRecordACPState(t, run1, fence1.Token, "snapshot.jsonl", 0, `{"v":1}`)
	state.StorageRevision = run1
	if err := store.Replace(runtimefence.WithContext(ctx, fence1), bot, session, state, reader); err != nil {
		t.Fatal(err)
	}
	persistACPStateWatermark(t, ctx, pool, bot, session, run1, turn1)
	if _, err := pool.Exec(ctx, "UPDATE session_runs SET state='completed' WHERE run_id=$1", run1); err != nil {
		t.Fatal(err)
	}
	run2, _, fence2 := createACPStateRun(t, ctx, q, pool, bot, session)
	state, reader = multiRecordACPState(t, run2, fence2.Token, "snapshot.jsonl", 0, `{"v":2}`, `{"v":3}`)
	state.StorageRevision = run2
	calls := 0
	failure := errors.New("capture interrupted")
	next := func(ctx context.Context) (agentstate.SessionStateRecord, error) {
		calls++
		if calls == 2 {
			return agentstate.SessionStateRecord{}, failure
		}
		return reader(ctx)
	}
	if err := store.Replace(runtimefence.WithContext(ctx, fence2), bot, session, state, next); !errors.Is(err, failure) {
		t.Fatalf("stage failure: %v", err)
	}
	var old string
	var candidates int
	if err := pool.QueryRow(ctx, "SELECT content FROM agent_session_state_lines WHERE session_id=$1 AND storage_revision=$2", session, run1).Scan(&old); err != nil || old != `{"v":1}` {
		t.Fatalf("published bytes corrupted: %s %v", old, err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM agent_session_states WHERE session_id=$1 AND through_run_id=$2", session, run2).Scan(&candidates); err != nil || candidates != 0 {
		t.Fatalf("failed stage survived: %d %v", candidates, err)
	}
}
