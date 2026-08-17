package orgclient

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/orgcontract"
)

// C1 judge relay, agent sender side (docs/plans/c1-judge-relay-spec-2026-08-15.md
// §1/§3). The gateway never holds a judge-provider credential: it relays a
// self-contained judge prompt to the org server's POST /api/agent/judge, which
// resolves its own sealed provider credential (llmprovider + secretref) and
// performs the call. This file owns JudgeRelay — the dedicated sender,
// mirroring PostPolicyState's Ed25519 signed-request proof. Nothing here
// touches SelectUnpushedSince or the orgpush.go privacy seam.

// judgeRelayMaxBodyExcerpt bounds the error-body excerpt carried in a
// non-200, non-latch-off JudgeRelay error (§3: "bounded 300 chars").
const judgeRelayMaxBodyExcerpt = 300

// judgeRelayMaxReplyBytes bounds how much of a 200 reply body JudgeRelay will
// read — defensive against a pathological/misbehaving server; a real judge
// reply is a short verdict, nowhere near this size.
const judgeRelayMaxReplyBytes = 1 << 20 // 1 MiB

// ErrJudgeRelayUnsupported is returned by JudgeRelay when the org server
// answers 404/405 — a pre-C1 server without the endpoint. It is NON-FATAL:
// the caller latches the relay off for the daemon lifetime, reports judge
// degradation, and never retries (§3).
var ErrJudgeRelayUnsupported = errors.New("orgclient: judge-relay endpoint unsupported (pre-C1 server)")

// JudgeRelayReply is the org server's judge answer (§1): the free-text reply
// and the provider model actually used.
type JudgeRelayReply struct {
	Text  string
	Model string
}

// judgeRelayRequest is the frozen wire body (§1: plain JSON, NO gzip).
// ModelHint omits when empty — it is recorded only; the org's provider
// selection (purpose → tagged rows) decides the actual model.
type judgeRelayRequest struct {
	Purpose   string `json:"purpose"`
	Prompt    string `json:"prompt"`
	ModelHint string `json:"model_hint,omitempty"`
}

// judgeRelayResponse is the frozen 200 wire body (§1).
type judgeRelayResponse struct {
	Text  string `json:"text"`
	Model string `json:"model"`
}

// JudgeRelay ships ONE self-contained judge prompt to the org server's
// POST /api/agent/judge (§1), authenticated with the same timestamped Ed25519
// signed-request proof as PostPolicyState (LoadEnrolment → LoadBearer →
// LoadAgentKey → sign PushSigningMessage(ts, body)). Unlike PostPolicyState the
// wire body is PLAIN JSON — the relay wire contract forbids gzip. purpose must
// be "admission" or "eval"; the server resolves its own sealed provider
// credential per purpose, so the credential never exists at the edge.
//
// Compat: a 404/405 returns ErrJudgeRelayUnsupported so the caller latches the
// relay off for the daemon lifetime (§3); 401/403 returns ErrAuthFailed; any
// other non-200 returns a generic error carrying the status code and a bounded
// body excerpt. JudgeRelay makes exactly one attempt — no internal retry; the
// admission pipeline owns fail posture (bootstrap contract §3).
func (c *Client) JudgeRelay(ctx context.Context, purpose, prompt, modelHint string) (JudgeRelayReply, error) {
	enr, err := c.store.LoadEnrolment(ctx)
	if err != nil {
		return JudgeRelayReply{}, fmt.Errorf("orgclient.JudgeRelay: %w", err)
	}
	if enr == nil {
		return JudgeRelayReply{}, ErrNotEnrolled
	}
	bearer, err := c.bearers.LoadBearer()
	if errors.Is(err, ErrNoSecret) {
		return JudgeRelayReply{}, ErrNotEnrolled
	}
	if err != nil {
		return JudgeRelayReply{}, fmt.Errorf("orgclient.JudgeRelay: load bearer: %w", err)
	}
	signKey, err := c.bearers.LoadAgentKey()
	if errors.Is(err, ErrNoSecret) {
		return JudgeRelayReply{}, ErrNotEnrolled
	}
	if err != nil {
		return JudgeRelayReply{}, fmt.Errorf("orgclient.JudgeRelay: load signing key: %w", err)
	}

	raw, err := json.Marshal(judgeRelayRequest{Purpose: purpose, Prompt: prompt, ModelHint: modelHint})
	if err != nil {
		return JudgeRelayReply{}, fmt.Errorf("orgclient.JudgeRelay: marshal: %w", err)
	}

	ts := time.Now().Unix()
	sig := ed25519.Sign(signKey, orgcontract.PushSigningMessage(ts, raw))

	url := strings.TrimRight(enr.OrgServerURL, "/") + "/api/agent/judge"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return JudgeRelayReply{}, fmt.Errorf("orgclient.JudgeRelay: new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(orgcontract.HeaderTimestamp, strconv.FormatInt(ts, 10))
	req.Header.Set(orgcontract.HeaderAgentSignature, base64.RawURLEncoding.EncodeToString(sig))

	resp, err := c.httpClient.Do(req)
	c.noteRenewalFromResponse(RenewalPathOther, resp, err)
	if err != nil {
		return JudgeRelayReply{}, fmt.Errorf("orgclient.JudgeRelay: post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		body, err := io.ReadAll(io.LimitReader(resp.Body, judgeRelayMaxReplyBytes))
		if err != nil {
			return JudgeRelayReply{}, fmt.Errorf("orgclient.JudgeRelay: read reply: %w", err)
		}
		var out judgeRelayResponse
		if err := json.Unmarshal(body, &out); err != nil {
			return JudgeRelayReply{}, fmt.Errorf("orgclient.JudgeRelay: decode reply: %w", err)
		}
		return JudgeRelayReply{Text: out.Text, Model: out.Model}, nil
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return JudgeRelayReply{}, ErrJudgeRelayUnsupported
	case http.StatusUnauthorized, http.StatusForbidden:
		return JudgeRelayReply{}, fmt.Errorf("orgclient.JudgeRelay: %w", ErrAuthFailed)
	default:
		excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, judgeRelayMaxBodyExcerpt))
		return JudgeRelayReply{}, fmt.Errorf("orgclient.JudgeRelay: server returned %d: %s", resp.StatusCode, excerpt)
	}
}
