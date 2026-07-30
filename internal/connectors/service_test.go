package connectors

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	connectsdk "github.com/memohai/connect-it/sdk/go"
)

func TestNormalizeAlias(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"GitHub":            "github",
		" Google Drive ":    "google-drive",
		"中文":                "connector",
		"many---separators": "many-separators",
		"UPPER_and spaces":  "upper-and-spaces",
	}
	for input, want := range tests {
		if got := normalizeAlias(input); got != want {
			t.Errorf("normalizeAlias(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestAllocateAliasAvoidsCollisionsAndLengthOverflow(t *testing.T) {
	t.Parallel()

	used := map[string]bool{
		"github":   true,
		"github-2": true,
	}
	if got := allocateAlias("GitHub", used); got != "github-3" {
		t.Fatalf("allocateAlias collision = %q, want %q", got, "github-3")
	}

	got := allocateAlias(strings.Repeat("a", 64), map[string]bool{
		strings.Repeat("a", 32): true,
	})
	if got != strings.Repeat("a", 30)+"-2" {
		t.Fatalf("allocateAlias long value = %q", got)
	}
	if len(got) > 32 {
		t.Fatalf("allocateAlias length = %d, want at most 32", len(got))
	}
}

func TestUpstreamErrorPreservesClassification(t *testing.T) {
	t.Parallel()

	apiErr := &connectsdk.APIError{
		StatusCode: http.StatusConflict,
		Code:       "conflict",
		Message:    "private upstream detail",
	}
	err := upstreamError(apiErr)
	if !errors.Is(err, ErrUpstreamUnavailable) {
		t.Fatalf("upstream error does not match ErrUpstreamUnavailable: %v", err)
	}
	var gotAPIError *connectsdk.APIError
	if !errors.As(err, &gotAPIError) || gotAPIError != apiErr {
		t.Fatalf("upstream error did not retain APIError: %v", err)
	}
}
