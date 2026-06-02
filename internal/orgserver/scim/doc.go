// Package scim implements the org server's SCIM 2.0 provisioning endpoints
// (RFC 7643/7644) using github.com/elimity-com/scim with a custom storage
// adapter backed by the server's SQLite database.
//
// Two resource types are served:
//
//   - User  (/scim/v2/Users)  → org_members
//   - Group (/scim/v2/Groups) → org_teams, with membership in org_team_members
//
// The library supplies the RFC 7643 core User and Group schemas
// (schema.CoreUserSchema/CoreGroupSchema), request parsing, filtering, and
// response serialisation; this package supplies the ResourceHandler
// callbacks (Create/Get/GetAll/Replace/Patch/Delete) that translate to SQL.
//
// SCIM authentication (the static IdP token) is enforced by the
// auth.RequireSCIMToken middleware at the server-wiring layer, not here, so
// the handlers can stay focused on storage.
package scim
