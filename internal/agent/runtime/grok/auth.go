package grok

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path"
	"strings"
	"time"

	"github.com/felinics/memoh/internal/agent/runtime/external"
	"github.com/felinics/memoh/internal/agentcredential"
	"github.com/felinics/memoh/internal/providers"
	"github.com/felinics/memoh/internal/workspace/bridge"
)

const authScope = providers.GrokOAuthIssuer + "::" + providers.GrokOAuthClientID

type nativeAuth struct {
	Key                       string `json:"key"`
	AuthMode                  string `json:"auth_mode"`
	Created                   string `json:"create_time"`
	UserID                    string `json:"user_id"`
	Email                     string `json:"email,omitempty"`
	RefreshToken              string `json:"refresh_token"` // #nosec G117 -- native credential file, encrypted on database writeback; never an API response.
	ExpiresAt                 string `json:"expires_at,omitempty"`
	Issuer                    string `json:"oidc_issuer"`
	ClientID                  string `json:"oidc_client_id"`
	CodingDataRetentionOptOut bool   `json:"coding_data_retention_opt_out"`
}

// Each BotAgent credential shares one native auth file. Grok's own
// auth.json.lock serializes single-use token refreshes across its processes.
// A lease has its own GROK_HOME, but GROK_AUTH_PATH points at this shared file.
// Configuration resets keep the current token pair; credential replacement
// selects a different ID. Epoch guards still fence old database writeback.
func (l *lease) projectAuth(ctx context.Context, input external.PromptInput, credential agentcredential.ResolvedCredential) (string, error) {
	rel := path.Join(".memoh/grok", input.BotAgentID, "auth", credential.ID, "auth.json")
	r, err := l.client.ReadRawNoFollow(ctx, "/data", rel)
	if err == nil {
		_ = r.Close()
		return path.Join("/data", rel), nil
	}
	if !errors.Is(err, bridge.ErrNotFound) {
		return "", err
	}
	auth := nativeAuth{Key: credential.Secret["access_token"], AuthMode: "oidc", Created: first(credential.Secret["created_at"], time.Now().UTC().Format(time.RFC3339)), UserID: credential.Secret["user_id"], Email: credential.Secret["email"], RefreshToken: credential.Secret["refresh_token"], ExpiresAt: credential.Secret["expires_at"], Issuer: providers.GrokOAuthIssuer, ClientID: providers.GrokOAuthClientID, CodingDataRetentionOptOut: true}
	raw, err := json.Marshal(map[string]nativeAuth{authScope: auth})
	if err != nil {
		return "", err
	}
	if _, err = l.client.WriteRawNoFollow(ctx, "/data", rel, bytes.NewReader(raw)); err != nil {
		return "", err
	}
	// WriteRawNoFollow creates files with owner-only mode; no secrets enter a
	// shell command, runtime metadata, UI response, or a session checkpoint.
	return path.Join("/data", rel), nil
}

func (l *lease) persistAuth(ctx context.Context, input external.PromptInput) {
	if l.authPath == "" || l.client == nil {
		return
	}
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	err := l.d.stateStore.GuardRuntimeSync(saveCtx, input.BotID, l.epoch.Bot, func(guardCtx context.Context) error {
		current, err := l.d.credentials.ResolveForBotAgent(guardCtx, input.BotID, input.BotAgentID)
		if err != nil {
			return err
		}
		if current.ID != l.credentialID || current.AuthKind != agentcredential.AuthKindGrokOAuth {
			return agentcredential.ErrRevoked
		}
		// Read the database version first. Any concurrent writeback after this
		// point makes our CAS fail instead of overwriting a newer token pair.
		r, err := l.client.ReadRawNoFollow(guardCtx, "/data", strings.TrimPrefix(l.authPath, "/data/"))
		if err != nil {
			return err
		}
		defer func() { _ = r.Close() }()
		var store map[string]nativeAuth
		if err = json.NewDecoder(io.LimitReader(r, 1<<20)).Decode(&store); err != nil {
			return err
		}
		auth, ok := store[authScope]
		if !ok || auth.Key == "" || auth.RefreshToken == "" || auth.Issuer != providers.GrokOAuthIssuer || auth.ClientID != providers.GrokOAuthClientID {
			return errors.New("invalid refreshed Grok credential")
		}
		if auth.Key == current.Secret["access_token"] && auth.RefreshToken == current.Secret["refresh_token"] {
			return nil
		}
		secret := map[string]string{"access_token": auth.Key, "refresh_token": auth.RefreshToken, "created_at": auth.Created, "expires_at": auth.ExpiresAt, "user_id": auth.UserID, "email": auth.Email}
		var expires *time.Time
		if parsed, e := time.Parse(time.RFC3339, auth.ExpiresAt); e == nil {
			expires = &parsed
		}
		_, err = l.d.credentials.UpdateSecretCAS(guardCtx, current.ID, current.CredentialVersion, secret, map[string]any{"email": auth.Email, "user_id": auth.UserID}, expires)
		return err
	})
	if err != nil {
		l.d.logger.Warn("Grok credential writeback failed", slog.Any("error", err))
	}
}
