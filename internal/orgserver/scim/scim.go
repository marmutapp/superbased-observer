package scim

import (
	"database/sql"
	"fmt"
	"net/http"

	scimlib "github.com/elimity-com/scim"
	"github.com/elimity-com/scim/optional"
	"github.com/elimity-com/scim/schema"
)

// NewHandler builds the SCIM 2.0 HTTP handler for the User and Group resource
// types, backed by the server DB. baseURL is the server's external URL; it is
// prepended to resource meta.location values. The returned handler serves
// SCIM under /v2/ — the server wiring mounts it at /scim/ (StripPrefix), so
// effective paths are /scim/v2/Users etc. Auth is layered on by the caller
// via auth.RequireSCIMToken.
func NewHandler(db *sql.DB, baseURL string) (http.Handler, error) {
	store := NewStore(db)
	args := &scimlib.ServerArgs{
		ServiceProviderConfig: &scimlib.ServiceProviderConfig{},
		ResourceTypes: []scimlib.ResourceType{
			{
				ID:          optional.NewString("User"),
				Name:        "User",
				Endpoint:    "/Users",
				Description: optional.NewString("SuperBased Observer org user"),
				Schema:      schema.CoreUserSchema(),
				Handler:     userHandler{store: store},
			},
			{
				ID:          optional.NewString("Group"),
				Name:        "Group",
				Endpoint:    "/Groups",
				Description: optional.NewString("SuperBased Observer team"),
				Schema:      schema.CoreGroupSchema(),
				Handler:     groupHandler{store: store},
			},
		},
	}
	var opts []scimlib.ServerOption
	if baseURL != "" {
		opts = append(opts, scimlib.WithBaseURL(baseURL+"/scim/v2"))
	}
	server, err := scimlib.NewServer(args, opts...)
	if err != nil {
		return nil, fmt.Errorf("scim.NewHandler: %w", err)
	}
	// The library routes under /v2/...; strip the /scim mount prefix.
	return http.StripPrefix("/scim", server), nil
}

// paginate applies SCIM StartIndex (1-based) and Count to a fully-materialised,
// already-filtered resource slice, returning a Page whose TotalResults is the
// match count before pagination.
//
// Count semantics: a positive Count caps the page; Count <= 0 is treated as
// "no limit" (return everything from StartIndex), which matches how IdPs that
// omit the count parameter expect to receive the full set for small orgs.
func paginate(all []scimlib.Resource, params scimlib.ListRequestParams) scimlib.Page {
	total := len(all)
	start := params.StartIndex
	if start < 1 {
		start = 1
	}
	if start > total {
		return scimlib.Page{TotalResults: total, Resources: []scimlib.Resource{}}
	}
	page := all[start-1:]
	if params.Count > 0 && params.Count < len(page) {
		page = page[:params.Count]
	}
	return scimlib.Page{TotalResults: total, Resources: page}
}
