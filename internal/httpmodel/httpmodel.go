// Package httpmodel owns the language-neutral identity and compatibility
// contract for statically observed HTTP methods and routes.
package httpmodel

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// MatchRank orders compatible declarations from strongest to weakest. RankNone
// is not compatible; lower non-zero values are stronger matches.
type MatchRank uint8

const (
	RankNone MatchRank = iota
	RankExact
	RankTemplate
	RankCatchAll
)

type segmentKind uint8

const (
	segmentLiteral segmentKind = iota
	segmentParameter
	segmentRegex
	segmentCatchAll
)

type segment struct {
	kind       segmentKind
	literal    string
	constraint string
	pattern    *regexp.Regexp
}

// Route separates source evidence from the canonical path identity used for
// matching. Canonical never contains a query or fragment, and parameter names
// are replaced without erasing regex constraints or catchall semantics.
type Route struct {
	Raw       string
	Path      string
	Canonical string
	Query     string
	Fragment  string
	Scheme    string
	Authority string
	segments  []segment
}

// NormalizeMethod trims, validates, and canonicalizes an HTTP method token.
func NormalizeMethod(value string) (string, error) {
	method := strings.Trim(value, " ")
	if method == "" {
		return "", fmt.Errorf("HTTP method is empty")
	}
	for _, character := range method {
		if !methodTokenCharacter(character) {
			return "", fmt.Errorf("invalid HTTP method %q", value)
		}
	}
	return strings.ToUpper(method), nil
}

func methodTokenCharacter(character rune) bool {
	if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' {
		return true
	}
	return strings.ContainsRune("!#$%&'*+-.^_`|~", character)
}

// ParseRoute parses an absolute path or URL without decoding escaped path
// separators. It rejects relative, malformed, and structurally unsafe input.
func ParseRoute(value string) (Route, error) {
	raw := strings.Trim(value, " ")
	if raw == "" {
		return Route{}, fmt.Errorf("HTTP route is empty")
	}
	if strings.ContainsAny(raw, "\r\n\t") {
		return Route{}, fmt.Errorf("invalid HTTP route %q", value)
	}
	if _, err := url.Parse(raw); err != nil {
		return Route{}, fmt.Errorf("parse HTTP route %q: %w", value, err)
	}

	core, fragment := cutOutsideTemplate(raw, '#')
	core, query := cutOutsideTemplate(core, '?')
	if _, err := url.QueryUnescape(query); err != nil {
		return Route{}, fmt.Errorf("invalid HTTP route query in %q: %w", value, err)
	}
	if _, err := url.PathUnescape(fragment); err != nil {
		return Route{}, fmt.Errorf("invalid HTTP route fragment in %q: %w", value, err)
	}
	path, scheme, authority := core, "", ""
	if parsed, err := url.Parse(core); err != nil {
		return Route{}, fmt.Errorf("parse HTTP route %q: %w", value, err)
	} else if parsed.IsAbs() {
		if !strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" || parsed.Opaque != "" {
			return Route{}, fmt.Errorf("invalid absolute HTTP URL %q", value)
		}
		authority = parsed.Host
		scheme = strings.ToLower(parsed.Scheme)
		path = parsed.Path
		if parsed.RawPath != "" {
			path = parsed.RawPath
		}
		if path == "" {
			path = "/"
		}
	} else if !strings.HasPrefix(core, "/") || strings.HasPrefix(core, "//") {
		return Route{}, fmt.Errorf("HTTP route %q must be an absolute path or URL", value)
	}
	if _, err := url.PathUnescape(path); err != nil {
		return Route{}, fmt.Errorf("invalid HTTP route %q: %w", value, err)
	}
	if hasDotSegment(path) {
		return Route{}, fmt.Errorf("HTTP route %q contains a dot segment", value)
	}
	if path != "/" {
		path = strings.TrimRight(path, "/")
		if path == "" {
			path = "/"
		}
	}
	segments, canonical, err := parseSegments(path)
	if err != nil {
		return Route{}, fmt.Errorf("invalid HTTP route %q: %w", value, err)
	}
	return Route{Raw: raw, Path: path, Canonical: canonical, Query: query,
		Fragment: fragment, Scheme: scheme, Authority: authority, segments: segments}, nil
}

func cutOutsideTemplate(value string, separator byte) (string, string) {
	depth := 0
	escaped := false
	inClass := false
	for index := 0; index < len(value); index++ {
		character := value[index]
		if escaped {
			escaped = false
			continue
		}
		if character == '\\' && depth > 0 {
			escaped = true
			continue
		}
		if depth > 0 {
			switch character {
			case '[':
				inClass = true
			case ']':
				inClass = false
			}
		}
		if !inClass {
			switch character {
			case '{':
				depth++
			case '}':
				if depth > 0 {
					depth--
				}
			}
		}
		if character == separator && depth == 0 {
			return value[:index], value[index+1:]
		}
	}
	return value, ""
}

func hasDotSegment(path string) bool {
	for _, value := range strings.Split(path, "/") {
		decoded, err := url.PathUnescape(value)
		if err == nil && (decoded == "." || decoded == "..") {
			return true
		}
	}
	return false
}

func parseSegments(path string) ([]segment, string, error) {
	if path == "/" {
		return nil, "/", nil
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	segments := make([]segment, 0, len(parts))
	canonical := make([]string, 0, len(parts))
	for index, part := range parts {
		parsed, identity, err := parseSegment(part)
		if err != nil {
			return nil, "", err
		}
		if parsed.kind == segmentCatchAll && index != len(parts)-1 {
			return nil, "", fmt.Errorf("catchall must be the final segment")
		}
		segments = append(segments, parsed)
		canonical = append(canonical, identity)
	}
	return segments, "/" + strings.Join(canonical, "/"), nil
}

func parseSegment(value string) (segment, string, error) {
	if value == "*" || strings.HasPrefix(value, "*") && len(value) > 1 {
		return segment{kind: segmentCatchAll}, "{_...}", nil
	}
	if !strings.ContainsAny(value, "{}") {
		if strings.Contains(value, "\\") {
			return segment{}, "", fmt.Errorf("literal segment contains a backslash")
		}
		return segment{kind: segmentLiteral, literal: value}, value, nil
	}
	if len(value) < 3 || value[0] != '{' || value[len(value)-1] != '}' {
		return segment{}, "", fmt.Errorf("malformed template segment %q", value)
	}
	inside := value[1 : len(value)-1]
	if strings.HasSuffix(inside, "...") {
		if strings.TrimSuffix(inside, "...") == "" {
			return segment{}, "", fmt.Errorf("catchall name is empty")
		}
		return segment{kind: segmentCatchAll}, "{_...}", nil
	}
	name, constraint, constrained := strings.Cut(inside, ":")
	if name == "" || strings.ContainsAny(name, "{}:/") {
		return segment{}, "", fmt.Errorf("invalid parameter name %q", name)
	}
	if !constrained {
		return segment{kind: segmentParameter}, "{_}", nil
	}
	if constraint == "" {
		return segment{}, "", fmt.Errorf("empty constraint for parameter %q", name)
	}
	pattern, err := regexp.Compile("^(?:" + constraint + ")$")
	if err != nil {
		return segment{}, "", fmt.Errorf("invalid constraint for parameter %q: %w", name, err)
	}
	return segment{kind: segmentRegex, constraint: constraint, pattern: pattern}, "{_:" + constraint + "}", nil
}

// Join composes a router prefix and leaf through the same parser used for
// standalone identities. Query and fragment evidence belongs to the leaf.
func Join(prefixValue, leafValue string) (Route, error) {
	prefix, err := ParseRoute(prefixValue)
	if err != nil {
		return Route{}, err
	}
	leaf, err := ParseRoute(leafValue)
	if err != nil {
		return Route{}, err
	}
	if prefix.Authority != "" || prefix.Query != "" || prefix.Fragment != "" || leaf.Authority != "" {
		return Route{}, fmt.Errorf("route joining accepts path-only prefix and leaf")
	}
	joined := strings.TrimRight(prefix.Path, "/") + "/" + strings.TrimLeft(leaf.Path, "/")
	if prefix.Path == "/" {
		joined = "/" + strings.TrimLeft(leaf.Path, "/")
	}
	if leaf.Path == "/" {
		joined = prefix.Path
	}
	if leaf.Query != "" {
		joined += "?" + leaf.Query
	}
	if leaf.Fragment != "" {
		joined += "#" + leaf.Fragment
	}
	return ParseRoute(joined)
}

// Compatibility reports whether request evidence can be proven to satisfy a
// declaration. The declaration is directional: an unknown request value never
// proves a literal or regex-constrained declaration.
func Compatibility(declaration, request Route) MatchRank {
	if declaration.Canonical == request.Canonical && allLiteral(declaration.segments) && allLiteral(request.segments) {
		return RankExact
	}
	declarationIndex, requestIndex := 0, 0
	hasCatchAll := false
	for declarationIndex < len(declaration.segments) {
		declared := declaration.segments[declarationIndex]
		if declared.kind == segmentCatchAll {
			if requestIndex >= len(request.segments) {
				return RankNone
			}
			hasCatchAll = true
			requestIndex = len(request.segments)
			declarationIndex++
			break
		}
		if requestIndex >= len(request.segments) || !compatibleSegment(declared, request.segments[requestIndex]) {
			return RankNone
		}
		declarationIndex++
		requestIndex++
	}
	if declarationIndex != len(declaration.segments) || requestIndex != len(request.segments) {
		return RankNone
	}
	if hasCatchAll {
		return RankCatchAll
	}
	return RankTemplate
}

func allLiteral(segments []segment) bool {
	for _, value := range segments {
		if value.kind != segmentLiteral {
			return false
		}
	}
	return true
}

func compatibleSegment(declaration, request segment) bool {
	switch declaration.kind {
	case segmentLiteral:
		return request.kind == segmentLiteral && declaration.literal == request.literal
	case segmentParameter:
		return request.kind != segmentCatchAll
	case segmentRegex:
		switch request.kind {
		case segmentLiteral:
			return declaration.pattern.MatchString(request.literal)
		case segmentRegex:
			return declaration.constraint == request.constraint
		default:
			return false
		}
	default:
		return false
	}
}
