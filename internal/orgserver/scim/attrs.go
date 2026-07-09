package scim

import (
	scimlib "github.com/elimity-com/scim"
	filter "github.com/scim2/filter-parser/v2"
)

// attrString returns attrs[key] as a string, or "" if absent/not a string.
func attrString(attrs scimlib.ResourceAttributes, key string) string {
	if v, ok := attrs[key].(string); ok {
		return v
	}
	return ""
}

// attrBoolDefault returns attrs[key] as a bool, or def if absent/not a bool.
func attrBoolDefault(attrs scimlib.ResourceAttributes, key string, def bool) bool {
	if v, ok := attrs[key].(bool); ok {
		return v
	}
	return def
}

// extractEmail pulls the user's email from the SCIM "emails" multi-valued
// attribute (preferring the primary), falling back to a top-level "email"
// string, then to userName when it looks like an address.
func extractEmail(attrs scimlib.ResourceAttributes) string {
	if raw, ok := attrs["emails"].([]interface{}); ok {
		var first string
		for _, e := range raw {
			m, ok := e.(map[string]interface{})
			if !ok {
				continue
			}
			val, _ := m["value"].(string)
			if val == "" {
				continue
			}
			if first == "" {
				first = val
			}
			if p, _ := m["primary"].(bool); p {
				return val
			}
		}
		if first != "" {
			return first
		}
	}
	if s := attrString(attrs, "email"); s != "" {
		return s
	}
	return ""
}

// extractMemberIDs reads member user ids from a SCIM "members" attribute value
// (a list of {value: <id>} objects).
func extractMemberIDs(value interface{}) []string {
	raw, ok := value.([]interface{})
	if !ok {
		// A single {value:...} object is also accepted.
		if m, ok := value.(map[string]interface{}); ok {
			if id, _ := m["value"].(string); id != "" {
				return []string{id}
			}
		}
		return nil
	}
	var ids []string
	for _, e := range raw {
		m, ok := e.(map[string]interface{})
		if !ok {
			continue
		}
		if id, _ := m["value"].(string); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// patchTargetAttr returns the top-level attribute name a PATCH op targets, or
// "" when the op has no path (a whole-resource value map).
func patchTargetAttr(op scimlib.PatchOperation) string {
	if op.Path == nil {
		return ""
	}
	return op.Path.AttributePath.AttributeName
}

// memberIDFromPathFilter extracts the user id from a value-path filter like
// `members[value eq "abc"]`, used by some IdPs (Azure AD) for member removal.
// Returns "" if the path carries no such filter.
func memberIDFromPathFilter(op scimlib.PatchOperation) string {
	if op.Path == nil || op.Path.ValueExpression == nil {
		return ""
	}
	expr, ok := op.Path.ValueExpression.(*filter.AttributeExpression)
	if !ok {
		return ""
	}
	if expr.AttributePath.AttributeName != "value" || expr.Operator != filter.EQ {
		return ""
	}
	if s, ok := expr.CompareValue.(string); ok {
		return s
	}
	return ""
}
