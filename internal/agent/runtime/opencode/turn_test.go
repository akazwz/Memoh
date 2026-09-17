package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/agent/event"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	"github.com/felinics/memoh/internal/agent/runtime/opencode/opencodecfg"
)

type recordingSink struct {
	mu     sync.Mutex
	events []event.StreamEvent
}

func (s *recordingSink) EmitStreamEvent(e event.StreamEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
}

func testTurn(server *httptest.Server, input external.PromptInput) *turnRunner {
	if input.Sink == nil {
		input.Sink = &recordingSink{}
	}
	r := newTurn(input, nil, nil, slog.Default())
	r.api = &apiClient{http: server.Client(), baseURL: server.URL, password: "test-password", directory: "/data/a project"}
	return r
}

// This server exercises the ordering contract, not a model: a fast native
// prompt may finish while deltas remain in the SSE buffer. The persisted
// response must fill any gap without duplicating streamed text or tool calls.
func TestTurnStreamsThenReconcilesAndResumes(t *testing.T) {
	var mu sync.Mutex
	connected := make(chan struct{})
	frames := make(chan any, 16)
	var messageID string
	var posted map[string]any
	sessionCreates := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || user != "opencode" || password != "test-password" || r.URL.Query().Get("directory") != "/data/a project" {
			t.Error("missing HTTP auth or directory scope")
		}
		switch {
		case r.Method == http.MethodPatch:
			_, _ = fmt.Fprint(w, `{}`)
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			sessionCreates++
			_, _ = fmt.Fprint(w, `{"id":"ses_root"}`)
		case r.URL.Path == "/event":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "data: {\"type\":\"server.connected\"}\n\n")
			w.(http.Flusher).Flush()
			close(connected)
			for {
				select {
				case frame := <-frames:
					b, _ := json.Marshal(frame)
					_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
					w.(http.Flusher).Flush()
				case <-r.Context().Done():
					return
				}
			}
		case r.URL.Path == "/session/ses_root/message" && r.Method == http.MethodPost:
			select {
			case <-connected:
			default:
				t.Error("prompt outran event subscription")
			}
			mu.Lock()
			if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
				t.Error(err)
			}
			messageID, _ = posted["messageID"].(string)
			mu.Unlock()
			frames <- map[string]any{"type": "message.part.updated", "properties": map[string]any{"part": map[string]any{"id": "p1", "sessionID": "ses_root", "messageID": "msg_assistant", "type": "text", "text": "Hel"}}}
			frames <- map[string]any{"type": "message.part.delta", "properties": map[string]any{"sessionID": "ses_root", "messageID": "msg_assistant", "partID": "p1", "field": "text", "delta": "lo"}}
			_, _ = fmt.Fprint(w, `{"info":{"id":"msg_assistant","role":"assistant"},"parts":[]}`)
		case r.URL.Path == "/session/ses_root/message":
			mu.Lock()
			id := messageID
			mu.Unlock()
			_, _ = fmt.Fprintf(w, `[{"info":{"id":%q,"role":"user"},"parts":[]},{"info":{"id":"msg_assistant","role":"assistant","parentID":%q,"tokens":{"input":3,"output":4,"cache":{"read":5,"write":2}}},"parts":[{"id":"p1","sessionID":"ses_root","messageID":"msg_assistant","type":"text","text":"Hello world"},{"id":"tool1","sessionID":"ses_root","messageID":"msg_assistant","type":"tool","callID":"call1","tool":"bash","state":{"status":"completed","input":{"command":"pwd"},"output":"/data"}}]}]`, id, id)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
	server := httptest.NewServer(handler)
	defer server.Close()
	sink := &recordingSink{}
	turn := testTurn(server, external.PromptInput{Prompt: "hello", ContextMarkdown: "current context", ModelID: "openai/test-model", ReasoningEffort: "high", Sink: sink})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := turn.run(ctx, opencodecfg.Config{}); err != nil {
		t.Fatal(err)
	}
	turn.close()
	result := turn.result(true)
	if result.Text != "Hello world" || result.Usage == nil || result.Usage.TotalTokens != 14 {
		t.Fatalf("unexpected result: %+v", result)
	}
	var text strings.Builder
	starts, ends := 0, 0
	for _, ev := range sink.events {
		if ev.Type == event.TextDelta {
			text.WriteString(ev.Delta)
		}
		if ev.Type == event.ToolCallStart {
			starts++
		}
		if ev.Type == event.ToolCallEnd {
			ends++
		}
	}
	if text.String() != "Hello world" || starts != 1 || ends != 1 {
		t.Fatalf("duplicated/missing stream: text=%q starts=%d ends=%d", text.String(), starts, ends)
	}
	mu.Lock()
	if posted["system"] != "current context" || posted["variant"] != "high" {
		t.Errorf("prompt lost context/variant: %+v", posted)
	}
	mu.Unlock()
	resumed := testTurn(server, external.PromptInput{RuntimeMetadata: result.RuntimeMetadata})
	fresh, err := resumed.ensureSession(ctx)
	if err != nil || fresh || resumed.sessionID != "ses_root" || sessionCreates != 1 {
		t.Fatalf("resume = %v, %v, %s", fresh, err, resumed.sessionID)
	}
	// An uncommitted native tail must not silently enter canonical history.
	mu.Lock()
	messageID = "msg_foreign"
	mu.Unlock()
	fresh, err = resumed.ensureSession(ctx)
	if err != nil || !fresh || sessionCreates != 2 {
		t.Fatalf("divergent head = %v, %v", fresh, err)
	}
}

func TestStopAbortsNativeBeforeCancellingRequest(t *testing.T) {
	started := make(chan struct{})
	aborted := make(chan struct{})
	var messageID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPatch:
			_, _ = fmt.Fprint(w, `{}`)
		case r.URL.Path == "/session":
			_, _ = fmt.Fprint(w, `{"id":"ses_root"}`)
		case r.URL.Path == "/event":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "data: {\"type\":\"server.connected\"}\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case strings.HasSuffix(r.URL.Path, "/abort"):
			close(aborted)
			_, _ = fmt.Fprint(w, "true")
		case r.Method == http.MethodPost:
			var body struct {
				MessageID string `json:"messageID"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			messageID = body.MessageID
			close(started)
			select {
			case <-aborted:
				_, _ = fmt.Fprint(w, `{"info":{},"parts":[]}`)
			case <-r.Context().Done():
				t.Error("request cancelled before native abort")
			}
		default:
			_, _ = fmt.Fprintf(w, `[{"info":{"id":"msg_a","role":"assistant","parentID":%q},"parts":[{"id":"p1","sessionID":"ses_root","messageID":"msg_a","type":"text","text":"partial"}]}]`, messageID)
		}
	}))
	defer server.Close()
	turn := testTurn(server, external.PromptInput{Prompt: "long task"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- turn.run(ctx, opencodecfg.Config{}) }()
	select {
	case <-started:
		cancel()
	case <-ctx.Done():
		t.Fatal("prompt never started")
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("stop = %v", err)
	}
	turn.close()
	if turn.result(false).Text != "partial" {
		t.Fatal("partial response lost")
	}
}

func TestNativeChildDecisionsAndForeignSessionIsolation(t *testing.T) {
	replies := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/session/ses_child":
			_, _ = fmt.Fprint(w, `{"parentID":"ses_root"}`)
		case "/session/ses_foreign":
			_, _ = fmt.Fprint(w, `{}`)
		case "/permission/perm_child/reply":
			var body struct {
				Reply string `json:"reply"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			replies <- body.Reply
			_, _ = fmt.Fprint(w, "true")
		case "/question/q_child/reject":
			replies <- "question-rejected"
			_, _ = fmt.Fprint(w, "true")
		default:
			t.Errorf("foreign decision escaped: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	turn := testTurn(server, external.PromptInput{})
	turn.sessionID = "ses_root"
	for _, frame := range []string{
		`{"type":"permission.asked","properties":{"id":"perm_child","sessionID":"ses_child","permission":"bash","patterns":["pwd"]}}`,
		`{"type":"question.asked","properties":{"id":"q_child","sessionID":"ses_child","questions":[{"question":"Continue?"}]}}`,
		`{"type":"permission.asked","properties":{"id":"perm_foreign","sessionID":"ses_foreign","permission":"bash"}}`,
	} {
		var ev nativeEvent
		_ = json.Unmarshal([]byte(frame), &ev)
		if err := turn.handleEvent(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
	}
	turn.close()
	if len(replies) != 2 {
		t.Fatalf("got %d decisions, want 2", len(replies))
	}
	for range 2 {
		reply := <-replies
		if reply != "reject" && reply != "question-rejected" {
			t.Fatal(reply)
		}
	}
}

func TestStateAndPromptScope(t *testing.T) {
	if _, err := stateRoot("../escape", "thread"); err == nil {
		t.Fatal("unsafe state path accepted")
	}
	input := external.PromptInput{Prompt: "look", ContextMarkdown: "updated memory", ModelID: "openrouter/vendor/model", Images: []external.Image{{MimeType: "image/png", Data: []byte("png")}}}
	body, err := promptBody(input, opencodecfg.Config{}, "msg_x")
	if err != nil {
		t.Fatal(err)
	}
	if body["system"] != "updated memory" || body["model"].(map[string]string)["modelID"] != "vendor/model" {
		t.Fatal(body)
	}
	parts := body["parts"].([]map[string]any)
	if parts[1]["url"] != "data:image/png;base64,cG5n" {
		t.Fatal(parts)
	}
}

func TestUnsupportedNativeQuestionsAreRejected(t *testing.T) {
	for _, question := range []string{
		`{"question":"Choose","custom":false,"options":[{"label":"Only choice"}]}`,
		`{"question":"Choose","custom":false,"options":[]}`,
	} {
		t.Run(question, func(t *testing.T) {
			rejected := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/question/q1/reject" || r.Method != http.MethodPost {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				rejected = true
				_, _ = fmt.Fprint(w, "true")
			}))
			defer server.Close()
			turn := testTurn(server, external.PromptInput{CanRequestUserInput: true})
			turn.userInput = &answeringQuestions{}
			if err := turn.question(context.Background(), json.RawMessage(`{"id":"q1","questions":[`+question+`]}`)); err != nil {
				t.Fatal(err)
			}
			if !rejected {
				t.Fatal("native question was not rejected")
			}
		})
	}
}
