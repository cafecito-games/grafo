// Package syntax adapts the pure-Go tree-sitter runtime to the node-centric
// API grafo's language parsers are written against. The runtime addresses
// nodes through the tree and its language, so every wrapper carries the
// language that resolves node types and field names.
package syntax

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	gotreesitter "github.com/odvcencio/gotreesitter"
)

// Language is a grammar the runtime can parse with.
type Language = gotreesitter.Language

// Point is a zero-based row and column within a source file.
type Point = gotreesitter.Point

// Tree is a parsed syntax tree. Callers must Release it once done.
type Tree struct {
	tree     *gotreesitter.Tree
	language *Language
}

// Node is a single node of a parsed tree.
type Node struct {
	node     *gotreesitter.Node
	language *Language
}

// Parse parses UTF-8 source with the given grammar and abandons the parse once
// ctx is done. The runtime has no context aware entry point, so cancellation
// rides on the cancellation flag it polls periodically.
func Parse(ctx context.Context, language *Language, content []byte) (*Tree, error) {
	if language == nil {
		return nil, errors.New("parse source: no grammar")
	}
	parser := gotreesitter.NewParser(language)
	if ctx.Done() != nil {
		var cancelled uint32
		parser.SetCancellationFlag(&cancelled)
		done := make(chan struct{})
		defer close(done)
		go func() {
			select {
			case <-ctx.Done():
				atomic.StoreUint32(&cancelled, 1)
			case <-done:
			}
		}()
	}
	tree, err := parser.Parse(content)
	if err != nil {
		return nil, fmt.Errorf("parse source: %w", err)
	}
	if tree == nil {
		return nil, errors.New("parse source: parser returned no syntax tree")
	}
	return &Tree{tree: tree, language: language}, nil
}

// RootNode returns the tree's root, or nil for an empty tree.
func (t *Tree) RootNode() *Node {
	if t == nil {
		return nil
	}
	return t.wrap(t.tree.RootNode())
}

// Release returns the tree's storage to the runtime.
func (t *Tree) Release() {
	if t != nil && t.tree != nil {
		t.tree.Release()
	}
}

func (t *Tree) wrap(node *gotreesitter.Node) *Node {
	if node == nil {
		return nil
	}
	return &Node{node: node, language: t.language}
}

func (n *Node) wrap(node *gotreesitter.Node) *Node {
	if node == nil {
		return nil
	}
	return &Node{node: node, language: n.language}
}

// Kind reports the node's grammar symbol name.
func (n *Node) Kind() string {
	if n == nil {
		return ""
	}
	return n.node.Type(n.language)
}

// ChildByFieldName returns the child the grammar labels with name, or nil.
func (n *Node) ChildByFieldName(name string) *Node {
	if n == nil {
		return nil
	}
	return n.wrap(n.node.ChildByFieldName(name, n.language))
}

// NamedChildCount reports how many of the node's children are named.
func (n *Node) NamedChildCount() uint {
	if n == nil {
		return 0
	}
	return uint(n.node.NamedChildCount())
}

// NamedChild returns the index-th named child, or nil when out of range.
func (n *Node) NamedChild(index uint) *Node {
	if n == nil {
		return nil
	}
	return n.wrap(n.node.NamedChild(int(index)))
}

// Parent returns the node's parent, or nil at the root.
func (n *Node) Parent() *Node {
	if n == nil {
		return nil
	}
	return n.wrap(n.node.Parent())
}

// NextSibling returns the following sibling, named or not, or nil.
func (n *Node) NextSibling() *Node {
	if n == nil {
		return nil
	}
	return n.wrap(n.node.NextSibling())
}

// StartByte reports the node's first byte offset.
func (n *Node) StartByte() uint {
	if n == nil {
		return 0
	}
	return uint(n.node.StartByte())
}

// EndByte reports the offset one past the node's last byte.
func (n *Node) EndByte() uint {
	if n == nil {
		return 0
	}
	return uint(n.node.EndByte())
}

// StartPosition reports where the node begins.
func (n *Node) StartPosition() Point {
	if n == nil {
		return Point{}
	}
	return n.node.StartPoint()
}

// EndPosition reports where the node ends.
func (n *Node) EndPosition() Point {
	if n == nil {
		return Point{}
	}
	return n.node.EndPoint()
}

// HasError reports whether the node or a descendant failed to parse.
func (n *Node) HasError() bool {
	return n != nil && n.node.HasError()
}

// Utf8Text returns the source the node spans.
func (n *Node) Utf8Text(source []byte) string {
	if n == nil {
		return ""
	}
	return n.node.Text(source)
}
