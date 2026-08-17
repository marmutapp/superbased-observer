package integration_test

import (
	"reflect"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/integration"
)

func TestModelLaunch(t *testing.T) {
	tests := []struct {
		name    string
		spec    integration.ModelSpec
		model   string
		wantArg []string
		wantEnv []string
		wantErr bool
	}{
		{
			name:    "valid arg, explicit flag",
			spec:    integration.ModelSpec{Kind: integration.ModelArg, Flag: "--model"},
			model:   "gpt-5.4",
			wantArg: []string{"--model", "gpt-5.4"},
		},
		{
			name:    "valid arg, default flag when empty",
			spec:    integration.ModelSpec{Kind: integration.ModelArg},
			model:   "sonnet",
			wantArg: []string{"--model", "sonnet"},
		},
		{
			name:    "valid arg with Lead (kiro-cli chat shape)",
			spec:    integration.ModelSpec{Kind: integration.ModelArg, Flag: "--model", Lead: []string{"chat"}},
			model:   "claude-sonnet-4",
			wantArg: []string{"chat", "--model", "claude-sonnet-4"},
		},
		{
			name:    "valid env",
			spec:    integration.ModelSpec{Kind: integration.ModelEnv, EnvVar: "GOOSE_MODEL"},
			model:   "claude-opus-5",
			wantEnv: []string{"GOOSE_MODEL=claude-opus-5"},
		},
		{
			name:    "env with Lead still carries Lead in args",
			spec:    integration.ModelSpec{Kind: integration.ModelEnv, EnvVar: "GOOSE_MODEL", Lead: []string{"run"}},
			model:   "claude-opus-5",
			wantArg: []string{"run"},
			wantEnv: []string{"GOOSE_MODEL=claude-opus-5"},
		},
		{
			name:    "legit special chars: provider/model with dot",
			spec:    integration.ModelSpec{Kind: integration.ModelArg},
			model:   "anthropic/claude-sonnet-4.6",
			wantArg: []string{"--model", "anthropic/claude-sonnet-4.6"},
		},
		{
			name:    "legit special chars: thinking-level suffix",
			spec:    integration.ModelSpec{Kind: integration.ModelArg},
			model:   "sonnet:high",
			wantArg: []string{"--model", "sonnet:high"},
		},
		{
			name:    "legit special chars: bracketed parameters",
			spec:    integration.ModelSpec{Kind: integration.ModelArg},
			model:   "claude-opus-4-8[context=1m,effort=high]",
			wantArg: []string{"--model", "claude-opus-4-8[context=1m,effort=high]"},
		},
		{
			name:    "ModelNone errors",
			spec:    integration.ModelSpec{Kind: integration.ModelNone},
			model:   "sonnet",
			wantErr: true,
		},
		{
			name:    "empty model errors",
			spec:    integration.ModelSpec{Kind: integration.ModelArg},
			model:   "",
			wantErr: true,
		},
		{
			name:    "whitespace-only model errors",
			spec:    integration.ModelSpec{Kind: integration.ModelArg},
			model:   "   ",
			wantErr: true,
		},
		{
			name:    "leading-dash model rejected (flag injection guard)",
			spec:    integration.ModelSpec{Kind: integration.ModelArg},
			model:   "--evil-flag",
			wantErr: true,
		},
		{
			name:    "embedded whitespace rejected",
			spec:    integration.ModelSpec{Kind: integration.ModelArg},
			model:   "sonnet --dangerous",
			wantErr: true,
		},
		{
			name:    "control character rejected",
			spec:    integration.ModelSpec{Kind: integration.ModelArg},
			model:   "sonnet\nrm -rf /",
			wantErr: true,
		},
		{
			name:    "ModelEnv with empty EnvVar errors",
			spec:    integration.ModelSpec{Kind: integration.ModelEnv},
			model:   "sonnet",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotArgs, gotEnv, err := integration.ModelLaunch(tt.spec, tt.model)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ModelLaunch(%+v, %q) = (%v, %v, nil), want error", tt.spec, tt.model, gotArgs, gotEnv)
				}
				return
			}
			if err != nil {
				t.Fatalf("ModelLaunch(%+v, %q) unexpected error: %v", tt.spec, tt.model, err)
			}
			if !reflect.DeepEqual(gotArgs, tt.wantArg) {
				t.Errorf("args = %v, want %v", gotArgs, tt.wantArg)
			}
			if !reflect.DeepEqual(gotEnv, tt.wantEnv) {
				t.Errorf("env = %v, want %v", gotEnv, tt.wantEnv)
			}
		})
	}
}
