package parser

import (
	"context"

	treesitter "github.com/tree-sitter/go-tree-sitter"
)

// ParseTreeSitter parses UTF-8 source with an already-configured tree-sitter
// parser and abandons the parse once ctx is done. Tree-sitter has no context
// aware entry point, so cancellation rides on the progress callback it invokes
// periodically: returning true there stops parsing and yields a nil tree, which
// callers must treat as a failed parse rather than an empty file.
func ParseTreeSitter(ctx context.Context, p *treesitter.Parser, content []byte) *treesitter.Tree {
	read := func(offset int, _ treesitter.Point) []byte {
		if offset < len(content) {
			return content[offset:]
		}
		return []byte{}
	}
	if ctx.Done() == nil {
		return p.ParseWithOptions(read, nil, nil)
	}
	return p.ParseWithOptions(read, nil, &treesitter.ParseOptions{
		ProgressCallback: func(treesitter.ParseState) bool { return ctx.Err() != nil },
	})
}
