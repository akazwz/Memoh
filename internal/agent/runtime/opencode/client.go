package opencode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// The modeled subset is verified against OpenCode v1.18.31's /doc schema.
// SDK package versions do not select a different wire protocol.
const ProtocolVersion = "1.18.31"

type apiClient struct {
	http      *http.Client
	baseURL   string
	password  string
	directory string
}

type httpError struct{ status int }

func (e *httpError) Error() string { return fmt.Sprintf("opencode HTTP status %d", e.status) }

func (c *apiClient) request(ctx context.Context, method, endpoint string, input any) (*http.Response, error) {
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(data)
	}
	u, err := url.Parse(c.baseURL + endpoint)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("directory", c.directory)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth("opencode", c.password)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req) //nolint:gosec // The loopback listener is validated at startup and the transport only dials through the bot-owned bridge.
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		// Provider responses can contain credentials or private diagnostics.
		return nil, &httpError{status: resp.StatusCode}
	}
	return resp, nil
}

func (c *apiClient) call(ctx context.Context, method, endpoint string, input, output any) error {
	resp, err := c.request(ctx, method, endpoint, input)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if output == nil {
		_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, 32<<20))
		return err
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(output)
}

type nativeEvent struct {
	Type       string          `json:"type"`
	Properties json.RawMessage `json:"properties"`
}

// subscribe waits for server.connected before allowing prompt dispatch, so
// a fast first response or permission request cannot outrun the subscription.
func (c *apiClient) subscribe(ctx context.Context) (<-chan nativeEvent, <-chan error, error) {
	resp, err := c.request(ctx, http.MethodGet, "/event", nil) //nolint:bodyclose // The reader goroutine owns and closes the body for the subscription lifetime.
	if err != nil {
		return nil, nil, err
	}
	events := make(chan nativeEvent, 256)
	errorsCh := make(chan error, 1)
	ready := make(chan struct{})
	go func() {
		defer func() { _ = resp.Body.Close() }()
		defer close(events)
		connected := false
		err := readEvents(resp.Body, func(ev nativeEvent) error {
			if ev.Type == "server.connected" && !connected {
				connected = true
				close(ready)
				return nil
			}
			select {
			case events <- ev:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		errorsCh <- err
	}()
	select {
	case <-ready:
		return events, errorsCh, nil
	case err := <-errorsCh:
		return nil, nil, err
	case <-ctx.Done():
		_ = resp.Body.Close()
		return nil, nil, ctx.Err()
	}
}

func readEvents(r io.Reader, emit func(nativeEvent) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), 32<<20)
	var data strings.Builder
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if data.Len() == 0 {
				continue
			}
			var ev nativeEvent
			if err := json.Unmarshal([]byte(data.String()), &ev); err != nil {
				return fmt.Errorf("decode opencode event: %w", err)
			}
			data.Reset()
			if err := emit(ev); err != nil {
				return err
			}
		} else if strings.HasPrefix(line, "data:") {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			if data.Len() > 32<<20 {
				return errors.New("opencode event exceeds size limit")
			}
		}
	}
	return scanner.Err()
}

type nativeMessage struct {
	Info struct {
		ID       string          `json:"id"`
		Role     string          `json:"role"`
		ParentID string          `json:"parentID"`
		Summary  json.RawMessage `json:"summary"`
		Finish   string          `json:"finish"`
		Model    struct {
			ProviderID string `json:"providerID"`
			ModelID    string `json:"modelID"`
		} `json:"model"`
		Error *struct {
			Name string `json:"name"`
		} `json:"error"`
		Tokens struct {
			Input     int `json:"input"`
			Output    int `json:"output"`
			Reasoning int `json:"reasoning"`
			Cache     struct {
				Read  int `json:"read"`
				Write int `json:"write"`
			} `json:"cache"`
		} `json:"tokens"`
	} `json:"info"`
	Parts []nativePart `json:"parts"`
}

type nativePart struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionID"`
	MessageID string `json:"messageID"`
	Type      string `json:"type"`
	Text      string `json:"text"`
	CallID    string `json:"callID"`
	Tool      string `json:"tool"`
	State     struct {
		Status string         `json:"status"`
		Input  map[string]any `json:"input"`
		Output string         `json:"output"`
		Error  string         `json:"error"`
	} `json:"state"`
}
