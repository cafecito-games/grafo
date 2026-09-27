// Package markdown extracts documentation structure and explicit code references.
package markdown

import (
	"bytes"
	"context"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	"github.com/yuin/goldmark"
	mdast "github.com/yuin/goldmark/ast"
	mdparser "github.com/yuin/goldmark/parser"
	mdtext "github.com/yuin/goldmark/text"
)

type Parser struct {
	parser mdparser.Parser
}

func New() *Parser {
	return &Parser{parser: goldmark.DefaultParser()}
}

func (*Parser) Language() string { return "markdown" }

func (*Parser) Supports(filePath string) bool {
	switch strings.ToLower(filepath.Ext(filePath)) {
	case ".md", ".markdown":
		return true
	default:
		return false
	}
}

type heading struct {
	offset  int
	line    int
	column  int
	level   int
	title   string
	anchor  string
	endLine int
	id      string
}

var structuralPrefixPattern = regexp.MustCompile(`(?i)\b(function|method|class|interface|type|endpoint)\s*$`)

func (p *Parser) Parse(_ context.Context, input parserapi.Input) (graph.ParseResult, error) {
	b := parserapi.NewBuilder(input, "markdown")
	lineStarts := sourceLineStarts(input.Content)
	document := p.parser.Parse(mdtext.NewReader(input.Content))
	headings := collectHeadings(document, input.Content, lineStarts)
	addHeadingNodes(b, input, headings)
	addReferences(b, input, document, headings, lineStarts)
	return b.Finish(), nil
}

func collectHeadings(document mdast.Node, source []byte, lineStarts []int) []heading {
	anchors := map[string]int{}
	var result []heading
	_ = mdast.Walk(document, func(node mdast.Node, entering bool) (mdast.WalkStatus, error) {
		if !entering {
			return mdast.WalkContinue, nil
		}
		value, ok := node.(*mdast.Heading)
		if !ok {
			return mdast.WalkContinue, nil
		}
		title := strings.TrimSpace(string(nodeText(value, source)))
		if title == "" {
			title = "Untitled"
		}
		base := slug(title)
		anchor := base
		if occurrence := anchors[base]; occurrence > 0 {
			anchor += "-" + strconv.Itoa(occurrence)
		}
		anchors[base]++
		line, column := lineColumn(lineStarts, node.Pos())
		result = append(result, heading{
			offset: node.Pos(), line: line, column: column, level: value.Level,
			title: title, anchor: anchor, endLine: len(lineStarts),
		})
		return mdast.WalkContinue, nil
	})
	sort.SliceStable(result, func(i, j int) bool { return result[i].offset < result[j].offset })
	for i := range result {
		for j := i + 1; j < len(result); j++ {
			if result[j].level <= result[i].level {
				result[i].endLine = result[j].line - 1
				break
			}
		}
	}
	return result
}

func addHeadingNodes(b *parserapi.Builder, input parserapi.Input, headings []heading) {
	stack := make([]*heading, 0, 6)
	for i := range headings {
		h := &headings[i]
		for len(stack) > 0 && stack[len(stack)-1].level >= h.level {
			stack = stack[:len(stack)-1]
		}
		parentID := b.FileID()
		if len(stack) > 0 {
			parentID = stack[len(stack)-1].id
		}
		qualified := input.Path + "#" + h.anchor
		h.id = b.AddNode(graph.Node{
			Kind: graph.KindDocSection, Name: h.title, QualifiedName: qualified,
			Location:   graph.Location{Path: input.Path, Line: h.line, Column: h.column, EndLine: h.endLine},
			Properties: map[string]string{"anchor": h.anchor, "level": strconv.Itoa(h.level)},
		})
		b.AddFact(parentID, graph.EdgeContains, h.id, "", graph.KindDocSection,
			graph.Location{Path: input.Path, Line: h.line, Column: h.column}, nil)
		stack = append(stack, h)
	}
}

func addReferences(b *parserapi.Builder, input parserapi.Input, document mdast.Node, headings []heading, lineStarts []int) {
	_ = mdast.Walk(document, func(node mdast.Node, entering bool) (mdast.WalkStatus, error) {
		if !entering {
			return mdast.WalkContinue, nil
		}
		fromID := sectionAt(headings, node.Pos(), b.FileID())
		line, column := lineColumn(lineStarts, node.Pos())
		loc := graph.Location{Path: input.Path, Line: line, Column: column}
		switch value := node.(type) {
		case *mdast.Link:
			raw := string(value.Destination)
			target, kind, ok := linkTarget(input.Path, raw)
			if ok {
				b.AddFact(fromID, graph.EdgeDocuments, "", target, kind, loc,
					map[string]string{"href": raw, "syntax": "markdown_link"})
			}
		case *mdast.CodeSpan:
			keyword := structuralKeywordBefore(value, input.Content)
			target := strings.TrimSpace(string(nodeText(value, input.Content)))
			if keyword != "" && target != "" {
				b.AddFact(fromID, graph.EdgeDocuments, "", target, structuralKind(keyword), loc,
					map[string]string{"keyword": keyword, "syntax": "structural_keyword"})
			}
		}
		return mdast.WalkContinue, nil
	})
}

func structuralKeywordBefore(node mdast.Node, source []byte) string {
	var reversed []string
	length := 0
	for sibling := node.PreviousSibling(); sibling != nil && length < 128; sibling = sibling.PreviousSibling() {
		value := string(nodeText(sibling, source))
		reversed = append(reversed, value)
		length += len(value)
	}
	var prefix strings.Builder
	for i := len(reversed) - 1; i >= 0; i-- {
		prefix.WriteString(reversed[i])
	}
	runes := []rune(prefix.String())
	if len(runes) > 128 {
		runes = runes[len(runes)-128:]
	}
	match := structuralPrefixPattern.FindStringSubmatch(string(runes))
	if len(match) != 2 {
		return ""
	}
	return strings.ToLower(match[1])
}

func sectionAt(headings []heading, offset int, fileID string) string {
	index := sort.Search(len(headings), func(i int) bool { return headings[i].offset > offset })
	if index == 0 {
		return fileID
	}
	return headings[index-1].id
}

func sourceLineStarts(source []byte) []int {
	starts := []int{0}
	for index, value := range source {
		if value == '\n' && index+1 < len(source) {
			starts = append(starts, index+1)
		}
	}
	return starts
}

func lineColumn(starts []int, offset int) (int, int) {
	if offset < 0 {
		offset = 0
	}
	index := sort.Search(len(starts), func(i int) bool { return starts[i] > offset }) - 1
	if index < 0 {
		index = 0
	}
	return index + 1, offset - starts[index] + 1
}

func slug(value string) string {
	value = strings.ToLower(value)
	var result strings.Builder
	separator := false
	for _, r := range value {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if separator && result.Len() > 0 {
				result.WriteByte('-')
			}
			result.WriteRune(r)
			separator = false
		case unicode.IsSpace(r) || r == '-':
			separator = true
		}
	}
	if result.Len() == 0 {
		return "section"
	}
	return result.String()
}

func linkTarget(documentPath, raw string) (string, graph.NodeKind, bool) {
	raw = strings.Trim(strings.TrimSpace(raw), "<>")
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "" || parsed.Host != "" || strings.HasPrefix(raw, "//") {
		return "", "", false
	}
	decodedPath, err := url.PathUnescape(parsed.Path)
	if err != nil {
		return "", "", false
	}
	var targetPath string
	if decodedPath == "" {
		targetPath = documentPath
	} else if strings.HasPrefix(decodedPath, "/") {
		targetPath = strings.TrimPrefix(path.Clean(decodedPath), "/")
	} else {
		targetPath = path.Clean(path.Join(path.Dir(documentPath), decodedPath))
	}
	if targetPath == "." || targetPath == "" || targetPath == ".." || strings.HasPrefix(targetPath, "../") {
		return "", "", false
	}
	fragment, err := url.PathUnescape(parsed.Fragment)
	if err != nil {
		return "", "", false
	}
	if fragment != "" && isMarkdown(targetPath) {
		return targetPath + "#" + slug(fragment), graph.KindDocSection, true
	}
	return targetPath, graph.KindFile, true
}

func isMarkdown(filePath string) bool {
	extension := strings.ToLower(path.Ext(filePath))
	return extension == ".md" || extension == ".markdown"
}

func structuralKind(keyword string) graph.NodeKind {
	switch keyword {
	case "function":
		return graph.KindFunction
	case "method":
		return graph.KindMethod
	case "class":
		return graph.KindClass
	case "interface":
		return graph.KindInterface
	case "type":
		return graph.KindType
	case "endpoint":
		return graph.KindEndpoint
	default:
		return graph.KindExternal
	}
}

// nodeText returns the source text carried by a node's inline descendants.
// goldmark deprecated ast.Node.Text in favour of per-node accessors, so this
// walks the tree the way the removed helper did and reads each leaf through the
// accessor its own type documents.
func nodeText(node mdast.Node, source []byte) []byte {
	var buffer bytes.Buffer
	appendNodeText(&buffer, node, source)
	return buffer.Bytes()
}

func appendNodeText(buffer *bytes.Buffer, node mdast.Node, source []byte) {
	switch value := node.(type) {
	case *mdast.Text:
		buffer.Write(value.Value(source))
		return
	case *mdast.String:
		buffer.Write(value.Value)
		return
	case *mdast.AutoLink:
		buffer.Write(value.Label(source))
		return
	case *mdast.RawHTML:
		buffer.Write(value.Segments.Value(source))
		return
	}
	for child := node.FirstChild(); child != nil; child = child.NextSibling() {
		appendNodeText(buffer, child, source)
		// A soft line break ends the child's line in the source, and dropping it
		// would glue the two lines' words together.
		if breaker, ok := child.(interface{ SoftLineBreak() bool }); ok && breaker.SoftLineBreak() {
			buffer.WriteByte('\n')
		}
	}
}
