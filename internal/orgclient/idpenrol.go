package orgclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/orgcontract"
)

// ACP-P6c IdP-driven managed enrolment, agent half (plan
// docs/plans/acp-p6c-idp-managed-enrolment-plan-2026-08-20.md §5). The CLI
// starts a pairing, shows the developer a short code and a URL, and polls
// while they complete an enterprise-IdP sign-in and approve in a browser on
// ANY device. Approval mints an ordinary one-time MANAGED enrolment code
// server-side, which the CLI then redeems through the UNCHANGED Enroll path —
// so this file adds a way to OBTAIN an enrolment, never a second way to
// perform one.
//
// Hand-rolled net/http on the managedbind.go template rather than the
// generated client: these two endpoints are unauthenticated, pre-enrolment,
// and deliberately absent from the OpenAPI surface the generated client is
// built from.
//
// The wire structs below mirror internal/orgserver/api/idp_enrol.go field for
// field. They live HERE, not in orgcontract, because the server's copies are
// unexported and package-local: there is no shared type to lift, and inventing
// one would imply a contract neither side actually imports.

const (
	// idpStartPath / idpPollPath are the two agent-facing routes.
	idpStartPath = "/api/agent/idp-enrol/start"
	idpPollPath  = "/api/agent/idp-enrol/poll"
	// idpMaxResponseBytes caps what a poll or start answer may be. Each is
	// one short JSON document; the cap exists so a hostile or broken endpoint
	// on the other end of a long poll loop cannot stream unbounded bytes into
	// an agent that is asking a yes/no question.
	idpMaxResponseBytes = 8 << 10
)

// Pairing statuses as they arrive on the wire. They mirror the server's
// spellings exactly; a status this build does not recognise is passed through
// so the CLI can say what it was told rather than silently treating it as
// pending.
const (
	IdPStatusPending  = "pending"
	IdPStatusSlowDown = "slow_down"
	IdPStatusApproved = "approved"
	IdPStatusDenied   = "denied"
	IdPStatusExpired  = "expired"
)

// ErrIdPEnrolUnavailable is the SINGLE named outcome for every 404-shaped
// answer from either endpoint. The server answers a plain 404 when the rail is
// switched off, and an older server answers a plain 404 because it has never
// heard of the route — the two are indistinguishable BY DESIGN, so the agent
// must not pretend to know which it hit. Poll's unknown-pairing answer is
// deliberately the same shape, so a stale device code lands here too.
//
// The honest CLI message is one sentence covering all of it, plus the rail
// that always works: ask an admin for an enrolment code.
var ErrIdPEnrolUnavailable = errors.New("this organisation server does not offer IdP enrolment (not enabled, or the server is older than the feature)")

// IdPEnrolStart is the POST /api/agent/idp-enrol/start answer.
type IdPEnrolStart struct {
	// DeviceCode is the high-entropy value this machine holds and presents on
	// every poll. It is never displayed: the human types UserCode instead.
	DeviceCode string `json:"device_code"`
	// UserCode is the short XXXX-XXXX code the developer types in the
	// browser.
	UserCode string `json:"user_code"`
	// VerificationURI is the page to open (the server composes it from its
	// own external URL, so it is the address the developer can actually
	// reach).
	VerificationURI string `json:"verification_uri"`
	// ExpiresIn is the pairing lifetime in seconds — the CLI's overall
	// deadline.
	ExpiresIn int `json:"expires_in"`
	// Interval is the seconds between polls the server is asking for.
	Interval int `json:"interval"`
}

// IdPEnrolPoll is the POST /api/agent/idp-enrol/poll answer. OneTimeToken is
// populated on EXACTLY ONE poll — the one that observes an approved pairing —
// after which the pairing is spent and later polls read as terminal.
type IdPEnrolPoll struct {
	Status       string `json:"status"`
	OneTimeToken string `json:"one_time_token,omitempty"`
	// Interval is the server re-stating its cadence, notably on a slow_down
	// answer. Zero means "keep using the interval start gave you".
	Interval int `json:"interval,omitempty"`
}

// idpPollBody is the poll request document.
type idpPollBody struct {
	DeviceCode string `json:"device_code"`
}

// StartIdPEnrol opens a device-code pairing against orgURL and returns the
// codes plus the cadence the server wants. It is UNAUTHENTICATED and
// pre-enrolment: this machine has no org identity yet, exactly as at
// POST /api/agent/enroll, and starting a pairing grants nothing — only a
// SAML-authenticated browser approval can turn one into an enrolment.
//
// A 404-shaped answer returns ErrIdPEnrolUnavailable (see that error for why
// the two causes are not distinguished).
func (c *Client) StartIdPEnrol(ctx context.Context, orgURL string) (IdPEnrolStart, error) {
	var out IdPEnrolStart
	// An empty JSON document rather than no body: the endpoint takes no
	// parameters today, and sending a well-formed document keeps the request
	// valid if it ever grows one.
	resp, err := c.postIdP(ctx, orgURL, idpStartPath, []byte(`{}`))
	if err != nil {
		return out, fmt.Errorf("orgclient.StartIdPEnrol: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusCreated, http.StatusOK:
		if err := orgcontract.DecodeCapped(resp.Body, idpMaxResponseBytes, &out); err != nil {
			return IdPEnrolStart{}, fmt.Errorf("orgclient.StartIdPEnrol: decode: %w", err)
		}
		if out.DeviceCode == "" || out.UserCode == "" {
			return IdPEnrolStart{}, errors.New("orgclient.StartIdPEnrol: server started a pairing but returned no codes")
		}
		return out, nil
	case http.StatusNotFound:
		return IdPEnrolStart{}, ErrIdPEnrolUnavailable
	default:
		return IdPEnrolStart{}, fmt.Errorf("orgclient.StartIdPEnrol: %w", statusError(resp))
	}
}

// PollIdPEnrol asks whether the pairing named by deviceCode has been decided.
// The device code IS the authenticator for the pairing and authorises nothing
// beyond reading that one pairing's answer.
//
// Every decided outcome is a normal return with a Status, not an error: denied
// and expired are first-class answers a developer needs to be told plainly.
// Only a transport failure, an unreadable body, or an unexpected status is an
// error — plus the 404 shape, which is ErrIdPEnrolUnavailable and, on this
// endpoint, ALSO covers a device code the server does not know (the server
// conflates the two so a poll can never enumerate live pairings).
func (c *Client) PollIdPEnrol(ctx context.Context, orgURL, deviceCode string) (IdPEnrolPoll, error) {
	if strings.TrimSpace(deviceCode) == "" {
		return IdPEnrolPoll{}, errors.New("orgclient.PollIdPEnrol: no device code to poll with")
	}
	body, err := json.Marshal(idpPollBody{DeviceCode: deviceCode})
	if err != nil {
		return IdPEnrolPoll{}, fmt.Errorf("orgclient.PollIdPEnrol: marshal: %w", err)
	}
	resp, err := c.postIdP(ctx, orgURL, idpPollPath, body)
	if err != nil {
		return IdPEnrolPoll{}, fmt.Errorf("orgclient.PollIdPEnrol: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		var out IdPEnrolPoll
		if err := orgcontract.DecodeCapped(resp.Body, idpMaxResponseBytes, &out); err != nil {
			return IdPEnrolPoll{}, fmt.Errorf("orgclient.PollIdPEnrol: decode: %w", err)
		}
		if out.Status == "" {
			return IdPEnrolPoll{}, errors.New("orgclient.PollIdPEnrol: server answered without a status")
		}
		if out.Status == IdPStatusApproved && len(out.OneTimeToken) == 0 {
			return IdPEnrolPoll{}, errors.New("orgclient.PollIdPEnrol: server approved the pairing but handed over no enrolment code")
		}
		return out, nil
	case http.StatusNotFound:
		return IdPEnrolPoll{}, ErrIdPEnrolUnavailable
	default:
		return IdPEnrolPoll{}, fmt.Errorf("orgclient.PollIdPEnrol: %w", statusError(resp))
	}
}

// postIdP is the one request builder both endpoints share: no credential (the
// machine has none yet), a JSON document, and the caller's context so a CLI
// interrupt cancels an in-flight poll.
func (c *Client) postIdP(ctx context.Context, orgURL, path string, body []byte) (*http.Response, error) {
	url := strings.TrimRight(strings.TrimSpace(orgURL), "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// statusError renders an unexpected status with a BOUNDED snippet of the
// body, so an operator sees the server's own explanation (a rate-limit
// message, a proxy's error page) without the agent reading an unbounded
// stream to produce it.
func statusError(resp *http.Response) error {
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512)) //nolint:errcheck // diagnostic only; a read failure just yields a shorter message
	text := strings.TrimSpace(string(snippet))
	if text == "" {
		return fmt.Errorf("server returned %d", resp.StatusCode)
	}
	return fmt.Errorf("server returned %d: %s", resp.StatusCode, text)
}
