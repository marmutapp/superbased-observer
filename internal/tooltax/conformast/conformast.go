package conformast

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

// parsePkg parses the non-test Go files in dir. Test files are excluded on
// purpose: a conformance test must read the SHIPPING classifier, never a
// fixture that happens to sit beside it.
func parsePkg(dir string) (map[string]*ast.File, error) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		return nil, fmt.Errorf("conformast.parsePkg(%s): %w", dir, err)
	}
	files := make(map[string]*ast.File)
	for _, p := range pkgs {
		for name, f := range p.Files {
			files[name] = f
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("conformast.parsePkg(%s): no non-test Go files", dir)
	}
	return files, nil
}

// SwitchCaseStrings returns every STRING-LITERAL case label appearing in
// any switch statement inside the named top-level function of the package
// rooted at dir, sorted and de-duplicated. That set is the function's
// literal domain: the exact names it recognises.
//
// Non-literal case expressions (a `switch { case strings.HasPrefix(...) }`
// prefix ladder, a constant reference) are SKIPPED — they are not a finite
// domain and the caller must document the residual. `default:` clauses
// contribute nothing by construction.
//
// It is an error for the function not to exist or to contain no string
// case labels: a silently empty domain is exactly the vacuous pin this
// package exists to prevent.
func SwitchCaseStrings(dir, funcName string) ([]string, error) {
	files, err := parsePkg(dir)
	if err != nil {
		return nil, err
	}
	var fn *ast.FuncDecl
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if ok && fd.Name != nil && fd.Name.Name == funcName && fd.Body != nil {
				fn = fd
			}
		}
	}
	if fn == nil {
		return nil, fmt.Errorf("conformast.SwitchCaseStrings(%s, %s): function not found", dir, funcName)
	}
	seen := map[string]struct{}{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		cc, ok := n.(*ast.CaseClause)
		if !ok {
			return true
		}
		for _, e := range cc.List {
			if s, ok := literalString(e); ok {
				seen[s] = struct{}{}
			}
		}
		return true
	})
	if len(seen) == 0 {
		return nil, fmt.Errorf("conformast.SwitchCaseStrings(%s, %s): no string case labels — "+
			"the domain is empty, which would make the caller's pin vacuous", dir, funcName)
	}
	return sortedSet(seen), nil
}

// MapKeys returns every STRING-LITERAL key of the named package-level map
// composite literal in the package rooted at dir, sorted. Use it for the
// adapters whose classifier is a table rather than a switch.
func MapKeys(dir, varName string) ([]string, error) {
	files, err := parsePkg(dir)
	if err != nil {
		return nil, err
	}
	var lit *ast.CompositeLit
	for _, f := range files {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if name.Name != varName || i >= len(vs.Values) {
						continue
					}
					if cl, ok := vs.Values[i].(*ast.CompositeLit); ok {
						lit = cl
					}
				}
			}
		}
	}
	if lit == nil {
		return nil, fmt.Errorf("conformast.MapKeys(%s, %s): no package-level map literal found", dir, varName)
	}
	seen := map[string]struct{}{}
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if s, ok := literalString(kv.Key); ok {
			seen[s] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil, fmt.Errorf("conformast.MapKeys(%s, %s): no string keys", dir, varName)
	}
	return sortedSet(seen), nil
}

// ActionClassifierDomain reports the string-literal case labels of every
// switch in the package rooted at dir whose CASE BODIES reference a
// models.Action* constant, plus the string keys of every package-level map
// literal whose VALUES are models.Action* constants. In other words: the
// domain of any NAME-BASED ACTION CLASSIFIER the package ships, found
// without being told where to look.
//
// It is the detector behind the registry's honest-zero tooth. A
// Vocabulary{InTaxonomy: false} row claims the adapter has no native tool
// names to canonicalise; if its package turns out to classify names into
// action types after all, the claim is false and the caller fails.
//
// The result maps a location label ("func mapToolName" / "var actionMap")
// to that site's domain, so a failure can name the offender.
func ActionClassifierDomain(dir string) (map[string][]string, error) {
	files, err := parsePkg(dir)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, f := range files {
		for _, d := range f.Decls {
			switch decl := d.(type) {
			case *ast.FuncDecl:
				if decl.Body == nil {
					continue
				}
				seen := map[string]struct{}{}
				ast.Inspect(decl.Body, func(n ast.Node) bool {
					cc, ok := n.(*ast.CaseClause)
					if !ok || !bodyMentionsActionConst(cc.Body) {
						return true
					}
					for _, e := range cc.List {
						if s, ok := literalString(e); ok {
							seen[s] = struct{}{}
						}
					}
					return true
				})
				if len(seen) > 0 {
					out["func "+decl.Name.Name] = sortedSet(seen)
				}
			case *ast.GenDecl:
				if decl.Tok != token.VAR {
					continue
				}
				for _, spec := range decl.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, name := range vs.Names {
						if i >= len(vs.Values) {
							continue
						}
						cl, ok := vs.Values[i].(*ast.CompositeLit)
						if !ok {
							continue
						}
						seen := map[string]struct{}{}
						for _, el := range cl.Elts {
							kv, ok := el.(*ast.KeyValueExpr)
							if !ok || !isActionConst(kv.Value) {
								continue
							}
							if s, ok := literalString(kv.Key); ok {
								seen[s] = struct{}{}
							}
						}
						if len(seen) > 0 {
							out["var "+name.Name] = sortedSet(seen)
						}
					}
				}
			}
		}
	}
	return out, nil
}

// bodyMentionsActionConst reports whether any statement in a case body
// references a models.Action* selector.
func bodyMentionsActionConst(body []ast.Stmt) bool {
	found := false
	for _, st := range body {
		ast.Inspect(st, func(n ast.Node) bool {
			if found {
				return false
			}
			if isActionConst(n) {
				found = true
				return false
			}
			return true
		})
	}
	return found
}

// isActionConst reports whether n is a `models.Action*` selector
// expression (or a bare `Action*` identifier, for a package that declares
// the constants itself).
func isActionConst(n ast.Node) bool {
	switch e := n.(type) {
	case *ast.SelectorExpr:
		id, ok := e.X.(*ast.Ident)
		return ok && id.Name == "models" && strings.HasPrefix(e.Sel.Name, "Action")
	case *ast.Ident:
		return strings.HasPrefix(e.Name, "Action") && len(e.Name) > len("Action")
	}
	return false
}

// literalString unquotes a string BasicLit, reporting false for anything
// else.
func literalString(e ast.Expr) (string, bool) {
	bl, ok := e.(*ast.BasicLit)
	if !ok || bl.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(bl.Value)
	if err != nil {
		return "", false
	}
	return s, true
}

func sortedSet(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
