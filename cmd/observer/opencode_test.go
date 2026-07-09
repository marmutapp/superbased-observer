package main

import (
	"strings"
	"testing"
)

func TestPrepareOpencodeEnv(t *testing.T) {
	t.Run("injects OPENAI_BASE_URL with /v1 when unset", func(t *testing.T) {
		env, baseURL, preset := prepareOpencodeEnv(
			[]string{"PATH=/bin", "HOME=/home/dev"},
			"http://127.0.0.1:8820",
		)
		if preset {
			t.Fatal("expected preset=false")
		}
		if baseURL != "http://127.0.0.1:8820/v1" {
			t.Errorf("baseURL = %q", baseURL)
		}
		if !containsKV(env, "OPENAI_BASE_URL=http://127.0.0.1:8820/v1") {
			t.Errorf("env missing injected OPENAI_BASE_URL: %v", env)
		}
	})

	t.Run("trailing slash on proxy URL is not doubled", func(t *testing.T) {
		_, baseURL, _ := prepareOpencodeEnv(nil, "http://127.0.0.1:8820/")
		if baseURL != "http://127.0.0.1:8820/v1" {
			t.Errorf("baseURL = %q (want no //v1)", baseURL)
		}
	})

	t.Run("user's existing OPENAI_BASE_URL wins", func(t *testing.T) {
		env, baseURL, preset := prepareOpencodeEnv(
			[]string{"OPENAI_BASE_URL=https://my.proxy/v1", "PATH=/bin"},
			"http://127.0.0.1:8820",
		)
		if !preset {
			t.Fatal("expected preset=true")
		}
		if baseURL != "https://my.proxy/v1" {
			t.Errorf("baseURL = %q (should keep user's)", baseURL)
		}
		// must not append a second OPENAI_BASE_URL
		if n := countKey(env, "OPENAI_BASE_URL"); n != 1 {
			t.Errorf("OPENAI_BASE_URL appears %d times, want 1", n)
		}
	})

	t.Run("empty existing value is treated as unset", func(t *testing.T) {
		_, _, preset := prepareOpencodeEnv([]string{"OPENAI_BASE_URL="}, "http://127.0.0.1:8820")
		if preset {
			t.Error("empty OPENAI_BASE_URL should not count as preset")
		}
	})
}

func containsKV(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}

func countKey(env []string, key string) int {
	n := 0
	for _, e := range env {
		if i := strings.IndexByte(e, '='); i > 0 && e[:i] == key {
			n++
		}
	}
	return n
}
