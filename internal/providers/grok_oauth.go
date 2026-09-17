package providers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Public OAuth client from the official Grok Build login implementation.
// These are protocol identifiers, not application secrets.
const (
	GrokOAuthIssuer   = "https://auth.x.ai"
	GrokOAuthClientID = "b1a00492-073a-47ea-816f-4c329264a828"
	GrokCLIVersion    = "1.0.30"
	grokOAuthScopes   = "openid profile email offline_access grok-cli:access api:access conversations:read conversations:write workspaces:read workspaces:write"
)

type GrokDeviceAuthorization struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int64  `json:"expires_in"`
	Interval                int64  `json:"interval"`
}
type GrokDevicePollResult struct {
	Pending  bool
	SlowDown bool
	Secret   map[string]string
}

func (s *Service) grokOAuthRequest(ctx context.Context, endpoint string, form url.Values) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, GrokOAuthIssuer+endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("x-grok-client-version", GrokCLIVersion)
	req.Header.Set("x-grok-client-surface", "ui")
	client := s.httpClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	// Never forward a device/refresh credential to a redirect target.
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := copyClient.Do(req) // #nosec G704 -- fixed official issuer; only internal endpoint constants, redirects disabled.
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return raw, resp.StatusCode, err
}

func (s *Service) StartGrokDeviceAuthorization(ctx context.Context) (GrokDeviceAuthorization, error) {
	raw, status, err := s.grokOAuthRequest(ctx, "/oauth2/device/code", url.Values{"client_id": {GrokOAuthClientID}, "scope": {grokOAuthScopes}, "referrer": {"grok-build"}})
	if err != nil {
		return GrokDeviceAuthorization{}, err
	}
	if status != http.StatusOK {
		return GrokDeviceAuthorization{}, fmt.Errorf("grok device authorization HTTP %d", status)
	}
	var result GrokDeviceAuthorization
	if json.Unmarshal(raw, &result) != nil || result.DeviceCode == "" || result.UserCode == "" || result.ExpiresIn <= 0 {
		return result, errors.New("invalid Grok device authorization response")
	}
	for _, link := range []string{result.VerificationURI, result.VerificationURIComplete} {
		if link == "" {
			continue
		}
		u, e := url.Parse(link)
		if e != nil || u.Scheme != "https" || (u.Host != "accounts.x.ai" && u.Host != "auth.x.ai") || u.User != nil {
			return result, errors.New("unexpected Grok verification URL")
		}
	}
	if result.VerificationURI == "" {
		return result, errors.New("missing Grok verification URL")
	}
	result.Interval = max(result.Interval, 5)
	result.ExpiresIn = min(result.ExpiresIn, 1800)
	return result, nil
}

func (s *Service) PollGrokDeviceAuthorization(ctx context.Context, deviceCode string) (GrokDevicePollResult, error) {
	raw, status, err := s.grokOAuthRequest(ctx, "/oauth2/token", url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "device_code": {deviceCode}, "client_id": {GrokOAuthClientID}})
	if err != nil {
		return GrokDevicePollResult{}, err
	}
	if status != http.StatusOK {
		var fail struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &fail)
		if fail.Error == "authorization_pending" || fail.Error == "slow_down" {
			return GrokDevicePollResult{Pending: true, SlowDown: fail.Error == "slow_down"}, nil
		}
		return GrokDevicePollResult{}, fmt.Errorf("grok device token HTTP %d", status)
	}
	var token struct {
		AccessToken  string `json:"access_token"`  // #nosec G117 -- private OAuth response decoded directly into encrypted credential storage.
		RefreshToken string `json:"refresh_token"` // #nosec G117 -- private OAuth response decoded directly into encrypted credential storage.
		IDToken      string `json:"id_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if json.Unmarshal(raw, &token) != nil || token.AccessToken == "" || token.RefreshToken == "" || token.ExpiresIn <= 0 {
		return GrokDevicePollResult{}, errors.New("invalid Grok OAuth token response")
	}
	secret := map[string]string{"access_token": token.AccessToken, "refresh_token": token.RefreshToken, "expires_at": time.Now().Add(time.Duration(token.ExpiresIn) * time.Second).UTC().Format(time.RFC3339), "created_at": time.Now().UTC().Format(time.RFC3339)}
	// Claims are display/projection metadata only; authorization remains the
	// provider's token exchange. Never make Memoh access decisions from this JWT.
	parts := strings.Split(token.IDToken, ".")
	if len(parts) == 3 {
		if data, e := base64.RawURLEncoding.DecodeString(parts[1]); e == nil {
			var claims struct {
				Subject string `json:"sub"`
				Email   string `json:"email"`
			}
			if json.Unmarshal(data, &claims) == nil {
				secret["user_id"] = claims.Subject
				secret["email"] = claims.Email
			}
		}
	}
	return GrokDevicePollResult{Secret: secret}, nil
}
