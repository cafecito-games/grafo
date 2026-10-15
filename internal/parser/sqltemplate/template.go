// Package sqltemplate performs lexical sqlc template handling without changing
// source byte offsets. It is deliberately independent of a SQL grammar: the
// normalized bytes are only a parser input, while locations and canonical
// evidence always refer to the original source.
package sqltemplate

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

type Diagnostic struct {
	Offset  int
	Message string
	Fatal   bool
}

type Macro struct {
	Name       string
	Argument   string
	Original   string
	Start, End int
	Collection bool
	Embed      bool
}

// Wildcard is a projection wildcard found in SQL code. Qualifier is empty for
// SELECT * and contains the table or alias for SELECT table.*.
type Wildcard struct {
	Qualifier string
	Start     int
	End       int
}

type Query struct {
	Name                         string
	Command                      string
	AnnotationStart, NameStart   int
	StatementStart, StatementEnd int
}

func (q Query) QualifiedName(path string) string { return path + "#" + q.Name }

type Document struct {
	Original    []byte
	SQL         []byte
	Macros      []Macro
	Wildcards   []Wildcard
	Queries     []Query
	Diagnostics []Diagnostic
}

var annotationPattern = regexp.MustCompile(`(?is)^\s*name\s*:\s*([A-Za-z_][A-Za-z0-9_]*)\s+(:[A-Za-z][A-Za-z0-9_]*)\s*$`)
var annotationPrefixPattern = regexp.MustCompile(`^name\s*:`)

var supportedCommands = map[string]bool{
	":one": true, ":many": true, ":exec": true, ":execrows": true,
	":execlastid": true, ":execresult": true, ":copyfrom": true,
	":batchexec": true, ":batchmany": true, ":batchone": true,
}

type span struct{ start, end int }
type comment struct {
	span
	contentStart, contentEnd int
}

type annotation struct {
	name, command      string
	start, end, nameAt int
	valid              bool
	diagnosticIndex    int
}

// Analyze normalizes supported sqlc macros for dialect and associates sqlc
// named-query comments with statements. SQL always has the same byte length as
// source, so every grammar byte offset is also an original-source byte offset.
func Analyze(source []byte, dialect string) Document {
	d := Document{Original: append([]byte(nil), source...), SQL: append([]byte(nil), source...)}
	postgres := strings.EqualFold(strings.TrimSpace(dialect), "postgres") || strings.EqualFold(strings.TrimSpace(dialect), "postgresql")
	code := make([]bool, len(source))
	for i := range code {
		code[i] = true
	}
	comments := make([]comment, 0)
	parameter := 0
	for i := 0; i < len(source); {
		switch {
		case hasPrefix(source, i, "--"):
			end := bytes.IndexByte(source[i+2:], '\n')
			if end < 0 {
				end = len(source)
			} else {
				end += i + 2
			}
			markNonCode(code, i, end)
			comments = append(comments, comment{span: span{i, end}, contentStart: i + 2, contentEnd: end})
			i = end
		case hasPrefix(source, i, "/*"):
			end, closed := blockCommentEnd(source, i, postgres)
			if !closed {
				d.Diagnostics = append(d.Diagnostics, Diagnostic{Offset: i, Message: "unterminated SQL block comment", Fatal: true})
			}
			markNonCode(code, i, end)
			contentEnd := end
			if contentEnd >= i+4 && hasPrefix(source, contentEnd-2, "*/") {
				contentEnd -= 2
			}
			comments = append(comments, comment{span: span{i, end}, contentStart: i + 2, contentEnd: contentEnd})
			i = end
		case postgres && (source[i] == 'E' || source[i] == 'e') && i+1 < len(source) && source[i+1] == '\'' && boundaryBefore(source, i):
			end, closed := quotedEnd(source, i+1, '\'', true)
			if !closed {
				d.Diagnostics = append(d.Diagnostics, Diagnostic{Offset: i, Message: "unterminated PostgreSQL escape string", Fatal: true})
			}
			markNonCode(code, i, end)
			i = end
		case source[i] == '\'', source[i] == '"', source[i] == '`':
			end, closed := quotedEnd(source, i, source[i], false)
			if !closed {
				d.Diagnostics = append(d.Diagnostics, Diagnostic{Offset: i, Message: "unterminated SQL quoted value", Fatal: true})
			}
			markNonCode(code, i, end)
			i = end
		case !postgres && source[i] == '[':
			end, closed := bracketEnd(source, i)
			if !closed {
				d.Diagnostics = append(d.Diagnostics, Diagnostic{Offset: i, Message: "unterminated SQLite bracket identifier", Fatal: true})
			}
			markNonCode(code, i, end)
			i = end
		case postgres && source[i] == '$':
			if end, recognized, closed := dollarQuoteEnd(source, i); recognized {
				if !closed {
					d.Diagnostics = append(d.Diagnostics, Diagnostic{Offset: i, Message: "unterminated PostgreSQL dollar-quoted string", Fatal: true})
				}
				markNonCode(code, i, end)
				i = end
				continue
			}
			i++
		case hasPrefix(source, i, "sqlc.") && boundaryBefore(source, i):
			end, macro, diagnostic := parseMacro(source, i)
			if diagnostic != "" {
				d.Diagnostics = append(d.Diagnostics, Diagnostic{Offset: i, Message: diagnostic, Fatal: true})
				if end <= i {
					i++
				} else {
					i = end
				}
				continue
			}
			replacement := ""
			if macro.Embed {
				replacement = macro.Argument + ".*"
			} else {
				parameter++
				replacement = placeholder(dialect, parameter)
			}
			replacePadded(d.SQL, macro.Start, macro.End, replacement)
			d.Macros = append(d.Macros, macro)
			i = end
		case source[i] == '@' && shorthandStart(source, i):
			end := i + 2
			for end < len(source) && identifierPart(source[end]) {
				end++
			}
			parameter++
			argument := string(source[i+1 : end])
			macro := Macro{Name: "arg", Argument: argument, Original: string(source[i:end]), Start: i, End: end}
			replacePadded(d.SQL, i, end, placeholder(dialect, parameter))
			d.Macros = append(d.Macros, macro)
			i = end
		default:
			i++
		}
	}

	annotations := parseAnnotations(source, comments, &d)
	statements := statementSpans(source, code)
	d.Wildcards = projectionWildcards(source, code)
	d.Queries = associateQueries(source, code, annotations, statements, &d)
	sort.Slice(d.Diagnostics, func(i, j int) bool {
		if d.Diagnostics[i].Offset != d.Diagnostics[j].Offset {
			return d.Diagnostics[i].Offset < d.Diagnostics[j].Offset
		}
		return d.Diagnostics[i].Message < d.Diagnostics[j].Message
	})
	return d
}

func (d Document) HasFatalDiagnostics() bool {
	for _, diagnostic := range d.Diagnostics {
		if diagnostic.Fatal {
			return true
		}
	}
	return false
}

func projectionWildcards(source []byte, code []bool) []Wildcard {
	var result []Wildcard
	for index, value := range source {
		if value != '*' || !code[index] {
			continue
		}
		previous := index - 1
		for previous >= 0 && whitespace(source[previous]) {
			previous--
		}
		if previous >= 0 && source[previous] == '.' {
			if qualifier, start, ok := wildcardQualifier(source, code, previous); ok {
				result = append(result, Wildcard{Qualifier: qualifier, Start: start, End: index + 1})
			}
			continue
		}
		if previous >= 0 && source[previous] == ',' {
			result = append(result, Wildcard{Start: index, End: index + 1})
			continue
		}
		end := previous + 1
		for previous >= 0 && identifierPart(source[previous]) && code[previous] {
			previous--
		}
		word := string(source[previous+1 : end])
		if strings.EqualFold(word, "select") || strings.EqualFold(word, "returning") || strings.EqualFold(word, "distinct") || strings.EqualFold(word, "all") {
			result = append(result, Wildcard{Start: index, End: index + 1})
		}
	}
	return result
}

func wildcardQualifier(source []byte, code []bool, dot int) (string, int, bool) {
	position := dot - 1
	var parts []string
	start := dot
	for {
		for position >= 0 && whitespace(source[position]) {
			position--
		}
		end := position + 1
		switch {
		case position >= 0 && identifierPart(source[position]) && code[position]:
			for position >= 0 && identifierPart(source[position]) && code[position] {
				position--
			}
			parts = append([]string{string(source[position+1 : end])}, parts...)
			start = position + 1
		case position >= 0 && (source[position] == '"' || source[position] == '`'):
			quote := source[position]
			position--
			for position >= 0 && source[position] != quote {
				position--
			}
			if position < 0 {
				return "", 0, false
			}
			parts = append([]string{string(source[position+1 : end-1])}, parts...)
			start = position
			position--
		case position >= 0 && source[position] == ']':
			position--
			for position >= 0 && source[position] != '[' {
				position--
			}
			if position < 0 {
				return "", 0, false
			}
			parts = append([]string{string(source[position+1 : end-1])}, parts...)
			start = position
			position--
		default:
			return "", 0, false
		}
		for position >= 0 && whitespace(source[position]) {
			position--
		}
		if position < 0 || source[position] != '.' {
			break
		}
		position--
	}
	return strings.Join(parts, "."), start, len(parts) > 0
}

func (d Document) Position(offset int) (line, column int) {
	if offset < 0 {
		offset = 0
	}
	if offset > len(d.Original) {
		offset = len(d.Original)
	}
	prefix := d.Original[:offset]
	line = bytes.Count(prefix, []byte{'\n'}) + 1
	column = offset - bytes.LastIndexByte(prefix, '\n')
	return line, column
}

// QueryAt returns the named query whose original statement contains offset.
func (d Document) QueryAt(offset int) (Query, bool) {
	for _, query := range d.Queries {
		if offset >= query.StatementStart && offset < query.StatementEnd {
			return query, true
		}
	}
	return Query{}, false
}

// QueryInRange returns the one named query whose statement begins in the
// parser-reported raw statement range. PostgreSQL includes leading comments in
// RawStmt locations, while the canonical query span begins at the SQL token.
func (d Document) QueryInRange(start, end int) (Query, bool) {
	if end <= start || end > len(d.Original) {
		end = len(d.Original)
	}
	var matched Query
	found := false
	for _, query := range d.Queries {
		if query.StatementStart < start || query.StatementStart >= end {
			continue
		}
		if found {
			return Query{}, false
		}
		matched, found = query, true
	}
	return matched, found
}

// EmbedAt reports whether offset belongs to a recognized sqlc.embed call.
// Dialect ASTs see the same-length normalized alias wildcard at this span.
func (d Document) EmbedAt(offset int) bool {
	for _, macro := range d.Macros {
		if macro.Embed && offset >= macro.Start && offset < macro.End {
			return true
		}
	}
	return false
}

func parseMacro(source []byte, start int) (int, Macro, string) {
	nameStart := start + len("sqlc.")
	i := nameStart
	for i < len(source) && identifierPart(source[i]) {
		i++
	}
	name := string(source[nameStart:i])
	if name == "" {
		return i, Macro{}, "malformed sqlc macro name"
	}
	if i >= len(source) || source[i] != '(' {
		return i, Macro{}, fmt.Sprintf("malformed sqlc.%s macro: expected '('", name)
	}
	if name != "arg" && name != "narg" && name != "slice" && name != "embed" {
		end := macroCallEnd(source, i)
		return end, Macro{}, fmt.Sprintf("unsupported sqlc macro %q", name)
	}
	end := macroCallEnd(source, i)
	if end < 0 {
		return len(source), Macro{}, fmt.Sprintf("unterminated sqlc.%s macro", name)
	}
	argumentSource := strings.TrimSpace(string(source[i+1 : end-1]))
	argument, ok := macroArgument(argumentSource)
	if !ok {
		return end, Macro{}, fmt.Sprintf("sqlc.%s requires one quoted or unquoted identifier", name)
	}
	macro := Macro{
		Name: name, Argument: argument, Original: string(source[start:end]), Start: start, End: end,
		Collection: name == "slice", Embed: name == "embed",
	}
	return end, macro, ""
}

func macroCallEnd(source []byte, open int) int {
	depth := 0
	for i := open; i < len(source); i++ {
		switch source[i] {
		case '\'', '"', '`':
			end, _ := quotedEnd(source, i, source[i], false)
			i = end - 1
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return -1
}

func macroArgument(value string) (string, bool) {
	if value == "" {
		return "", false
	}
	if len(value) >= 2 && ((value[0] == '\'' && value[len(value)-1] == '\'') ||
		(value[0] == '"' && value[len(value)-1] == '"') ||
		(value[0] == '`' && value[len(value)-1] == '`')) {
		value = value[1 : len(value)-1]
	}
	parts := strings.Split(value, ".")
	for _, part := range parts {
		if part == "" || !identifierStart(part[0]) {
			return "", false
		}
		for i := 1; i < len(part); i++ {
			if !identifierPart(part[i]) {
				return "", false
			}
		}
	}
	return value, true
}

func parseAnnotations(source []byte, comments []comment, d *Document) []annotation {
	annotations := make([]annotation, 0)
	for _, item := range comments {
		content := source[item.contentStart:item.contentEnd]
		if !bytes.Contains(bytes.ToLower(content), []byte("name")) || !bytes.Contains(content, []byte(":")) {
			continue
		}
		match := annotationPattern.FindSubmatchIndex(content)
		if match == nil {
			trimmed := bytes.TrimSpace(bytes.ToLower(content))
			if annotationPrefixPattern.Match(trimmed) {
				d.Diagnostics = append(d.Diagnostics, Diagnostic{Offset: item.start, Message: "malformed sqlc named-query annotation"})
			}
			continue
		}
		name := string(content[match[2]:match[3]])
		command := strings.ToLower(string(content[match[4]:match[5]]))
		valid := supportedCommands[command]
		if !valid {
			d.Diagnostics = append(d.Diagnostics, Diagnostic{Offset: item.contentStart + match[4], Message: fmt.Sprintf("unsupported sqlc query command %q", command)})
		}
		annotations = append(annotations, annotation{
			name: name, command: command, start: item.start, end: item.end,
			nameAt: item.contentStart + match[2], valid: valid, diagnosticIndex: len(d.Diagnostics) - 1,
		})
	}
	return annotations
}

func statementSpans(source []byte, code []bool) []span {
	var result []span
	begin := 0
	for i, value := range source {
		if value != ';' || !code[i] {
			continue
		}
		if statement, ok := trimStatement(source, code, begin, i+1); ok {
			result = append(result, statement)
		}
		begin = i + 1
	}
	if statement, ok := trimStatement(source, code, begin, len(source)); ok {
		result = append(result, statement)
	}
	return result
}

func trimStatement(source []byte, code []bool, start, end int) (span, bool) {
	for start < end && (!code[start] || whitespace(source[start]) || source[start] == ';') {
		start++
	}
	if start >= end {
		return span{}, false
	}
	for end > start && whitespace(source[end-1]) {
		end--
	}
	return span{start, end}, true
}

func associateQueries(source []byte, code []bool, annotations []annotation, statements []span, d *Document) []Query {
	byStatement := make(map[int][]annotation)
	for _, item := range annotations {
		associated := -1
		for index, statement := range statements {
			if item.start >= statement.start && item.start < statement.end {
				d.Diagnostics = append(d.Diagnostics, Diagnostic{Offset: item.start, Message: "sqlc named-query annotation appears inside a SQL statement"})
				associated = -2
				break
			}
			if item.end <= statement.start && !hasCode(source, code, item.end, statement.start) {
				previousEnd := 0
				if index > 0 {
					previousEnd = statements[index-1].end
				}
				if item.start >= previousEnd {
					associated = index
					break
				}
			}
		}
		if associated == -1 {
			d.Diagnostics = append(d.Diagnostics, Diagnostic{Offset: item.start, Message: "sqlc named-query annotation has no following statement"})
		} else if associated >= 0 {
			byStatement[associated] = append(byStatement[associated], item)
		}
	}
	queries := make([]Query, 0, len(byStatement))
	for index, items := range byStatement {
		if len(items) != 1 {
			for _, item := range items {
				d.Diagnostics = append(d.Diagnostics, Diagnostic{Offset: item.start, Message: "multiple sqlc named-query annotations target one statement"})
			}
			continue
		}
		item := items[0]
		if !item.valid {
			continue
		}
		statement := statements[index]
		invalid := false
		for _, diagnostic := range d.Diagnostics {
			if diagnostic.Offset >= statement.start && diagnostic.Offset < statement.end {
				invalid = true
				break
			}
		}
		if invalid {
			continue
		}
		queries = append(queries, Query{
			Name: item.name, Command: item.command, AnnotationStart: item.start, NameStart: item.nameAt,
			StatementStart: statement.start, StatementEnd: statement.end,
		})
	}
	sort.Slice(queries, func(i, j int) bool { return queries[i].StatementStart < queries[j].StatementStart })
	duplicates := map[string]int{}
	for _, query := range queries {
		duplicates[strings.ToLower(query.Name)]++
	}
	filtered := queries[:0]
	for _, query := range queries {
		if duplicates[strings.ToLower(query.Name)] > 1 {
			d.Diagnostics = append(d.Diagnostics, Diagnostic{Offset: query.NameStart, Message: fmt.Sprintf("duplicate sqlc query name %q", query.Name)})
			continue
		}
		filtered = append(filtered, query)
	}
	return filtered
}

func hasCode(source []byte, code []bool, start, end int) bool {
	for i := start; i < end; i++ {
		if code[i] && !whitespace(source[i]) && source[i] != ';' {
			return true
		}
	}
	return false
}

func placeholder(dialect string, ordinal int) string {
	if strings.EqualFold(strings.TrimSpace(dialect), "postgres") || strings.EqualFold(strings.TrimSpace(dialect), "postgresql") {
		return fmt.Sprintf("$%d", ordinal)
	}
	return "?"
}

func replacePadded(target []byte, start, end int, replacement string) {
	copy(target[start:end], replacement)
	for i := start + len(replacement); i < end; i++ {
		target[i] = ' '
	}
}

func boundaryBefore(source []byte, index int) bool {
	return index == 0 || !identifierPart(source[index-1])
}

func shorthandStart(source []byte, index int) bool {
	if index+1 >= len(source) || !identifierStart(source[index+1]) {
		return false
	}
	if index == 0 {
		return true
	}
	previous := source[index-1]
	if identifierPart(previous) || previous == '@' {
		return false
	}
	return !strings.ContainsRune("<>?|&#/+-%^~!*", rune(previous))
}

func identifierStart(value byte) bool {
	return value == '_' || value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= 0x80
}

func identifierPart(value byte) bool {
	return identifierStart(value) || value >= '0' && value <= '9'
}

func whitespace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\r' || value == '\n' || value == '\f'
}

func hasPrefix(source []byte, at int, value string) bool {
	return at >= 0 && at+len(value) <= len(source) && string(source[at:at+len(value)]) == value
}

func markNonCode(code []bool, start, end int) {
	if end > len(code) {
		end = len(code)
	}
	for i := start; i < end; i++ {
		code[i] = false
	}
}

func quotedEnd(source []byte, start int, quote byte, backslashEscapes bool) (int, bool) {
	for i := start + 1; i < len(source); i++ {
		if backslashEscapes && source[i] == '\\' && i+1 < len(source) {
			i++
			continue
		}
		if source[i] != quote {
			continue
		}
		if i+1 < len(source) && source[i+1] == quote {
			i++
			continue
		}
		return i + 1, true
	}
	return len(source), false
}

func bracketEnd(source []byte, start int) (int, bool) {
	if end := bytes.IndexByte(source[start+1:], ']'); end >= 0 {
		return start + 1 + end + 1, true
	}
	return len(source), false
}

func blockCommentEnd(source []byte, start int, nested bool) (int, bool) {
	depth := 1
	for i := start + 2; i+1 < len(source); i++ {
		if nested && hasPrefix(source, i, "/*") {
			depth++
			i++
		} else if hasPrefix(source, i, "*/") {
			depth--
			i++
			if depth == 0 {
				return i + 1, true
			}
		}
	}
	return len(source), false
}

func dollarQuoteEnd(source []byte, start int) (end int, recognized, closed bool) {
	i := start + 1
	for i < len(source) && (identifierPart(source[i]) || source[i] == '_') {
		i++
	}
	if i >= len(source) || source[i] != '$' {
		return 0, false, false
	}
	delimiter := source[start : i+1]
	if end := bytes.Index(source[i+1:], delimiter); end >= 0 {
		return i + 1 + end + len(delimiter), true, true
	}
	return len(source), true, false
}
