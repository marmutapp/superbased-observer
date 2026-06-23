package scim

import (
	"net/http"

	scimlib "github.com/elimity-com/scim"
	scimerrors "github.com/elimity-com/scim/errors"
)

// userHandler implements scimlib.ResourceHandler for the User resource,
// backed by org_members.
type userHandler struct {
	store *Store
}

// Create provisions a new user. userName is required and unique; email is
// taken from the SCIM emails attribute (or falls back to userName).
func (h userHandler) Create(r *http.Request, attrs scimlib.ResourceAttributes) (scimlib.Resource, error) {
	userName := attrString(attrs, "userName")
	if userName == "" {
		return scimlib.Resource{}, scimerrors.ScimErrorBadParams([]string{"userName"})
	}
	id, err := newID()
	if err != nil {
		return scimlib.Resource{}, err
	}
	now := h.store.now()
	u := userRow{
		UserID:      id,
		ExternalID:  attrString(attrs, "externalId"),
		UserName:    userName,
		Email:       firstNonEmpty(extractEmail(attrs), userName),
		DisplayName: attrString(attrs, "displayName"),
		Active:      attrBoolDefault(attrs, "active", true),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := h.store.insertUser(r.Context(), u); err != nil {
		if isUniqueViolation(err) {
			return scimlib.Resource{}, scimerrors.ScimErrorUniqueness
		}
		return scimlib.Resource{}, err
	}
	return h.store.userResource(u), nil
}

func (h userHandler) Get(r *http.Request, id string) (scimlib.Resource, error) {
	u, ok, err := h.store.getUser(r.Context(), id)
	if err != nil {
		return scimlib.Resource{}, err
	}
	if !ok {
		return scimlib.Resource{}, scimerrors.ScimErrorResourceNotFound(id)
	}
	return h.store.userResource(u), nil
}

func (h userHandler) GetAll(r *http.Request, params scimlib.ListRequestParams) (scimlib.Page, error) {
	users, err := h.store.listUsers(r.Context())
	if err != nil {
		return scimlib.Page{}, err
	}
	resources := make([]scimlib.Resource, 0, len(users))
	for _, u := range users {
		res := h.store.userResource(u)
		if params.FilterValidator != nil {
			if err := params.FilterValidator.PassesFilter(res.Attributes); err != nil {
				continue
			}
		}
		resources = append(resources, res)
	}
	return paginate(resources, params), nil
}

func (h userHandler) Replace(r *http.Request, id string, attrs scimlib.ResourceAttributes) (scimlib.Resource, error) {
	existing, ok, err := h.store.getUser(r.Context(), id)
	if err != nil {
		return scimlib.Resource{}, err
	}
	if !ok {
		return scimlib.Resource{}, scimerrors.ScimErrorResourceNotFound(id)
	}
	userName := attrString(attrs, "userName")
	if userName == "" {
		return scimlib.Resource{}, scimerrors.ScimErrorBadParams([]string{"userName"})
	}
	u := userRow{
		UserID:      id,
		ExternalID:  attrString(attrs, "externalId"),
		UserName:    userName,
		Email:       firstNonEmpty(extractEmail(attrs), userName),
		DisplayName: attrString(attrs, "displayName"),
		Active:      attrBoolDefault(attrs, "active", true),
		CreatedAt:   existing.CreatedAt,
		UpdatedAt:   h.store.now(),
	}
	if _, err := h.store.replaceUser(r.Context(), u); err != nil {
		if isUniqueViolation(err) {
			return scimlib.Resource{}, scimerrors.ScimErrorUniqueness
		}
		return scimlib.Resource{}, err
	}
	return h.store.userResource(u), nil
}

// Patch applies add/replace operations to a user. The operations IdPs send in
// practice are deprovision (replace active=false) and reactivate (active=true),
// plus the occasional displayName/userName update; both the path form
// (`{op,path:"active",value:false}`) and the no-path form
// (`{op,value:{active:false}}`) are accepted.
func (h userHandler) Patch(r *http.Request, id string, ops []scimlib.PatchOperation) (scimlib.Resource, error) {
	u, ok, err := h.store.getUser(r.Context(), id)
	if err != nil {
		return scimlib.Resource{}, err
	}
	if !ok {
		return scimlib.Resource{}, scimerrors.ScimErrorResourceNotFound(id)
	}
	for _, op := range ops {
		switch patchTargetAttr(op) {
		case "active":
			if b, ok := op.Value.(bool); ok {
				u.Active = b
			}
		case "displayName":
			if s, ok := op.Value.(string); ok {
				u.DisplayName = s
			}
		case "userName":
			if s, ok := op.Value.(string); ok && s != "" {
				u.UserName = s
			}
		case "":
			// No-path op: Value is a partial attribute map.
			if m, ok := op.Value.(map[string]interface{}); ok {
				applyUserValueMap(&u, m)
			}
		default:
			// Unsupported attribute path: ignore (lenient PATCH), per the
			// many-IdP-quirks reality. Active/displayName/userName cover the
			// provisioning lifecycle.
		}
	}
	u.UpdatedAt = h.store.now()
	if _, err := h.store.replaceUser(r.Context(), u); err != nil {
		if isUniqueViolation(err) {
			return scimlib.Resource{}, scimerrors.ScimErrorUniqueness
		}
		return scimlib.Resource{}, err
	}
	return h.store.userResource(u), nil
}

func (h userHandler) Delete(r *http.Request, id string) error {
	ok, err := h.store.deleteUser(r.Context(), id)
	if err != nil {
		return err
	}
	if !ok {
		return scimerrors.ScimErrorResourceNotFound(id)
	}
	return nil
}

func applyUserValueMap(u *userRow, m map[string]interface{}) {
	if v, ok := m["active"].(bool); ok {
		u.Active = v
	}
	if v, ok := m["displayName"].(string); ok {
		u.DisplayName = v
	}
	if v, ok := m["userName"].(string); ok && v != "" {
		u.UserName = v
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
