package yamlmin

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// scan splits a document into significant lines, dropping blanks and comments
// and recording each line's indentation.
//
// Comment stripping is quote-aware: agent.yaml contains both trailing comments
// after values and a '#' that is part of no comment at all, and the difference
// is decided by whether the '#' sits inside quotes. A '#' also only opens a
// comment at the start of the content or after whitespace, so a plain scalar
// like foo#bar keeps its hash — the same rule the YAML spec applies.
func scan(data []byte) ([]srcLine, error) {
	var out []srcLine
	raw := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")

	for i, text := range raw {
		num := i + 1

		if strings.TrimSpace(text) == "---" || strings.TrimSpace(text) == "..." {
			return nil, fmt.Errorf("line %d: multi-document streams are not supported", num)
		}

		indent := 0
		for indent < len(text) {
			switch text[indent] {
			case ' ':
				indent++
				continue
			case '\t':
				// YAML forbids tabs in indentation, and accepting them here
				// would give a file that looks aligned but nests differently
				// than it reads.
				return nil, fmt.Errorf("line %d: tab character in indentation", num)
			}
			break
		}

		content, err := stripComment(text[indent:], num)
		if err != nil {
			return nil, err
		}
		content = strings.TrimRight(content, " ")
		if content == "" {
			continue
		}
		out = append(out, srcLine{indent: indent, text: content, num: num})
	}
	return out, nil
}

// stripComment removes a trailing comment, honouring quotes.
func stripComment(s string, lineNum int) (string, error) {
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote == '"' && c == '\\':
			i++ // skip the escaped character
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '#':
			if i == 0 || s[i-1] == ' ' || s[i-1] == '\t' {
				return s[:i], nil
			}
		}
	}
	if quote != 0 {
		return "", fmt.Errorf("line %d: unterminated %s-quoted string", lineNum, quoteName(quote))
	}
	return s, nil
}

func quoteName(q byte) string {
	if q == '\'' {
		return "single"
	}
	return "double"
}

// unquote converts scalar source text to its string value.
//
// Plain (unquoted) scalars are returned verbatim. The distinction between
// quoted and plain does not matter here the way it does in a general YAML
// implementation, because the target Go type — not the quoting — decides how
// the text is interpreted. Quoting only controls escaping.
func unquote(text string, lineNum int) (string, error) {
	if len(text) >= 2 && text[0] == '\'' && text[len(text)-1] == '\'' {
		// Single quotes are literal; the only escape is '' for a quote.
		return strings.ReplaceAll(text[1:len(text)-1], "''", "'"), nil
	}
	if len(text) >= 2 && text[0] == '"' && text[len(text)-1] == '"' {
		return unescapeDouble(text[1:len(text)-1], lineNum)
	}
	if strings.HasPrefix(text, "\"") || strings.HasPrefix(text, "'") {
		return "", fmt.Errorf("line %d: unterminated quoted string", lineNum)
	}
	// A plain scalar of "null" or "~" means the empty value, matching YAML's
	// core schema. Every other plain scalar is its own text.
	if text == "null" || text == "~" || text == "Null" || text == "NULL" {
		return "", nil
	}
	return text, nil
}

func unescapeDouble(s string, lineNum int) (string, error) {
	if !strings.ContainsRune(s, '\\') {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			continue
		}
		i++
		if i >= len(s) {
			return "", fmt.Errorf("line %d: trailing backslash in double-quoted string", lineNum)
		}
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case '0':
			b.WriteByte(0)
		case '"':
			b.WriteByte('"')
		case '\\':
			b.WriteByte('\\')
		case '/':
			b.WriteByte('/')
		case 'u':
			if i+4 >= len(s) {
				return "", fmt.Errorf("line %d: truncated \\u escape", lineNum)
			}
			cp, err := strconv.ParseUint(s[i+1:i+5], 16, 32)
			if err != nil {
				return "", fmt.Errorf("line %d: invalid \\u escape", lineNum)
			}
			var buf [4]byte
			n := utf8.EncodeRune(buf[:], rune(cp))
			b.Write(buf[:n])
			i += 4
		default:
			// Refusing an unrecognised escape is deliberate: guessing would
			// silently change a config value.
			return "", fmt.Errorf("line %d: unsupported escape \\%c", lineNum, s[i])
		}
	}
	return b.String(), nil
}
