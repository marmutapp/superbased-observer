package store

import (
	"github.com/marmutapp/superbased-observer/internal/identity"
	"github.com/marmutapp/superbased-observer/internal/models"
)

// These thin adapters let the four stamped row types satisfy
// identity.OrgRow without internal/identity importing internal/models.
// Each wraps a pointer to the concrete row and writes its OrgID/UserEmail
// fields. store.Ingest / Insert* call s.stamp(<adapter>{&row}) before the
// SQL bind; with a nil/unenrolled Stamper this is a no-op and the org
// columns persist as NULL.

type actionOrgRow struct{ a *models.Action }

func (r actionOrgRow) SetOrg(orgID, userEmail string) { r.a.OrgID, r.a.UserEmail = orgID, userEmail }

type sessionOrgRow struct{ s *models.Session }

func (r sessionOrgRow) SetOrg(orgID, userEmail string) { r.s.OrgID, r.s.UserEmail = orgID, userEmail }

type tokenOrgRow struct{ t *models.TokenEvent }

func (r tokenOrgRow) SetOrg(orgID, userEmail string) { r.t.OrgID, r.t.UserEmail = orgID, userEmail }

type apiTurnOrgRow struct{ t *models.APITurn }

func (r apiTurnOrgRow) SetOrg(orgID, userEmail string) { r.t.OrgID, r.t.UserEmail = orgID, userEmail }

var (
	_ identity.OrgRow = actionOrgRow{}
	_ identity.OrgRow = sessionOrgRow{}
	_ identity.OrgRow = tokenOrgRow{}
	_ identity.OrgRow = apiTurnOrgRow{}
)
