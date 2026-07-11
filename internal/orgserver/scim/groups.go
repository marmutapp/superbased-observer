package scim

import (
	"net/http"

	scimlib "github.com/elimity-com/scim"
	scimerrors "github.com/elimity-com/scim/errors"
)

// groupHandler implements scimlib.ResourceHandler for the Group resource
// (== team), backed by org_teams and org_team_members.
type groupHandler struct {
	store *Store
}

func (h groupHandler) Create(r *http.Request, attrs scimlib.ResourceAttributes) (scimlib.Resource, error) {
	displayName := attrString(attrs, "displayName")
	if displayName == "" {
		return scimlib.Resource{}, scimerrors.ScimErrorBadParams([]string{"displayName"})
	}
	id, err := newID()
	if err != nil {
		return scimlib.Resource{}, err
	}
	now := h.store.now()
	t := teamRow{
		TeamID:      id,
		ExternalID:  attrString(attrs, "externalId"),
		DisplayName: displayName,
		Members:     extractMemberIDs(attrs["members"]),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := h.store.insertTeam(r.Context(), t); err != nil {
		if isUniqueViolation(err) {
			return scimlib.Resource{}, scimerrors.ScimErrorUniqueness
		}
		return scimlib.Resource{}, err
	}
	return h.store.teamResource(t), nil
}

func (h groupHandler) Get(r *http.Request, id string) (scimlib.Resource, error) {
	t, ok, err := h.store.getTeam(r.Context(), id)
	if err != nil {
		return scimlib.Resource{}, err
	}
	if !ok {
		return scimlib.Resource{}, scimerrors.ScimErrorResourceNotFound(id)
	}
	return h.store.teamResource(t), nil
}

func (h groupHandler) GetAll(r *http.Request, params scimlib.ListRequestParams) (scimlib.Page, error) {
	teams, err := h.store.listTeams(r.Context())
	if err != nil {
		return scimlib.Page{}, err
	}
	resources := make([]scimlib.Resource, 0, len(teams))
	for _, t := range teams {
		res := h.store.teamResource(t)
		if params.FilterValidator != nil {
			if err := params.FilterValidator.PassesFilter(res.Attributes); err != nil {
				continue
			}
		}
		resources = append(resources, res)
	}
	return paginate(resources, params), nil
}

func (h groupHandler) Replace(r *http.Request, id string, attrs scimlib.ResourceAttributes) (scimlib.Resource, error) {
	existing, ok, err := h.store.getTeam(r.Context(), id)
	if err != nil {
		return scimlib.Resource{}, err
	}
	if !ok {
		return scimlib.Resource{}, scimerrors.ScimErrorResourceNotFound(id)
	}
	displayName := attrString(attrs, "displayName")
	if displayName == "" {
		return scimlib.Resource{}, scimerrors.ScimErrorBadParams([]string{"displayName"})
	}
	t := teamRow{
		TeamID:      id,
		ExternalID:  attrString(attrs, "externalId"),
		DisplayName: displayName,
		Members:     extractMemberIDs(attrs["members"]),
		CreatedAt:   existing.CreatedAt,
		UpdatedAt:   h.store.now(),
	}
	if _, err := h.store.replaceTeam(r.Context(), t); err != nil {
		return scimlib.Resource{}, err
	}
	return h.store.teamResource(t), nil
}

// Patch applies member add/remove operations (the SCIM verbs IdPs use to keep
// group membership in sync) plus displayName replace. Member removal accepts
// both the value-list form (`{op:"remove",path:"members",value:[{value}]}`)
// and the value-path-filter form (`{op:"remove",path:'members[value eq "x"]'}`).
func (h groupHandler) Patch(r *http.Request, id string, ops []scimlib.PatchOperation) (scimlib.Resource, error) {
	t, ok, err := h.store.getTeam(r.Context(), id)
	if err != nil {
		return scimlib.Resource{}, err
	}
	if !ok {
		return scimlib.Resource{}, scimerrors.ScimErrorResourceNotFound(id)
	}
	ctx := r.Context()
	for _, op := range ops {
		switch patchTargetAttr(op) {
		case "members":
			ids := extractMemberIDs(op.Value)
			if pf := memberIDFromPathFilter(op); pf != "" {
				ids = append(ids, pf)
			}
			switch op.Op {
			case "add", "replace":
				if op.Op == "replace" && op.Path != nil {
					// `replace members` with a full set: reset then add.
					if err := h.store.removeMembers(ctx, id, t.Members); err != nil {
						return scimlib.Resource{}, err
					}
				}
				if err := h.store.addMembers(ctx, id, ids); err != nil {
					return scimlib.Resource{}, err
				}
			case "remove":
				if len(ids) == 0 {
					// `remove members` with no target → clear all.
					ids = t.Members
				}
				if err := h.store.removeMembers(ctx, id, ids); err != nil {
					return scimlib.Resource{}, err
				}
			}
		case "displayName":
			if s, ok := op.Value.(string); ok && s != "" {
				if _, err := h.store.replaceTeam(ctx, teamRow{
					TeamID: id, ExternalID: t.ExternalID, DisplayName: s,
					Members: t.Members, CreatedAt: t.CreatedAt, UpdatedAt: h.store.now(),
				}); err != nil {
					return scimlib.Resource{}, err
				}
			}
		case "":
			// No-path op carrying {members:[...]} or {displayName:...}.
			if m, ok := op.Value.(map[string]interface{}); ok {
				if mem, ok := m["members"]; ok {
					if err := h.store.addMembers(ctx, id, extractMemberIDs(mem)); err != nil {
						return scimlib.Resource{}, err
					}
				}
			}
		}
	}
	// Re-read for a consistent response.
	t, _, err = h.store.getTeam(ctx, id)
	if err != nil {
		return scimlib.Resource{}, err
	}
	return h.store.teamResource(t), nil
}

func (h groupHandler) Delete(r *http.Request, id string) error {
	ok, err := h.store.deleteTeam(r.Context(), id)
	if err != nil {
		return err
	}
	if !ok {
		return scimerrors.ScimErrorResourceNotFound(id)
	}
	return nil
}
