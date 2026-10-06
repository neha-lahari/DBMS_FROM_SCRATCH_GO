// lexer.go - tokenizer for the SQL dialect.
package main

import "strings"

type TokKind int

const (
	TkIdent TokKind = iota
	TkVar           // @name
	TkParam         // ?
	TkNumber
	TkString
	TkSymbol
	TkEnd
)

type Token struct {
	Kind   TokKind
	Text   string // identifiers are lower-cased; strings are unescaped
	IsReal bool
	Pos    int
}

func isAlpha(c byte) bool        { return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
func isDigit(c byte) bool        { return c >= '0' && c <= '9' }
func isAlnum(c byte) bool        { return isAlpha(c) || isDigit(c) }
func toLowerStr(s string) string { return strings.ToLower(s) }

func lex(in string) []Token {
	var out []Token
	i, n := 0, len(in)
	for i < n {
		c := in[i]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			i++
			continue
		}
		if c == '-' && i+1 < n && in[i+1] == '-' { // -- comment
			for i < n && in[i] != '\n' {
				i++
			}
			continue
		}
		if c == '/' && i+1 < n && in[i+1] == '*' { // /* block comment */
			j := i + 2
			for j+1 < n && !(in[j] == '*' && in[j+1] == '/') {
				j++
			}
			i = j + 2
			continue
		}
		pos := i
		switch {
		case isAlpha(c):
			j := i
			for j < n && isAlnum(in[j]) {
				j++
			}
			out = append(out, Token{Kind: TkIdent, Text: toLowerStr(in[i:j]), Pos: pos})
			i = j
		case c == '@':
			j := i + 1
			for j < n && isAlnum(in[j]) {
				j++
			}
			if j == i+1 {
				panic(dbErrorf("syntax error: '@' must be followed by a name"))
			}
			out = append(out, Token{Kind: TkVar, Text: "@" + toLowerStr(in[i+1:j]), Pos: pos})
			i = j
		case c == '"':
			j := i + 1
			for j < n && in[j] != '"' {
				j++
			}
			if j >= n {
				panic(dbErrorf("syntax error: unterminated quoted identifier"))
			}
			out = append(out, Token{Kind: TkIdent, Text: toLowerStr(in[i+1 : j]), Pos: pos})
			i = j + 1
		case isDigit(c) || (c == '.' && i+1 < n && isDigit(in[i+1]) && !(i > 0 && in[i-1] == '.')):
			j := i
			for j < n && isDigit(in[j]) {
				j++
			}
			isReal := false
			if j < n && in[j] == '.' && !(j+1 < n && in[j+1] == '.') {
				isReal = true
				j++
				for j < n && isDigit(in[j]) {
					j++
				}
			}
			if j < n && (in[j] == 'e' || in[j] == 'E') {
				k := j + 1
				if k < n && (in[k] == '+' || in[k] == '-') {
					k++
				}
				if k < n && isDigit(in[k]) {
					isReal = true
					for k < n && isDigit(in[k]) {
						k++
					}
					j = k
				}
			}
			out = append(out, Token{Kind: TkNumber, Text: in[i:j], IsReal: isReal, Pos: pos})
			i = j
		case c == '\'':
			var sb strings.Builder
			j := i + 1
			for {
				if j >= n {
					panic(dbErrorf("syntax error: unterminated string literal"))
				}
				if in[j] == '\'' {
					if j+1 < n && in[j+1] == '\'' {
						sb.WriteByte('\'')
						j += 2
						continue
					}
					break
				}
				sb.WriteByte(in[j])
				j++
			}
			out = append(out, Token{Kind: TkString, Text: sb.String(), Pos: pos})
			i = j + 1
		case c == '?':
			out = append(out, Token{Kind: TkParam, Text: "?", Pos: pos})
			i++
		default:
			two := ""
			if i+2 <= n {
				two = in[i : i+2]
			}
			if two == "<=" || two == ">=" || two == "<>" || two == "!=" || two == "||" || two == ":=" {
				out = append(out, Token{Kind: TkSymbol, Text: two, Pos: pos})
				i += 2
			} else if strings.ContainsRune("(),;*+-/%=<>.", rune(c)) {
				out = append(out, Token{Kind: TkSymbol, Text: string(c), Pos: pos})
				i++
			} else {
				panic(dbErrorf("syntax error: unexpected character '%c'", c))
			}
		}
	}
	out = append(out, Token{Kind: TkEnd, Pos: n})
	return out
}

var reservedWords = map[string]bool{
	"select": true, "from": true, "where": true, "group": true, "by": true, "having": true,
	"order": true, "limit": true, "offset": true, "join": true, "left": true, "right": true,
	"inner": true, "outer": true, "full": true, "cross": true, "on": true, "and": true, "or": true, "not": true,
	"as": true, "set": true, "values": true, "into": true, "is": true, "null": true, "like": true,
	"in": true, "between": true, "asc": true, "desc": true, "distinct": true, "union": true,
	"intersect": true, "except": true, "all": true, "using": true, "case": true, "when": true,
	"then": true, "else": true, "end": true, "exists": true, "cast": true, "database": true, "filter": true, "index": true, "databases": true, "use": true,
}
