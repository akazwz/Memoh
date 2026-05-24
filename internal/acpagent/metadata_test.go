package acpagent

import "testing"

func TestMetadataAgentEnabled(t *testing.T) {
	tests := []struct {
		name     string
		metadata map[string]any
		want     bool
	}{
		{
			name: "agent config enabled",
			metadata: map[string]any{
				MetadataKeyACP: map[string]any{
					"agents": map[string]any{
						CodexAgentID: map[string]any{"enabled": true},
					},
				},
			},
			want: true,
		},
		{
			name: "agent config disabled",
			metadata: map[string]any{
				MetadataKeyACP: map[string]any{
					"agents": map[string]any{
						CodexAgentID: map[string]any{"enabled": false},
					},
				},
			},
			want: false,
		},
		{
			name: "enabled agents compatibility",
			metadata: map[string]any{
				MetadataKeyACP: map[string]any{
					"enabled_agents": []any{CodexAgentID},
				},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MetadataAgentEnabled(tt.metadata, CodexAgentID); got != tt.want {
				t.Fatalf("MetadataAgentEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}
