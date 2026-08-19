// Package yamlmin parses the subset of YAML this agent's configuration file
// uses, and binds it onto a Go struct.
//
// It replaces gopkg.in/yaml.v3 (~11k lines vendored) with the part of YAML that
// agent.yaml actually contains: block mappings, block and flow sequences of
// scalars, quoted and plain scalars, and comments.
//
// The scope is deliberately narrow, and the narrowness is the safety argument
// rather than a shortcut. A general YAML implementation has to resolve an
// untyped scalar into a type on its own, which is where YAML's notorious
// surprises live — the "Norway problem" (no -> false), sexagesimals, octal-ish
// strings, six ways to write a boolean. This package never does that, because
// it always knows the Go type it is decoding into: the struct field decides how
// the text is interpreted, so `no` lands in a string field as the string "no"
// and in a bool field as an error, and neither outcome depends on a guess.
//
// What it deliberately does NOT support, and rejects rather than misreads:
// anchors and aliases (& *), tags (!!), multi-document streams (---),
// block scalars (| >), flow mappings ({}), and complex keys. None appear in
// agent.yaml, and silently misparsing them would be far worse than refusing.
package yamlmin

import (
	"fmt"
	"strings"
)

type kind uint8

const (
	kindScalar kind = iota
	kindMapping
	kindSequence
)

// node is a parsed YAML value. Only one of scalar/fields/items is meaningful,
// according to kind.
type node struct {
	kind   kind
	line   int
	scalar string
	// keys preserves document order for error messages; fields holds the values.
	keys   []string
	fields map[string]*node
	items  []*node
}

// srcLine is one significant line: blank lines and whole-line comments are
// dropped during scanning so the parser never has to think about them.
type srcLine struct {
	indent int
	text   string
	num    int
}

type parser struct {
	lines []srcLine
	pos   int
}

func (p *parser) peek() (srcLine, bool) {
	if p.pos >= len(p.lines) {
		return srcLine{}, false
	}
	return p.lines[p.pos], true
}

// parse turns a document into a node tree.
func parse(data []byte) (*node, error) {
	lines, err := scan(data)
	if err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return &node{kind: kindMapping, fields: map[string]*node{}}, nil
	}
	p := &parser{lines: lines}
	n, err := p.parseBlock(lines[0].indent)
	if err != nil {
		return nil, err
	}
	if p.pos < len(p.lines) {
		l := p.lines[p.pos]
		return nil, fmt.Errorf("line %d: unexpected indentation", l.num)
	}
	return n, nil
}

// parseBlock parses whatever block starts at the current line, at the given
// indent.
func (p *parser) parseBlock(indent int) (*node, error) {
	l, ok := p.peek()
	if !ok {
		return &node{kind: kindScalar}, nil
	}
	if strings.HasPrefix(l.text, "- ") || l.text == "-" {
		return p.parseSequence(indent)
	}
	return p.parseMapping(indent)
}

func (p *parser) parseMapping(indent int) (*node, error) {
	n := &node{kind: kindMapping, fields: map[string]*node{}}
	for {
		l, ok := p.peek()
		if !ok || l.indent < indent {
			return n, nil
		}
		if l.indent > indent {
			return nil, fmt.Errorf("line %d: unexpected indentation in mapping", l.num)
		}
		if strings.HasPrefix(l.text, "- ") {
			return nil, fmt.Errorf("line %d: sequence item where a mapping key was expected", l.num)
		}

		key, rest, err := splitKey(l.text, l.num)
		if err != nil {
			return nil, err
		}
		if n.line == 0 {
			n.line = l.num
		}
		if _, dup := n.fields[key]; dup {
			return nil, fmt.Errorf("line %d: duplicate key %q", l.num, key)
		}
		p.pos++

		var child *node
		if rest != "" {
			child, err = parseInlineValue(rest, l.num)
			if err != nil {
				return nil, err
			}
		} else {
			// A bare "key:" introduces either a nested block (indented
			// further) or an explicitly empty value.
			next, hasNext := p.peek()
			if hasNext && next.indent > l.indent {
				child, err = p.parseBlock(next.indent)
				if err != nil {
					return nil, err
				}
			} else {
				child = &node{kind: kindScalar, line: l.num}
			}
		}
		n.keys = append(n.keys, key)
		n.fields[key] = child
	}
}

func (p *parser) parseSequence(indent int) (*node, error) {
	n := &node{kind: kindSequence}
	for {
		l, ok := p.peek()
		if !ok || l.indent < indent {
			return n, nil
		}
		if l.indent > indent {
			return nil, fmt.Errorf("line %d: unexpected indentation in sequence", l.num)
		}
		if !strings.HasPrefix(l.text, "- ") && l.text != "-" {
			return n, nil
		}
		if n.line == 0 {
			n.line = l.num
		}
		rest := strings.TrimSpace(strings.TrimPrefix(l.text, "-"))
		p.pos++

		if rest == "" {
			next, hasNext := p.peek()
			if hasNext && next.indent > l.indent {
				child, err := p.parseBlock(next.indent)
				if err != nil {
					return nil, err
				}
				n.items = append(n.items, child)
				continue
			}
			n.items = append(n.items, &node{kind: kindScalar, line: l.num})
			continue
		}

		// "- key: value" starts a mapping whose first key sits on the dash
		// line. Not used by agent.yaml, but cheap to accept correctly and
		// confusing to reject.
		if _, _, err := splitKey(rest, l.num); err == nil {
			sub := &parser{lines: p.lines, pos: p.pos}
			inline := srcLine{indent: l.indent + 2, text: rest, num: l.num}
			sub.lines = append([]srcLine{inline}, p.lines[p.pos:]...)
			sub.pos = 0
			m, err := sub.parseMapping(l.indent + 2)
			if err != nil {
				return nil, err
			}
			p.pos += sub.pos - 1
			n.items = append(n.items, m)
			continue
		}

		child, err := parseInlineValue(rest, l.num)
		if err != nil {
			return nil, err
		}
		n.items = append(n.items, child)
	}
}

// splitKey separates "key: rest" respecting quotes around the key.
func splitKey(text string, lineNum int) (key, rest string, err error) {
	if strings.HasPrefix(text, "\"") || strings.HasPrefix(text, "'") {
		q := text[0]
		end := -1
		for i := 1; i < len(text); i++ {
			if text[i] == '\\' && q == '"' {
				i++
				continue
			}
			if text[i] == q {
				end = i
				break
			}
		}
		if end < 0 {
			return "", "", fmt.Errorf("line %d: unterminated quoted key", lineNum)
		}
		key, err = unquote(text[:end+1], lineNum)
		if err != nil {
			return "", "", err
		}
		after := strings.TrimSpace(text[end+1:])
		if !strings.HasPrefix(after, ":") {
			return "", "", fmt.Errorf("line %d: expected ':' after key", lineNum)
		}
		return key, strings.TrimSpace(after[1:]), nil
	}

	i := strings.IndexByte(text, ':')
	if i < 0 {
		return "", "", fmt.Errorf("line %d: expected 'key: value'", lineNum)
	}
	key = strings.TrimSpace(text[:i])
	if key == "" {
		return "", "", fmt.Errorf("line %d: empty key", lineNum)
	}
	return key, strings.TrimSpace(text[i+1:]), nil
}

// parseInlineValue handles everything that can appear to the right of "key:".
func parseInlineValue(text string, lineNum int) (*node, error) {
	if strings.HasPrefix(text, "[") {
		return parseFlowSequence(text, lineNum)
	}
	if strings.HasPrefix(text, "{") {
		return nil, fmt.Errorf("line %d: flow mappings are not supported", lineNum)
	}
	if strings.HasPrefix(text, "|") || strings.HasPrefix(text, ">") {
		return nil, fmt.Errorf("line %d: block scalars are not supported", lineNum)
	}
	if strings.HasPrefix(text, "&") || strings.HasPrefix(text, "*") {
		return nil, fmt.Errorf("line %d: anchors and aliases are not supported", lineNum)
	}
	if strings.HasPrefix(text, "!") {
		return nil, fmt.Errorf("line %d: tags are not supported", lineNum)
	}
	s, err := unquote(text, lineNum)
	if err != nil {
		return nil, err
	}
	return &node{kind: kindScalar, scalar: s, line: lineNum}, nil
}

func parseFlowSequence(text string, lineNum int) (*node, error) {
	if !strings.HasSuffix(text, "]") {
		return nil, fmt.Errorf("line %d: unterminated flow sequence", lineNum)
	}
	inner := strings.TrimSpace(text[1 : len(text)-1])
	n := &node{kind: kindSequence, line: lineNum}
	if inner == "" {
		return n, nil
	}
	for _, part := range splitFlowItems(inner) {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("line %d: empty item in flow sequence", lineNum)
		}
		s, err := unquote(part, lineNum)
		if err != nil {
			return nil, err
		}
		n.items = append(n.items, &node{kind: kindScalar, scalar: s, line: lineNum})
	}
	return n, nil
}

// splitFlowItems splits on commas that are not inside quotes.
func splitFlowItems(s string) []string {
	var out []string
	var buf strings.Builder
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == '\\' && quote == '"' && i+1 < len(s) {
				buf.WriteByte(c)
				i++
				buf.WriteByte(s[i])
				continue
			}
			if c == quote {
				quote = 0
			}
			buf.WriteByte(c)
		case c == '"' || c == '\'':
			quote = c
			buf.WriteByte(c)
		case c == ',':
			out = append(out, buf.String())
			buf.Reset()
		default:
			buf.WriteByte(c)
		}
	}
	out = append(out, buf.String())
	return out
}
