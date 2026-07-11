package main

import "testing"

func TestParseEndpointFlagsSpaceSeparated(t *testing.T) {
	args := []string{"abc-uuid", "--endpoint", "https://127.0.0.1:35989", "--csrf", "xyz"}
	positional, pin := parseEndpointFlags(args)
	if len(positional) != 1 || positional[0] != "abc-uuid" {
		t.Fatalf("positional = %v, want [abc-uuid]", positional)
	}
	if pin.Endpoint != "https://127.0.0.1:35989" {
		t.Errorf("Endpoint = %q, want https://127.0.0.1:35989", pin.Endpoint)
	}
	if pin.CSRFToken != "xyz" {
		t.Errorf("CSRFToken = %q, want xyz", pin.CSRFToken)
	}
}

func TestParseEndpointFlagsEqualsForm(t *testing.T) {
	args := []string{"abc-uuid", "--endpoint=http://127.0.0.1:12345", "--csrf=tok"}
	positional, pin := parseEndpointFlags(args)
	if len(positional) != 1 || positional[0] != "abc-uuid" {
		t.Fatalf("positional = %v", positional)
	}
	if pin.Endpoint != "http://127.0.0.1:12345" {
		t.Errorf("Endpoint = %q", pin.Endpoint)
	}
	if pin.CSRFToken != "tok" {
		t.Errorf("CSRFToken = %q", pin.CSRFToken)
	}
}

func TestParseEndpointFlagsEmptyCSRFAllowed(t *testing.T) {
	// agy.exe / CLI embedded servers don't advertise --csrf_token —
	// the bridge must accept an empty --csrf value (not interpret a
	// missing CSRF as an error).
	args := []string{"abc-uuid", "--endpoint", "http://127.0.0.1:54321", "--csrf", ""}
	_, pin := parseEndpointFlags(args)
	if pin.Endpoint == "" {
		t.Error("Endpoint should be populated")
	}
	if pin.CSRFToken != "" {
		t.Errorf("CSRFToken = %q, want empty", pin.CSRFToken)
	}
}

func TestParseEndpointFlagsNoFlags(t *testing.T) {
	args := []string{"abc-uuid"}
	positional, pin := parseEndpointFlags(args)
	if len(positional) != 1 || positional[0] != "abc-uuid" {
		t.Fatalf("positional = %v", positional)
	}
	if pin.Endpoint != "" || pin.CSRFToken != "" {
		t.Errorf("pin should be empty, got %+v", pin)
	}
}

func TestParseEndpointFlagsProbePositionalOrder(t *testing.T) {
	// Probe takes <conversation_uuid> <method_name> [field_number],
	// and the flags can appear at the end. Verify the parser preserves
	// positional order.
	args := []string{"conv-uuid", "MyMethod", "3", "--endpoint", "http://127.0.0.1:9000"}
	positional, pin := parseEndpointFlags(args)
	want := []string{"conv-uuid", "MyMethod", "3"}
	if len(positional) != len(want) {
		t.Fatalf("positional = %v, want %v", positional, want)
	}
	for i := range want {
		if positional[i] != want[i] {
			t.Errorf("positional[%d] = %q, want %q", i, positional[i], want[i])
		}
	}
	if pin.Endpoint != "http://127.0.0.1:9000" {
		t.Errorf("Endpoint = %q", pin.Endpoint)
	}
}
