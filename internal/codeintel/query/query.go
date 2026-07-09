package query

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/codeintel"
)

// Run parses and executes a Cypher-SUBSET query over g, returning a
// tabular ResultSet. It supports the shapes Observer needs — one MATCH
// path (1..N hops) + optional WHERE (AND-joined property comparisons) +
// RETURN projection + LIMIT — and rejects everything else with a clear
// error (no writes, aggregation, OPTIONAL/UNION/WITH). Full grammar in
// docs/codeintel/query-cypher.md:
//
//		MATCH (a)-[:CALLS]->(b) WHERE a.name = "Run" RETURN b.name, b.file LIMIT 10
//
//	  - node:  (var) | (var:Label) | (var {name:"x", kind:"function"})
//	  - rel:   -[:TYPE]-> | <-[:TYPE]- | -[:TYPE]- (undirected) | -[]-> (any type)
//	  - WHERE: AND-joined `var.prop OP literal`, OP ∈ { = , <> , CONTAINS }
//	  - RETURN: var | var.prop, optional `AS alias`
//	  - props: name, fqn, kind, file, lang/language, start_line, end_line
func Run(g codeintel.Graph, cypher string) (codeintel.ResultSet, error) {
	q, err := parse(cypher)
	if err != nil {
		return codeintel.ResultSet{}, err
	}
	return q.exec(g)
}

// --- AST --------------------------------------------------------------

type nodePat struct {
	variable string
	kind     string            // from :Label, lowercased; "" = any
	props    map[string]string // inline {k:v} equality constraints
}

type relPat struct {
	kind string // edge kind (uppercased); "" = any
	dir  int    // +1 forward (a)->(b), -1 backward (a)<-(b), 0 undirected
}

type predicate struct {
	variable string
	prop     string
	op       string // "=", "<>", "CONTAINS"
	value    string
}

type returnItem struct {
	variable string
	prop     string // "" = whole node (rendered as fqn or name)
	alias    string
}

type ast struct {
	nodes   []nodePat
	rels    []relPat // len == len(nodes)-1
	where   []predicate
	returns []returnItem
	limit   int // 0 = no limit
}

// --- executor ---------------------------------------------------------

func (q *ast) exec(g codeintel.Graph) (codeintel.ResultSet, error) {
	byID := make(map[int64]codeintel.GraphNode, len(g.Nodes))
	for _, n := range g.Nodes {
		byID[n.ID] = n
	}
	// adjacency keyed by edge kind: out[kind][src] = []dst, in[kind][dst] = []src
	out := map[string]map[int64][]int64{}
	in := map[string]map[int64][]int64{}
	add := func(m map[string]map[int64][]int64, kind string, from, to int64) {
		if m[kind] == nil {
			m[kind] = map[int64][]int64{}
		}
		m[kind][from] = append(m[kind][from], to)
	}
	for _, e := range g.Edges {
		add(out, e.Kind, e.Src, e.Dst)
		add(in, e.Kind, e.Dst, e.Src)
	}

	// Seed bindings: every node matching nodes[0].
	type binding []int64 // node id per pattern position
	var bindings []binding
	for _, n := range g.Nodes {
		if matchNode(n, q.nodes[0]) {
			bindings = append(bindings, binding{n.ID})
		}
	}
	// Expand along each rel.
	for ri, rel := range q.rels {
		nextPat := q.nodes[ri+1]
		var next []binding
		for _, b := range bindings {
			cur := b[len(b)-1]
			for _, cand := range expand(out, in, rel, cur) {
				cn, ok := byID[cand]
				if !ok || !matchNode(cn, nextPat) {
					continue
				}
				nb := append(append(binding{}, b...), cand)
				next = append(next, nb)
			}
		}
		bindings = next
	}

	// Apply WHERE (conjunction) over full bindings.
	varPos := map[string]int{}
	for i, n := range q.nodes {
		if n.variable != "" {
			varPos[n.variable] = i
		}
	}
	rs := codeintel.ResultSet{Columns: q.columns()}
	for _, b := range bindings {
		ok := true
		for _, p := range q.where {
			pos, exists := varPos[p.variable]
			if !exists {
				return codeintel.ResultSet{}, fmt.Errorf("query: WHERE references unknown variable %q", p.variable)
			}
			if !evalPred(byID[b[pos]], p) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		row, err := q.project(byID, b, varPos)
		if err != nil {
			return codeintel.ResultSet{}, err
		}
		rs.Rows = append(rs.Rows, row)
		if q.limit > 0 && len(rs.Rows) >= q.limit {
			break
		}
	}
	return rs, nil
}

func expand(out, in map[string]map[int64][]int64, rel relPat, cur int64) []int64 {
	var dst []int64
	collect := func(m map[string]map[int64][]int64) {
		if rel.kind != "" {
			dst = append(dst, m[rel.kind][cur]...)
			return
		}
		for _, byFrom := range m {
			dst = append(dst, byFrom[cur]...)
		}
	}
	switch rel.dir {
	case +1:
		collect(out)
	case -1:
		collect(in)
	default: // undirected
		collect(out)
		collect(in)
	}
	return dst
}

func matchNode(n codeintel.GraphNode, p nodePat) bool {
	if p.kind != "" && strings.ToLower(n.Kind) != p.kind {
		return false
	}
	for k, v := range p.props {
		if nodeProp(n, k) != v {
			return false
		}
	}
	return true
}

func evalPred(n codeintel.GraphNode, p predicate) bool {
	lhs := nodeProp(n, p.prop)
	switch p.op {
	case "=":
		return lhs == p.value
	case "<>":
		return lhs != p.value
	case "CONTAINS":
		return strings.Contains(lhs, p.value)
	default:
		return false
	}
}

func nodeProp(n codeintel.GraphNode, prop string) string {
	switch strings.ToLower(prop) {
	case "name":
		return n.Name
	case "fqn":
		return n.FQN
	case "kind":
		return n.Kind
	case "file":
		return n.File
	case "lang", "language":
		return n.Language
	case "start_line", "startline":
		return strconv.Itoa(n.StartLine)
	case "end_line", "endline":
		return strconv.Itoa(n.EndLine)
	case "id":
		return strconv.FormatInt(n.ID, 10)
	default:
		return ""
	}
}

func (q *ast) columns() []string {
	cols := make([]string, len(q.returns))
	for i, r := range q.returns {
		switch {
		case r.alias != "":
			cols[i] = r.alias
		case r.prop != "":
			cols[i] = r.variable + "." + r.prop
		default:
			cols[i] = r.variable
		}
	}
	return cols
}

func (q *ast) project(byID map[int64]codeintel.GraphNode, b []int64, varPos map[string]int) ([]string, error) {
	row := make([]string, len(q.returns))
	for i, r := range q.returns {
		pos, ok := varPos[r.variable]
		if !ok {
			return nil, fmt.Errorf("query: RETURN references unknown variable %q", r.variable)
		}
		n := byID[b[pos]]
		if r.prop == "" {
			// whole node → fqn, falling back to name
			if n.FQN != "" {
				row[i] = n.FQN
			} else {
				row[i] = n.Name
			}
		} else {
			row[i] = nodeProp(n, r.prop)
		}
	}
	return row, nil
}

// --- parser -----------------------------------------------------------

var errEmpty = errors.New("query: empty query")

func parse(s string) (*ast, error) {
	toks, err := lex(s)
	if err != nil {
		return nil, err
	}
	if len(toks) == 0 {
		return nil, errEmpty
	}
	p := &parser{toks: toks}
	q := &ast{}
	if !p.expectKeyword("MATCH") {
		return nil, fmt.Errorf("query: expected MATCH, got %q", p.peekVal())
	}
	if err := p.parsePattern(q); err != nil {
		return nil, err
	}
	if p.peekKeyword("WHERE") {
		p.next()
		if err := p.parseWhere(q); err != nil {
			return nil, err
		}
	}
	if !p.expectKeyword("RETURN") {
		return nil, fmt.Errorf("query: expected RETURN, got %q", p.peekVal())
	}
	if err := p.parseReturn(q); err != nil {
		return nil, err
	}
	if p.peekKeyword("LIMIT") {
		p.next()
		t := p.next()
		if t.kind != tokNumber {
			return nil, fmt.Errorf("query: LIMIT expects a number, got %q", t.val)
		}
		n, _ := strconv.Atoi(t.val)
		q.limit = n
	}
	if !p.atEnd() {
		return nil, fmt.Errorf("query: unexpected trailing token %q (this Cypher subset supports one MATCH … WHERE? … RETURN … LIMIT?)", p.peekVal())
	}
	if len(q.returns) == 0 {
		return nil, errors.New("query: RETURN requires at least one item")
	}
	return q, nil
}

type parser struct {
	toks []token
	pos  int
}

func (p *parser) atEnd() bool { return p.pos >= len(p.toks) }
func (p *parser) peek() token {
	if p.atEnd() {
		return token{}
	}
	return p.toks[p.pos]
}
func (p *parser) peekVal() string { return p.peek().val }
func (p *parser) next() token     { t := p.peek(); p.pos++; return t }
func (p *parser) peekKeyword(k string) bool {
	t := p.peek()
	return t.kind == tokIdent && strings.EqualFold(t.val, k)
}

func (p *parser) expectKeyword(k string) bool {
	if p.peekKeyword(k) {
		p.next()
		return true
	}
	return false
}

func (p *parser) expect(kind tokKind) (token, bool) {
	if p.peek().kind == kind {
		return p.next(), true
	}
	return token{}, false
}

func (p *parser) parsePattern(q *ast) error {
	n, err := p.parseNode()
	if err != nil {
		return err
	}
	q.nodes = append(q.nodes, n)
	for {
		if p.peek().kind != tokDash && p.peek().kind != tokLArrow {
			break
		}
		rel, err := p.parseRel()
		if err != nil {
			return err
		}
		nn, err := p.parseNode()
		if err != nil {
			return err
		}
		q.rels = append(q.rels, rel)
		q.nodes = append(q.nodes, nn)
	}
	return nil
}

func (p *parser) parseNode() (nodePat, error) {
	var n nodePat
	if _, ok := p.expect(tokLParen); !ok {
		return n, fmt.Errorf("query: expected '(' to start a node, got %q", p.peekVal())
	}
	if p.peek().kind == tokIdent {
		n.variable = p.next().val
	}
	if p.peek().kind == tokColon {
		p.next()
		lbl, ok := p.expect(tokIdent)
		if !ok {
			return n, errors.New("query: expected a label after ':'")
		}
		n.kind = strings.ToLower(lbl.val)
	}
	if p.peek().kind == tokLBrace {
		props, err := p.parsePropMap()
		if err != nil {
			return n, err
		}
		n.props = props
	}
	if _, ok := p.expect(tokRParen); !ok {
		return n, fmt.Errorf("query: expected ')' to close a node, got %q", p.peekVal())
	}
	return n, nil
}

func (p *parser) parsePropMap() (map[string]string, error) {
	p.next() // consume '{'
	props := map[string]string{}
	for {
		key, ok := p.expect(tokIdent)
		if !ok {
			return nil, errors.New("query: expected a property name in { }")
		}
		if _, ok := p.expect(tokColon); !ok {
			return nil, errors.New("query: expected ':' in property map")
		}
		val, ok := p.expect(tokString)
		if !ok {
			return nil, errors.New("query: property values must be quoted strings")
		}
		props[strings.ToLower(key.val)] = val.val
		if p.peek().kind == tokComma {
			p.next()
			continue
		}
		break
	}
	if _, ok := p.expect(tokRBrace); !ok {
		return nil, errors.New("query: expected '}' to close a property map")
	}
	return props, nil
}

// parseRel parses -[:TYPE]->, <-[:TYPE]-, or -[:TYPE]- (and []-any).
func (p *parser) parseRel() (relPat, error) {
	var r relPat
	back := false
	if p.peek().kind == tokLArrow { // <-
		p.next()
		back = true
	} else if _, ok := p.expect(tokDash); !ok {
		return r, fmt.Errorf("query: expected a relationship, got %q", p.peekVal())
	}
	// optional [ :TYPE ]
	if p.peek().kind == tokLBracket {
		p.next()
		if p.peek().kind == tokColon {
			p.next()
			t, ok := p.expect(tokIdent)
			if !ok {
				return r, errors.New("query: expected a relationship type after ':'")
			}
			r.kind = strings.ToUpper(t.val)
		} else if p.peek().kind == tokIdent {
			// relationship variable (ignored) optionally followed by :TYPE
			p.next()
			if p.peek().kind == tokColon {
				p.next()
				t, ok := p.expect(tokIdent)
				if !ok {
					return r, errors.New("query: expected a relationship type after ':'")
				}
				r.kind = strings.ToUpper(t.val)
			}
		}
		if _, ok := p.expect(tokRBracket); !ok {
			return r, errors.New("query: expected ']' to close a relationship")
		}
	}
	// closing direction
	switch {
	case back:
		if _, ok := p.expect(tokDash); !ok {
			return r, errors.New("query: expected '-' to close a backward relationship")
		}
		r.dir = -1
	case p.peek().kind == tokRArrow:
		p.next()
		r.dir = +1
	case p.peek().kind == tokDash:
		p.next()
		r.dir = 0 // undirected
	default:
		return r, fmt.Errorf("query: expected '->' or '-' to close a relationship, got %q", p.peekVal())
	}
	return r, nil
}

func (p *parser) parseWhere(q *ast) error {
	for {
		v, ok := p.expect(tokIdent)
		if !ok {
			return errors.New("query: WHERE expects `var.prop OP value`")
		}
		if _, ok := p.expect(tokDot); !ok {
			return errors.New("query: WHERE expects `var.prop` (missing '.')")
		}
		prop, ok := p.expect(tokIdent)
		if !ok {
			return errors.New("query: WHERE expects a property after '.'")
		}
		var op string
		switch {
		case p.peek().kind == tokEq:
			p.next()
			op = "="
		case p.peek().kind == tokNeq:
			p.next()
			op = "<>"
		case p.peekKeyword("CONTAINS"):
			p.next()
			op = "CONTAINS"
		default:
			return fmt.Errorf("query: unsupported WHERE operator %q (use =, <>, or CONTAINS)", p.peekVal())
		}
		val, ok := p.expect(tokString)
		if !ok {
			return errors.New("query: WHERE values must be quoted strings")
		}
		q.where = append(q.where, predicate{variable: v.val, prop: strings.ToLower(prop.val), op: op, value: val.val})
		if p.peekKeyword("AND") {
			p.next()
			continue
		}
		break
	}
	return nil
}

func (p *parser) parseReturn(q *ast) error {
	for {
		v, ok := p.expect(tokIdent)
		if !ok {
			return errors.New("query: RETURN expects `var` or `var.prop`")
		}
		item := returnItem{variable: v.val}
		if p.peek().kind == tokDot {
			p.next()
			prop, ok := p.expect(tokIdent)
			if !ok {
				return errors.New("query: RETURN expects a property after '.'")
			}
			item.prop = strings.ToLower(prop.val)
		}
		if p.peekKeyword("AS") {
			p.next()
			a, ok := p.expect(tokIdent)
			if !ok {
				return errors.New("query: AS expects an alias")
			}
			item.alias = a.val
		}
		q.returns = append(q.returns, item)
		if p.peek().kind == tokComma {
			p.next()
			continue
		}
		break
	}
	return nil
}

// --- lexer ------------------------------------------------------------

type tokKind int

const (
	tokIdent tokKind = iota
	tokString
	tokNumber
	tokLParen
	tokRParen
	tokLBracket
	tokRBracket
	tokLBrace
	tokRBrace
	tokColon
	tokComma
	tokDot
	tokDash
	tokRArrow // ->
	tokLArrow // <-
	tokEq     // =
	tokNeq    // <>
)

type token struct {
	kind tokKind
	val  string
}

func lex(s string) ([]token, error) {
	var toks []token
	runes := []rune(s)
	i := 0
	for i < len(runes) {
		c := runes[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '(':
			toks = append(toks, token{tokLParen, "("})
			i++
		case c == ')':
			toks = append(toks, token{tokRParen, ")"})
			i++
		case c == '[':
			toks = append(toks, token{tokLBracket, "["})
			i++
		case c == ']':
			toks = append(toks, token{tokRBracket, "]"})
			i++
		case c == '{':
			toks = append(toks, token{tokLBrace, "{"})
			i++
		case c == '}':
			toks = append(toks, token{tokRBrace, "}"})
			i++
		case c == ':':
			toks = append(toks, token{tokColon, ":"})
			i++
		case c == ',':
			toks = append(toks, token{tokComma, ","})
			i++
		case c == '.':
			toks = append(toks, token{tokDot, "."})
			i++
		case c == '=':
			toks = append(toks, token{tokEq, "="})
			i++
		case c == '-':
			if i+1 < len(runes) && runes[i+1] == '>' {
				toks = append(toks, token{tokRArrow, "->"})
				i += 2
			} else {
				toks = append(toks, token{tokDash, "-"})
				i++
			}
		case c == '<':
			if i+1 < len(runes) && runes[i+1] == '-' {
				toks = append(toks, token{tokLArrow, "<-"})
				i += 2
			} else if i+1 < len(runes) && runes[i+1] == '>' {
				toks = append(toks, token{tokNeq, "<>"})
				i += 2
			} else {
				return nil, fmt.Errorf("query: unexpected '<' at %d", i)
			}
		case c == '"' || c == '\'':
			quote := c
			i++
			start := i
			var sb strings.Builder
			for i < len(runes) && runes[i] != quote {
				if runes[i] == '\\' && i+1 < len(runes) {
					i++
				}
				sb.WriteRune(runes[i])
				i++
			}
			if i >= len(runes) {
				return nil, fmt.Errorf("query: unterminated string starting at %d", start)
			}
			i++ // closing quote
			toks = append(toks, token{tokString, sb.String()})
		case c >= '0' && c <= '9':
			start := i
			for i < len(runes) && runes[i] >= '0' && runes[i] <= '9' {
				i++
			}
			toks = append(toks, token{tokNumber, string(runes[start:i])})
		case isIdentStart(c):
			start := i
			for i < len(runes) && isIdentPart(runes[i]) {
				i++
			}
			toks = append(toks, token{tokIdent, string(runes[start:i])})
		default:
			return nil, fmt.Errorf("query: unexpected character %q at %d", string(c), i)
		}
	}
	return toks, nil
}

func isIdentStart(r rune) bool {
	return r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func isIdentPart(r rune) bool {
	return isIdentStart(r) || (r >= '0' && r <= '9')
}
