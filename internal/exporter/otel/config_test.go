package otel

import (
	"testing"
	"time"
)

func TestConfigWithEnvOverrides_DefaultsFilled(t *testing.T) {
	// No env vars set: empty Config must acquire the package defaults.
	t.Setenv(envEndpoint, "")
	t.Setenv(envTracesEndpoint, "")
	t.Setenv(envInsecure, "")
	t.Setenv(envTracesInsecure, "")
	t.Setenv(envSemconvOptIn, "")
	t.Setenv(envServiceNameAttr, "")

	got := Config{}.withEnvOverrides()
	if got.Endpoint != defaultEndpoint {
		t.Errorf("Endpoint: got %q want %q", got.Endpoint, defaultEndpoint)
	}
	if got.PollInterval != defaultPollInterval {
		t.Errorf("PollInterval: got %v want %v", got.PollInterval, defaultPollInterval)
	}
	if got.SemconvStability != defaultSemconvStability {
		t.Errorf("SemconvStability: got %q want %q", got.SemconvStability, defaultSemconvStability)
	}
	if got.ServiceName != defaultServiceName {
		t.Errorf("ServiceName: got %q want %q", got.ServiceName, defaultServiceName)
	}
}

func TestConfigWithEnvOverrides_FileValuesPreserved(t *testing.T) {
	t.Setenv(envEndpoint, "")
	t.Setenv(envTracesEndpoint, "")
	t.Setenv(envInsecure, "")
	t.Setenv(envTracesInsecure, "")
	t.Setenv(envSemconvOptIn, "")

	in := Config{
		Endpoint:         "collector.internal:4318",
		Insecure:         true,
		PollInterval:     5 * time.Second,
		SemconvStability: "custom",
		ServiceName:      "my-svc",
	}
	got := in.withEnvOverrides()
	if got.Endpoint != "collector.internal:4318" {
		t.Errorf("Endpoint: got %q", got.Endpoint)
	}
	if !got.Insecure {
		t.Error("Insecure should stay true")
	}
	if got.PollInterval != 5*time.Second {
		t.Errorf("PollInterval: got %v", got.PollInterval)
	}
	if got.SemconvStability != "custom" {
		t.Errorf("SemconvStability: got %q", got.SemconvStability)
	}
}

func TestConfigWithEnvOverrides_EnvWins(t *testing.T) {
	t.Setenv(envTracesEndpoint, "")
	t.Setenv(envEndpoint, "env-host:4318")
	t.Setenv(envInsecure, "true")
	t.Setenv(envSemconvOptIn, "gen_ai_latest_experimental")
	t.Setenv(envServiceNameAttr, "env-svc")

	in := Config{Endpoint: "file-host:4318", Insecure: false, SemconvStability: "file", ServiceName: "file-svc"}
	got := in.withEnvOverrides()
	if got.Endpoint != "env-host:4318" {
		t.Errorf("env endpoint should win: got %q", got.Endpoint)
	}
	if !got.Insecure {
		t.Error("env OTEL_EXPORTER_OTLP_INSECURE=true should win")
	}
	if got.SemconvStability != "gen_ai_latest_experimental" {
		t.Errorf("env semconv should win: got %q", got.SemconvStability)
	}
	if got.ServiceName != "env-svc" {
		t.Errorf("env service name should win: got %q", got.ServiceName)
	}
}

func TestConfigWithEnvOverrides_TracesEndpointWinsOverGeneric(t *testing.T) {
	t.Setenv(envEndpoint, "generic:4318")
	t.Setenv(envTracesEndpoint, "traces:4318")

	got := Config{}.withEnvOverrides()
	if got.Endpoint != "traces:4318" {
		t.Errorf("traces-specific endpoint should win: got %q", got.Endpoint)
	}
}

func TestLookupBool(t *testing.T) {
	cases := []struct {
		raw     string
		wantVal bool
		wantOK  bool
	}{
		{"true", true, true},
		{"1", true, true},
		{"yes", true, true},
		{"false", false, true},
		{"0", false, true},
		{"no", false, true},
		{"TRUE", true, true},
		{"", false, false},
		{"maybe", false, false},
	}
	for _, c := range cases {
		t.Setenv("SBO_TEST_BOOL", c.raw)
		val, ok := lookupBool("SBO_TEST_BOOL")
		if val != c.wantVal || ok != c.wantOK {
			t.Errorf("lookupBool(%q) = (%v,%v), want (%v,%v)", c.raw, val, ok, c.wantVal, c.wantOK)
		}
	}
}
