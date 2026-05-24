// Package search implements the Monbooru query language parser and SQL executor.
package search

import (
	"strings"
)

// Expr is the interface for AST nodes.
type Expr interface {
	exprNode()
}

// AndExpr is an implicit AND (space-separated terms).
type AndExpr struct{ Left, Right Expr }

// OrExpr is an explicit OR.
type OrExpr struct{ Left, Right Expr }

// NotExpr negates its child (`-` or `NOT`).
type NotExpr struct{ Expr Expr }

// TagExpr matches a literal or wildcard tag name.
type TagExpr struct {
	Tag      string // normalized lowercase
	Wildcard string // "" | "prefix" | "suffix" | "substring"
}

// FilterExpr is a `key:value` filter.
type FilterExpr struct {
	Key string
	Val string
}

func (AndExpr) exprNode()    {}
func (OrExpr) exprNode()     {}
func (NotExpr) exprNode()    {}
func (TagExpr) exprNode()    {}
func (FilterExpr) exprNode() {}

// Parse parses a query string into an AST.
func Parse(query string) (Expr, error) {
	p := &parser{tokens: tokenize(query)}
	exprs := p.parseAll()
	if len(exprs) == 0 {
		return nil, nil
	}
	result := exprs[0]
	for _, e := range exprs[1:] {
		result = AndExpr{Left: result, Right: e}
	}
	return result, nil
}

type tokenKind int

const (
	tokTag tokenKind = iota
	tokFilter
	tokNot
	tokOR
)

type token struct {
	kind tokenKind
	val  string // raw value
}

func tokenize(query string) []token {
	var tokens []token
	query = strings.TrimSpace(query)
	if query == "" {
		return nil
	}

	i := 0
	for i < len(query) {
		if query[i] == ' ' || query[i] == '\t' {
			i++
			continue
		}

		if i+4 <= len(query) && strings.EqualFold(query[i:i+4], "not ") {
			tokens = append(tokens, token{kind: tokNot, val: "NOT"})
			i += 4
			continue
		}

		if query[i] == '-' && i+1 < len(query) && query[i+1] != ' ' {
			tokens = append(tokens, token{kind: tokNot, val: "-"})
			i++
			continue
		}

		// Read a term up to whitespace, supporting quoted filter values
		// like `folder:"my set 1"` and bare quoted tag tokens like
		// `"red hair"` whose internal spaces are collapsed to
		// underscores in parseTerm.
		j := i
		if query[j] == '"' {
			j++
			for j < len(query) && query[j] != '"' {
				j++
			}
			if j < len(query) {
				j++ // skip closing "
			}
		} else {
			for j < len(query) && query[j] != ' ' && query[j] != '\t' {
				if query[j] == ':' && j+1 < len(query) && query[j+1] == '"' {
					j += 2 // skip :"
					for j < len(query) && query[j] != '"' {
						j++
					}
					if j < len(query) {
						j++ // skip closing "
					}
					break
				}
				j++
			}
		}
		term := query[i:j]
		i = j

		if strings.EqualFold(term, "or") {
			tokens = append(tokens, token{kind: tokOR, val: "OR"})
			continue
		}
		// Literal `AND` is the implicit space-AND in long-hand form; some
		// users paste in queries from booru engines that require the
		// keyword. Drop the token so `a AND b` parses identically to
		// `a b`. Otherwise it would lowercase into a tag named "and"
		// and intersect a never-matching leaf into the expression.
		if strings.EqualFold(term, "and") {
			continue
		}

		// Any `key:value` is a filter token. Known filter keys get
		// special handling in buildFilterExpr; unknown keys fall back
		// to a category-qualified tag search.
		if colonIdx := strings.IndexByte(term, ':'); colonIdx > 0 {
			tokens = append(tokens, token{kind: tokFilter, val: term})
			continue
		}

		tokens = append(tokens, token{kind: tokTag, val: term})
	}
	return tokens
}

type parser struct {
	tokens []token
	pos    int
}

func (p *parser) peek() *token {
	if p.pos >= len(p.tokens) {
		return nil
	}
	return &p.tokens[p.pos]
}

func (p *parser) next() *token {
	if p.pos >= len(p.tokens) {
		return nil
	}
	t := &p.tokens[p.pos]
	p.pos++
	return t
}

func (p *parser) parseAll() []Expr {
	var exprs []Expr
	for {
		t := p.peek()
		if t == nil {
			break
		}

		if t.kind == tokNot {
			p.next()
			next := p.peek()
			if next == nil {
				break
			}
			inner := p.parseTerm()
			if inner != nil {
				exprs = append(exprs, NotExpr{Expr: inner})
			}
			continue
		}

		// A bare leading `OR` (or a chain of them) has no left operand.
		// Drop the token and keep parsing so the right-hand expression
		// stands on its own; otherwise parseTerm returns nil at the OR
		// and parseAll falls out of the loop with an empty expression
		// slice, which the executor treats as match-all.
		if t.kind == tokOR {
			p.next()
			continue
		}

		left := p.parseTerm()
		if left == nil {
			break
		}

		// Fold any chained OR terms into a left-leaning OrExpr so
		// `a OR b OR c` produces three leaves.
		if or := p.peek(); or != nil && or.kind == tokOR {
			expr := left
			for {
				next := p.peek()
				if next == nil || next.kind != tokOR {
					break
				}
				p.next()
				right := p.parseTerm()
				if right == nil {
					break
				}
				expr = OrExpr{Left: expr, Right: right}
			}
			exprs = append(exprs, expr)
			continue
		}

		exprs = append(exprs, left)
	}
	return exprs
}

func (p *parser) parseTerm() Expr {
	t := p.peek()
	if t == nil {
		return nil
	}
	if t.kind == tokNot || t.kind == tokOR {
		return nil
	}
	p.next()

	switch t.kind {
	case tokFilter:
		colonIdx := strings.IndexByte(t.val, ':')
		key := strings.ToLower(t.val[:colonIdx])
		val := t.val[colonIdx+1:]
		if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
			val = val[1 : len(val)-1]
		}
		return FilterExpr{Key: key, Val: val}

	case tokTag:
		tag := strings.ToLower(t.val)
		// Bare quoted tokens like `"red hair"` and `"red_hair"` are the
		// documented multi-word tag-input form. Strip the wrapping
		// quotes and collapse internal whitespace to underscores so
		// they compose with `-` and `NOT` like any other tag literal.
		if len(tag) >= 2 && tag[0] == '"' && tag[len(tag)-1] == '"' {
			tag = tag[1 : len(tag)-1]
			tag = strings.Join(strings.Fields(tag), "_")
		}
		// All-asterisks tokens (`*`, `**`, `***`...) would otherwise
		// build a `LIKE '%' ESCAPE '\'` and match every tag - a
		// "select all" alias the documented syntax doesn't expose.
		// Collapse to a literal-no-match so they compose predictably
		// with the rest of the query.
		if strings.Trim(tag, "*") == "" {
			return TagExpr{Tag: "", Wildcard: ""}
		}
		if strings.HasPrefix(tag, "*") && strings.HasSuffix(tag, "*") && len(tag) > 2 {
			return TagExpr{Tag: trimWildcards(tag), Wildcard: "substring"}
		}
		if strings.HasSuffix(tag, "*") {
			return TagExpr{Tag: strings.TrimSuffix(tag, "*"), Wildcard: "prefix"}
		}
		if strings.HasPrefix(tag, "*") && len(tag) > 1 {
			return TagExpr{Tag: strings.TrimPrefix(tag, "*"), Wildcard: "suffix"}
		}
		return TagExpr{Tag: tag, Wildcard: ""}
	}
	return nil
}

func trimWildcards(s string) string {
	s = strings.TrimPrefix(s, "*")
	s = strings.TrimSuffix(s, "*")
	return s
}

