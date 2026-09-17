package agentcredential

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/felinics/memoh/internal/db"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
	"github.com/felinics/memoh/internal/providers"
)

var (
	ErrAuthorizationExpired     = errors.New("authorization session expired or unavailable")
	ErrAuthorizationNotReady    = errors.New("authorization is not complete")
	ErrAuthorizationLimit       = errors.New("too many pending authorizations")
	ErrAuthorizationFailed      = errors.New("authorization failed")
	ErrAuthorizationCodeInvalid = errors.New("authorization code is invalid")
)

type grokAuthorizer interface {
	StartGrokDeviceAuthorization(context.Context) (providers.GrokDeviceAuthorization, error)
	PollGrokDeviceAuthorization(context.Context, string) (providers.GrokDevicePollResult, error)
}

const authorizationTTL = 30 * time.Minute

type agentAuthorizer interface {
	StartOpenAICodexACPDeviceAuthorization(context.Context) (providers.OpenAICodexACPDeviceAuthorization, error)
	PollOpenAICodexACPDeviceAuthorization(context.Context, string, string) (providers.OpenAICodexACPDevicePollResult, error)
	ExchangeOpenAICodexACPDeviceCode(context.Context, string, string) (providers.OpenAICodexOAuthCredentials, error)
	StartClaudeCodeAuthorization() (providers.ClaudeCodeAuthorization, error)
	ExchangeClaudeCodeAuthorization(context.Context, string, string, string) (string, error)
}

// AuthorizationService stages encrypted credentials independently of a Bot.
// Row locks serialize polling and one-time claims across server instances.
type AuthorizationService struct {
	credentials *Service
	oauth       agentAuthorizer
}

func NewAuthorizationService(credentials *Service, oauth *providers.Service) *AuthorizationService {
	return &AuthorizationService{credentials: credentials, oauth: oauth}
}

type AuthorizationRequest struct {
	Runtime  string            `json:"runtime" validate:"required" enums:"codex,claude-code,grok"`
	AuthKind string            `json:"auth_kind" validate:"required"`
	Secret   map[string]string `json:"secret,omitempty"`
}

// Authorization contains only the public handoff, never credentials or provider device IDs.
type Authorization struct {
	ID               string    `json:"id" validate:"required"`
	Runtime          string    `json:"runtime" validate:"required"`
	AuthKind         string    `json:"auth_kind" validate:"required"`
	Status           string    `json:"status" validate:"required" enums:"pending,ready,claimed"`
	ExpiresAt        time.Time `json:"expires_at" validate:"required"`
	UserCode         string    `json:"user_code,omitempty"`
	VerificationURL  string    `json:"verification_url,omitempty"`
	AuthorizationURL string    `json:"authorization_url,omitempty"`
	IntervalSeconds  int64     `json:"interval_seconds,omitempty"`
}

func (s *AuthorizationService) Create(ctx context.Context, owner string, req AuthorizationRequest) (Authorization, error) {
	if !s.credentials.Configured() {
		return Authorization{}, ErrEncryptionUnavailable
	}
	ownerID, err := db.ParseUUID(owner)
	if err != nil || !Compatible(req.Runtime, req.AuthKind) {
		return Authorization{}, ErrInvalidRequest
	}
	grokDevice := req.AuthKind == AuthKindGrokOAuth && len(req.Secret) == 0
	claudeBrowser := req.AuthKind == AuthKindClaudeCodeOAuth && len(req.Secret) == 0
	if req.AuthKind != AuthKindOpenAICodexOAuth && !claudeBrowser && !grokDevice && !validSecret(req.AuthKind, req.Secret) {
		return Authorization{}, ErrInvalidRequest
	}
	var result Authorization
	err = s.credentials.inTx(ctx, func(q dbstore.Queries) error {
		// Bound abandoned sessions per user, including concurrent starts.
		if err := q.LockAgentAuthorizationOwner(ctx, owner); err != nil {
			return err
		}
		if err := q.PruneAgentAuthorizations(ctx); err != nil {
			return err
		}
		count, err := q.CountAgentAuthorizations(ctx, ownerID)
		if err != nil {
			return err
		}
		if count >= 5 {
			return ErrAuthorizationLimit
		}
		payload := req.Secret
		status := "ready"
		expires := time.Now().Add(authorizationTTL)
		switch {
		case req.AuthKind == AuthKindOpenAICodexOAuth:
			device, err := s.oauth.StartOpenAICodexACPDeviceAuthorization(ctx)
			if err != nil {
				return fmt.Errorf("%w: %w", ErrAuthorizationFailed, err)
			}
			status = "pending"
			expires = time.Now().Add(15 * time.Minute)
			payload = map[string]string{
				"device_auth_id": device.DeviceAuthID, "user_code": device.UserCode,
				"verification_url": device.VerificationURL, "interval": strconv.FormatInt(device.IntervalSeconds, 10),
			}
		case grokDevice:
			authorizer, ok := s.oauth.(grokAuthorizer)
			if !ok {
				return ErrAuthorizationFailed
			}
			device, err := authorizer.StartGrokDeviceAuthorization(ctx)
			if err != nil {
				return fmt.Errorf("%w: %w", ErrAuthorizationFailed, err)
			}
			status = "pending"
			expires = time.Now().Add(time.Duration(device.ExpiresIn) * time.Second)
			verificationURL := device.VerificationURIComplete
			if verificationURL == "" {
				verificationURL = device.VerificationURI
			}
			payload = map[string]string{"device_code": device.DeviceCode, "user_code": device.UserCode, "verification_url": verificationURL, "interval": strconv.FormatInt(device.Interval, 10)}
		case claudeBrowser:
			auth, err := s.oauth.StartClaudeCodeAuthorization()
			if err != nil {
				return fmt.Errorf("%w: %w", ErrAuthorizationFailed, err)
			}
			status = "pending"
			payload = map[string]string{"authorization_url": auth.AuthorizationURL, "state": auth.State, "code_verifier": auth.CodeVerifier}
		}
		ciphertext, nonce, err := s.credentials.encrypt(payload)
		if err != nil {
			return err
		}
		row, err := q.CreateAgentAuthorization(ctx, dbsqlc.CreateAgentAuthorizationParams{
			OwnerUserID: ownerID, Runtime: req.Runtime, AuthKind: req.AuthKind, Status: status,
			EncryptedPayload: ciphertext, EncryptionNonce: nonce, ExpiresAt: timestamptz(&expires),
			PollAfter: timestamptz(timePtr(time.Now().Add(pollInterval(payload)))),
		})
		if err != nil {
			return err
		}
		result = authorizationView(row, payload)
		return nil
	})
	return result, err
}

func (s *AuthorizationService) Get(ctx context.Context, owner, id string, poll bool) (Authorization, error) {
	var result Authorization
	err := s.withSession(ctx, owner, id, func(q dbstore.Queries, row dbsqlc.AgentAuthorization) error {
		if row.Status == "claimed" {
			result = authorizationView(row, nil)
			return nil
		}
		payload, err := s.credentials.decrypt(row.EncryptedPayload, row.EncryptionNonce, 1)
		if err != nil {
			return err
		}
		if poll && row.Status == "pending" && row.AuthKind == AuthKindOpenAICodexOAuth && !time.Now().Before(row.PollAfter.Time) {
			response, err := s.oauth.PollOpenAICodexACPDeviceAuthorization(ctx, payload["device_auth_id"], payload["user_code"])
			if err != nil {
				return fmt.Errorf("%w: %w", ErrAuthorizationFailed, err)
			}
			row.PollAfter = timestamptz(timePtr(time.Now().Add(pollInterval(payload))))
			if !response.Pending {
				token, err := s.oauth.ExchangeOpenAICodexACPDeviceCode(ctx, response.AuthorizationCode, response.CodeVerifier)
				if err != nil {
					return fmt.Errorf("%w: %w", ErrAuthorizationFailed, err)
				}
				payload = map[string]string{
					"access_token": token.AccessToken, "refresh_token": token.RefreshToken,
					"id_token": token.IDToken, "account_id": token.AccountID,
				}
				if !validSecret(row.AuthKind, payload) {
					return ErrAuthorizationFailed
				}
				row.Status = "ready"
				row.ExpiresAt = timestamptz(timePtr(time.Now().Add(authorizationTTL)))
				row.EncryptedPayload, row.EncryptionNonce, err = s.credentials.encrypt(payload)
				if err != nil {
					return err
				}
			}
			row, err = q.UpdateAgentAuthorization(ctx, authorizationUpdate(row))
			if err != nil {
				return err
			}
		}
		if poll && row.Status == "pending" && row.AuthKind == AuthKindGrokOAuth && !time.Now().Before(row.PollAfter.Time) {
			authorizer, ok := s.oauth.(grokAuthorizer)
			if !ok {
				return ErrAuthorizationFailed
			}
			response, err := authorizer.PollGrokDeviceAuthorization(ctx, payload["device_code"])
			if err != nil {
				return fmt.Errorf("%w: %w", ErrAuthorizationFailed, err)
			}
			if response.SlowDown {
				payload["interval"] = strconv.FormatInt(int64(pollInterval(payload)/time.Second)+5, 10)
			}
			row.PollAfter = timestamptz(timePtr(time.Now().Add(pollInterval(payload))))
			if !response.Pending {
				payload = response.Secret
				if !validSecret(row.AuthKind, payload) {
					return ErrAuthorizationFailed
				}
				row.Status = "ready"
				row.ExpiresAt = timestamptz(timePtr(time.Now().Add(authorizationTTL)))
			}
			row.EncryptedPayload, row.EncryptionNonce, err = s.credentials.encrypt(payload)
			if err != nil {
				return err
			}
			row, err = q.UpdateAgentAuthorization(ctx, authorizationUpdate(row))
			if err != nil {
				return err
			}
		}
		result = authorizationView(row, payload)
		return nil
	})
	return result, err
}

// Exchange completes a pending Claude authorization. Failed exchanges remain
// retryable; concurrent retries cannot exchange a one-time code twice.
func (s *AuthorizationService) Exchange(ctx context.Context, owner, id, code string) (Authorization, error) {
	var result Authorization
	err := s.withSession(ctx, owner, id, func(q dbstore.Queries, row dbsqlc.AgentAuthorization) error {
		if row.AuthKind != AuthKindClaudeCodeOAuth {
			return ErrInvalidRequest
		}
		if row.Status != "pending" {
			result = authorizationView(row, nil)
			return nil
		}
		payload, err := s.credentials.decrypt(row.EncryptedPayload, row.EncryptionNonce, 1)
		if err != nil {
			return err
		}
		token, err := s.oauth.ExchangeClaudeCodeAuthorization(ctx, code, payload["state"], payload["code_verifier"])
		if errors.Is(err, providers.ErrClaudeCodeAuthorizationCodeInvalid) {
			return ErrAuthorizationCodeInvalid
		}
		if err != nil {
			return fmt.Errorf("%w: %w", ErrAuthorizationFailed, err)
		}
		secret := map[string]string{"oauth_token": token}
		if !validSecret(row.AuthKind, secret) {
			return ErrAuthorizationFailed
		}
		row.EncryptedPayload, row.EncryptionNonce, err = s.credentials.encrypt(secret)
		if err != nil {
			return err
		}
		row.Status = "ready"
		row.ExpiresAt = timestamptz(timePtr(time.Now().Add(authorizationTTL)))
		row, err = q.UpdateAgentAuthorization(ctx, authorizationUpdate(row))
		if err != nil {
			return err
		}
		result = authorizationView(row, nil)
		return nil
	})
	return result, err
}

// Claim is idempotent for the same Agent and cannot attach a session to a second
// Agent. Binding, credential encryption, and erasing staged secrets commit together.
func (s *AuthorizationService) Claim(ctx context.Context, owner, id, botID, agentID string) (PublicCredential, error) {
	var result PublicCredential
	err := s.withSession(ctx, owner, id, func(q dbstore.Queries, row dbsqlc.AgentAuthorization) error {
		creds := &Service{queries: q, aead: s.credentials.aead}
		if row.Status == "claimed" {
			if row.ClaimedAgentID.String() != agentID {
				return ErrRevoked
			}
			var err error
			result, err = creds.GetForBotAgent(ctx, botID, agentID)
			return err
		}
		if row.Status != "ready" {
			return ErrAuthorizationNotReady
		}
		secret, err := s.credentials.decrypt(row.EncryptedPayload, row.EncryptionNonce, 1)
		if err != nil {
			return err
		}
		result, err = creds.AttachToBotAgent(ctx, owner, botID, agentID, CreateRequest{
			Provider: ProviderForAuthKind(row.AuthKind), AuthKind: row.AuthKind, Secret: secret,
		})
		if err != nil {
			return err
		}
		row.ClaimedAgentID, err = db.ParseUUID(agentID)
		if err != nil {
			return ErrInvalidRequest
		}
		row.Status = "claimed"
		row.EncryptedPayload = []byte{}
		row.EncryptionNonce = []byte{}
		_, err = q.UpdateAgentAuthorization(ctx, authorizationUpdate(row))
		return err
	})
	return result, err
}

func (s *AuthorizationService) Cancel(ctx context.Context, owner, id string) error {
	sessionID, ownerID, err := parseTwoIDs(id, owner)
	if err != nil {
		return ErrInvalidRequest
	}
	return s.credentials.queries.DeleteAgentAuthorization(ctx, dbsqlc.DeleteAgentAuthorizationParams{ID: sessionID, OwnerUserID: ownerID})
}

func (s *AuthorizationService) withSession(ctx context.Context, owner, id string, fn func(dbstore.Queries, dbsqlc.AgentAuthorization) error) error {
	if !s.credentials.Configured() {
		return ErrEncryptionUnavailable
	}
	sessionID, ownerID, err := parseTwoIDs(id, owner)
	if err != nil {
		return ErrAuthorizationExpired
	}
	return s.credentials.inTx(ctx, func(q dbstore.Queries) error {
		row, err := q.GetAgentAuthorization(ctx, dbsqlc.GetAgentAuthorizationParams{ID: sessionID, OwnerUserID: ownerID})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAuthorizationExpired
		}
		if err != nil {
			return err
		}
		return fn(q, row)
	})
}

func authorizationView(row dbsqlc.AgentAuthorization, payload map[string]string) Authorization {
	result := Authorization{ID: row.ID.String(), Runtime: row.Runtime, AuthKind: row.AuthKind, Status: row.Status, ExpiresAt: row.ExpiresAt.Time}
	if row.Status == "pending" {
		if row.AuthKind == AuthKindOpenAICodexOAuth || row.AuthKind == AuthKindGrokOAuth {
			result.UserCode, result.VerificationURL = payload["user_code"], payload["verification_url"]
			result.IntervalSeconds = int64(pollInterval(payload) / time.Second)
		} else {
			result.AuthorizationURL = payload["authorization_url"]
		}
	}
	return result
}

func authorizationUpdate(row dbsqlc.AgentAuthorization) dbsqlc.UpdateAgentAuthorizationParams {
	return dbsqlc.UpdateAgentAuthorizationParams{
		ID: row.ID, Status: row.Status,
		EncryptedPayload: row.EncryptedPayload, EncryptionNonce: row.EncryptionNonce,
		ExpiresAt: row.ExpiresAt, PollAfter: row.PollAfter, ClaimedAgentID: row.ClaimedAgentID,
	}
}

func pollInterval(payload map[string]string) time.Duration {
	seconds, _ := strconv.ParseInt(payload["interval"], 10, 64)
	return time.Duration(max(5, min(seconds, 60))) * time.Second
}
func timePtr(t time.Time) *time.Time { return &t }
