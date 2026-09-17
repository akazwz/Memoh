package providers

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestGrokDeviceAuthorizationUsesOfficialClientAndHandlesPolling(t *testing.T) {
	calls := 0
	s := &Service{httpClient: &http.Client{Transport: claudeOAuthTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.Host != "auth.x.ai" || req.Header.Get("x-grok-client-version") != GrokCLIVersion {
			t.Fatalf("unexpected endpoint %s", req.URL)
		}
		if err := req.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if req.Form.Get("client_id") != GrokOAuthClientID {
			t.Fatal("wrong client")
		}
		body := ""
		status := http.StatusOK
		switch calls {
		case 1:
			if req.URL.Path != "/oauth2/device/code" || !strings.Contains(req.Form.Get("scope"), "offline_access") {
				t.Fatal("wrong authorization form")
			}
			body = `{"device_code":"device-test","user_code":"ABCD","verification_uri":"https://accounts.x.ai/device","expires_in":900,"interval":5}`
		case 2:
			status = 400
			body = `{"error":"authorization_pending"}`
		case 3:
			status = 400
			body = `{"error":"slow_down"}`
		case 4:
			if req.Form.Get("device_code") != "device-test" || req.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" {
				t.Fatal("wrong polling form")
			}
			body = `{"access_token":"access-test","refresh_token":"refresh-test","expires_in":3600}`
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}}
	auth, err := s.StartGrokDeviceAuthorization(context.Background())
	if err != nil || auth.UserCode != "ABCD" {
		t.Fatalf("authorize: %#v, %v", auth, err)
	}
	for i := 0; i < 3; i++ {
		r, e := s.PollGrokDeviceAuthorization(context.Background(), auth.DeviceCode)
		if e != nil {
			t.Fatal(e)
		}
		if i < 2 && (!r.Pending || r.SlowDown != (i == 1)) {
			t.Fatalf("poll %d: %#v", i, r)
		}
		if i == 2 && (r.Secret["access_token"] != "access-test" || r.Secret["refresh_token"] != "refresh-test") {
			t.Fatal("missing token pair")
		}
	}
}

func TestGrokOAuthRejectsUntrustedVerificationLinksAndPrivateDiagnostics(t *testing.T) {
	for _, body := range []string{`{"device_code":"secret","user_code":"code","verification_uri":"https://evil.example/device","expires_in":900}`, `{"error_description":"secret diagnostic"}`} {
		s := &Service{httpClient: &http.Client{Transport: claudeOAuthTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
		})}}
		_, err := s.StartGrokDeviceAuthorization(context.Background())
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe error: %v", err)
		}
	}
}
