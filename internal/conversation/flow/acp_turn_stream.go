package flow

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	agentpkg "github.com/memohai/memoh/internal/agent"
	"github.com/memohai/memoh/internal/conversation"
	messageevent "github.com/memohai/memoh/internal/message/event"
)

// ACPTurnSnapshot is the reconnect backfill for an ACP turn: the UI messages
// accumulated server-side so far, plus the turn identity a client needs to
// dedupe the live stream and target an abort. Messages carry the same shape
// as live `acp_turn_stream` "message" payloads (upsert by message ID).
type ACPTurnSnapshot struct {
	SessionID string                   `json:"session_id"`
	TurnID    string                   `json:"turn_id,omitempty"`
	Active    bool                     `json:"active"`
	UpdatedAt time.Time                `json:"updated_at,omitempty"`
	Messages  []conversation.UIMessage `json:"messages"`
}

// acpTurnStream mirrors one ACP turn onto the bot event hub
// (EventTypeACPTurnStream) and keeps an in-memory snapshot so a client that
// (re)connects mid-turn can backfill what it missed. It runs on the event
// sink path, deliberately independent of the WS stream context: a headless
// turn (originating connection gone) keeps publishing.
//
// The stream registers itself in the resolver's registry only once the pool
// grants the turn (turnStarted), so a busy-rejected prompt can never clobber
// the running turn's snapshot.
type acpTurnStream struct {
	resolver  *Resolver
	botID     string
	sessionID string

	mu        sync.Mutex
	turnID    string
	active    bool
	finished  bool
	converter *conversation.UIMessageStreamConverter
	order     []int
	messages  map[int]conversation.UIMessage
	updatedAt time.Time
}

func (r *Resolver) beginACPTurnStream(botID, sessionID string) *acpTurnStream {
	return &acpTurnStream{
		resolver:  r,
		botID:     strings.TrimSpace(botID),
		sessionID: strings.TrimSpace(sessionID),
		converter: conversation.NewUIMessageStreamConverter(),
		messages:  map[int]conversation.UIMessage{},
	}
}

// turnStarted records the pool-granted turn identity, registers the stream
// for snapshot lookups, and announces the turn. Tolerates being called again
// on a pool-internal retry (the fresh turn ID simply supersedes).
func (s *acpTurnStream) turnStarted(turnID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.turnID = strings.TrimSpace(turnID)
	s.active = true
	s.updatedAt = time.Now()
	s.mu.Unlock()
	if s.resolver != nil && s.sessionID != "" {
		s.resolver.acpTurnStreams.Store(s.sessionID, s)
	}
	s.publish(map[string]any{"type": "start"})
}

// handleEvent feeds one agent stream event through the server-side UI
// converter, folds the result into the snapshot, and mirrors it to the hub.
func (s *acpTurnStream) handleEvent(event agentpkg.StreamEvent) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.finished {
		s.mu.Unlock()
		return
	}
	uiMessages := s.converter.HandleEvent(conversation.UIStreamEventFromAgent(event))
	for _, message := range uiMessages {
		if _, seen := s.messages[message.ID]; !seen {
			s.order = append(s.order, message.ID)
		}
		s.messages[message.ID] = message
	}
	if len(uiMessages) > 0 {
		s.updatedAt = time.Now()
	}
	active := s.active
	s.mu.Unlock()
	if !active {
		// Pre-grant events (the synthesized agent_start) accumulate but are
		// not broadcast: without a turn ID a client cannot dedupe them.
		return
	}
	for _, message := range uiMessages {
		s.publish(map[string]any{"type": "message", "data": message})
	}
}

// finish marks the turn terminal and announces the outcome ("end",
// "cancelled", "error"). Idempotent; a stream that never got a turn granted
// announces nothing. The snapshot stays readable (Active=false) until the
// session's next turn replaces it — the persisted round covers everything
// after that.
func (s *acpTurnStream) finish(outcome string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.finished {
		s.mu.Unlock()
		return
	}
	s.finished = true
	wasActive := s.active
	s.active = false
	s.updatedAt = time.Now()
	s.mu.Unlock()
	if wasActive {
		s.publish(map[string]any{"type": "end", "outcome": outcome})
	}
	s.scheduleEviction()
}

// acpTurnSnapshotTTL is how long a finished turn's snapshot stays readable for
// reconnect backfill before it is evicted from the registry. After this the
// persisted round is the whole story. A var (not const) so tests can shorten it.
var acpTurnSnapshotTTL = 5 * time.Minute

// scheduleEviction drops this finished stream from the resolver registry after
// the snapshot TTL, so the registry doesn't retain a per-session snapshot for
// the entire process lifetime (an unbounded leak as sessions accumulate). A
// newer turn on the same session replaces the registry entry via Store, so we
// only evict when THIS stream is still the registered one.
func (s *acpTurnStream) scheduleEviction() {
	r := s.resolver
	if r == nil || s.sessionID == "" {
		return
	}
	time.AfterFunc(acpTurnSnapshotTTL, func() {
		if cur, ok := r.acpTurnStreams.Load(s.sessionID); ok && cur == s {
			r.acpTurnStreams.Delete(s.sessionID)
		}
	})
}

func (s *acpTurnStream) publish(stream map[string]any) {
	if s == nil || s.resolver == nil || s.resolver.eventPublisher == nil {
		return
	}
	s.mu.Lock()
	turnID := s.turnID
	s.mu.Unlock()
	payload := map[string]any{
		"session_id": s.sessionID,
		"turn_id":    turnID,
		"stream":     stream,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	s.resolver.eventPublisher.Publish(messageevent.Event{
		Type:  messageevent.EventTypeACPTurnStream,
		BotID: s.botID,
		Data:  data,
	})
}

// ACPTurnSnapshot returns the session's current (or most recent) turn
// snapshot for reconnect backfill. ok is false when no turn ran for the
// session in this process — the persisted history is then the whole story.
func (r *Resolver) ACPTurnSnapshot(sessionID string) (ACPTurnSnapshot, bool) {
	if r == nil {
		return ACPTurnSnapshot{}, false
	}
	value, ok := r.acpTurnStreams.Load(strings.TrimSpace(sessionID))
	if !ok {
		return ACPTurnSnapshot{}, false
	}
	s, ok := value.(*acpTurnStream)
	if !ok {
		return ACPTurnSnapshot{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot := ACPTurnSnapshot{
		SessionID: s.sessionID,
		TurnID:    s.turnID,
		Active:    s.active,
		UpdatedAt: s.updatedAt,
		Messages:  make([]conversation.UIMessage, 0, len(s.order)),
	}
	for _, id := range s.order {
		snapshot.Messages = append(snapshot.Messages, s.messages[id])
	}
	return snapshot, true
}
