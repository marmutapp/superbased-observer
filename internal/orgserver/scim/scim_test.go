package scim

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	orgdb "github.com/marmutapp/superbased-observer/internal/orgserver/db"
)

func newSCIMServer(t *testing.T) *httptest.Server {
	t.Helper()
	d, err := orgdb.Open(context.Background(), orgdb.Options{Path: t.TempDir() + "/server.db"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	h, err := NewHandler(d, "https://org.example")
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// do issues a SCIM request and decodes the JSON body (if any).
func do(t *testing.T, srv *httptest.Server, method, path, body string) (int, map[string]interface{}) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/scim+json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]interface{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &m)
	}
	return resp.StatusCode, m
}

func createUser(t *testing.T, srv *httptest.Server, userName string) string {
	t.Helper()
	body := fmt.Sprintf(`{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"userName":%q,"displayName":"Dev","emails":[{"value":%q,"primary":true}],"active":true}`, userName, userName)
	code, m := do(t, srv, http.MethodPost, "/scim/v2/Users", body)
	if code != http.StatusCreated {
		t.Fatalf("create user %s: code=%d body=%v", userName, code, m)
	}
	id, _ := m["id"].(string)
	if id == "" {
		t.Fatalf("create user %s: no id in %v", userName, m)
	}
	return id
}

func createGroup(t *testing.T, srv *httptest.Server, name string, memberIDs ...string) string {
	t.Helper()
	members := make([]string, 0, len(memberIDs))
	for _, id := range memberIDs {
		members = append(members, fmt.Sprintf(`{"value":%q}`, id))
	}
	body := fmt.Sprintf(`{"schemas":["urn:ietf:params:scim:schemas:core:2.0:Group"],"displayName":%q,"members":[%s]}`,
		name, strings.Join(members, ","))
	code, m := do(t, srv, http.MethodPost, "/scim/v2/Groups", body)
	if code != http.StatusCreated {
		t.Fatalf("create group %s: code=%d body=%v", name, code, m)
	}
	id, _ := m["id"].(string)
	return id
}

// --- The 12 conformance operations, happy path ----------------------------

func TestSCIMConformanceHappyPath(t *testing.T) {
	srv := newSCIMServer(t)

	// 1. create
	uid := createUser(t, srv, "alice@acme.example")

	// 2. get
	code, m := do(t, srv, http.MethodGet, "/scim/v2/Users/"+uid, "")
	if code != http.StatusOK || m["userName"] != "alice@acme.example" {
		t.Fatalf("get: code=%d body=%v", code, m)
	}

	// 3. list with filter
	_ = createUser(t, srv, "bob@acme.example")
	code, m = do(t, srv, http.MethodGet, "/scim/v2/Users?filter="+url.QueryEscape(`userName eq "alice@acme.example"`), "")
	if code != http.StatusOK {
		t.Fatalf("filtered list: code=%d", code)
	}
	if tot, _ := m["totalResults"].(float64); tot != 1 {
		t.Errorf("filtered list totalResults = %v, want 1 (body=%v)", m["totalResults"], m)
	}

	// 12. paginated list (count=1)
	code, m = do(t, srv, http.MethodGet, "/scim/v2/Users?startIndex=1&count=1", "")
	if code != http.StatusOK {
		t.Fatalf("paginated list: code=%d", code)
	}
	if res, _ := m["Resources"].([]interface{}); len(res) != 1 {
		t.Errorf("paginated list returned %d resources, want 1", len(asSlice(m["Resources"])))
	}
	if tot, _ := m["totalResults"].(float64); tot != 2 {
		t.Errorf("paginated totalResults = %v, want 2", m["totalResults"])
	}

	// 4. replace
	body := `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"userName":"alice@acme.example","displayName":"Alice Renamed","active":true}`
	code, m = do(t, srv, http.MethodPut, "/scim/v2/Users/"+uid, body)
	if code != http.StatusOK || m["displayName"] != "Alice Renamed" {
		t.Fatalf("replace: code=%d body=%v", code, m)
	}

	// 5. patch (displayName)
	code, m = do(t, srv, http.MethodPatch, "/scim/v2/Users/"+uid,
		`{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[{"op":"replace","path":"displayName","value":"Alice Patched"}]}`)
	if code != http.StatusOK || m["displayName"] != "Alice Patched" {
		t.Fatalf("patch displayName: code=%d body=%v", code, m)
	}

	// 10. deprovision (patch active=false)
	code, m = do(t, srv, http.MethodPatch, "/scim/v2/Users/"+uid,
		`{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[{"op":"replace","path":"active","value":false}]}`)
	if code != http.StatusOK || m["active"] != false {
		t.Fatalf("deprovision: code=%d body=%v", code, m)
	}

	// 11. reactivate (patch active=true)
	code, m = do(t, srv, http.MethodPatch, "/scim/v2/Users/"+uid,
		`{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[{"op":"replace","path":"active","value":true}]}`)
	if code != http.StatusOK || m["active"] != true {
		t.Fatalf("reactivate: code=%d body=%v", code, m)
	}

	// 7. group create (with a member)
	gid := createGroup(t, srv, "platform", uid)
	if gid == "" {
		t.Fatal("group create returned empty id")
	}
	code, m = do(t, srv, http.MethodGet, "/scim/v2/Groups/"+gid, "")
	if code != http.StatusOK || len(asSlice(m["members"])) != 1 {
		t.Fatalf("group get: code=%d members=%v", code, m["members"])
	}

	// 9. remove member
	code, m = do(t, srv, http.MethodPatch, "/scim/v2/Groups/"+gid,
		fmt.Sprintf(`{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[{"op":"remove","path":"members","value":[{"value":%q}]}]}`, uid))
	if code != http.StatusOK {
		t.Fatalf("remove member: code=%d body=%v", code, m)
	}
	if len(asSlice(m["members"])) != 0 {
		t.Errorf("after remove, members=%v want empty", m["members"])
	}

	// 8. add member (back)
	uid2 := createUser(t, srv, "carol@acme.example")
	code, m = do(t, srv, http.MethodPatch, "/scim/v2/Groups/"+gid,
		fmt.Sprintf(`{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[{"op":"add","path":"members","value":[{"value":%q}]}]}`, uid2))
	if code != http.StatusOK || len(asSlice(m["members"])) != 1 {
		t.Fatalf("add member: code=%d members=%v", code, m["members"])
	}

	// 6. delete (user) → 204
	code, _ = do(t, srv, http.MethodDelete, "/scim/v2/Users/"+uid, "")
	if code != http.StatusNoContent {
		t.Fatalf("delete: code=%d", code)
	}
	code, _ = do(t, srv, http.MethodGet, "/scim/v2/Users/"+uid, "")
	if code != http.StatusNotFound {
		t.Errorf("get after delete: code=%d, want 404", code)
	}
}

// --- Sad paths --------------------------------------------------------------

func TestSCIMSadPaths(t *testing.T) {
	srv := newSCIMServer(t)

	t.Run("create missing userName", func(t *testing.T) {
		code, _ := do(t, srv, http.MethodPost, "/scim/v2/Users",
			`{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"displayName":"NoName"}`)
		if code != http.StatusBadRequest {
			t.Errorf("code=%d, want 400", code)
		}
	})

	t.Run("duplicate userName", func(t *testing.T) {
		_ = createUser(t, srv, "dup@acme.example")
		code, _ := do(t, srv, http.MethodPost, "/scim/v2/Users",
			`{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"userName":"dup@acme.example","active":true}`)
		if code != http.StatusConflict {
			t.Errorf("code=%d, want 409", code)
		}
	})

	t.Run("get missing user", func(t *testing.T) {
		code, _ := do(t, srv, http.MethodGet, "/scim/v2/Users/nonexistent", "")
		if code != http.StatusNotFound {
			t.Errorf("code=%d, want 404", code)
		}
	})

	t.Run("delete missing user", func(t *testing.T) {
		code, _ := do(t, srv, http.MethodDelete, "/scim/v2/Users/nonexistent", "")
		if code != http.StatusNotFound {
			t.Errorf("code=%d, want 404", code)
		}
	})

	t.Run("patch missing user", func(t *testing.T) {
		code, _ := do(t, srv, http.MethodPatch, "/scim/v2/Users/nonexistent",
			`{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[{"op":"replace","path":"active","value":false}]}`)
		if code != http.StatusNotFound {
			t.Errorf("code=%d, want 404", code)
		}
	})

	t.Run("group create missing displayName", func(t *testing.T) {
		code, _ := do(t, srv, http.MethodPost, "/scim/v2/Groups",
			`{"schemas":["urn:ietf:params:scim:schemas:core:2.0:Group"],"members":[]}`)
		if code != http.StatusBadRequest {
			t.Errorf("code=%d, want 400", code)
		}
	})
}

func asSlice(v interface{}) []interface{} {
	s, _ := v.([]interface{})
	return s
}
