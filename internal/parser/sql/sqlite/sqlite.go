// Package sqlite parses SQLite SQL and extracts schema declarations and relation access.
package sqlite

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	"github.com/sqlc-dev/meyer/ast"
	"github.com/sqlc-dev/meyer/parser"
)

// Parser extracts SQLite declarations and relation access from SQL files.
// Meyer follows SQLite's own grammar and exposes byte spans for every AST node.
type Parser struct{}

func New() *Parser               { return &Parser{} }
func (*Parser) Name() string     { return "sqlite" }
func (*Parser) Language() string { return "sqlite" }

// Extensions is intentionally empty. Common SQLite extensions such as .sqlite,
// .sqlite3, and .db usually contain binary databases rather than SQL source.
func (*Parser) Extensions() []string { return nil }

func (*Parser) Probe(ctx context.Context, input parserapi.Input) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := parser.Parse(ctx, bytes.NewReader(input.Content))
	return err
}

type extractor struct {
	b       *parserapi.Builder
	input   parserapi.Input
	content []byte
}

func (*Parser) Parse(ctx context.Context, input parserapi.Input) (graph.ParseResult, error) {
	b := parserapi.NewBuilder(input, "sqlite")
	if err := ctx.Err(); err != nil {
		return b.Finish(), err
	}
	statements, err := parser.Parse(ctx, bytes.NewReader(input.Content))
	if err != nil {
		return b.Finish(), fmt.Errorf("parse SQLite: %w", err)
	}
	e := &extractor{b: b, input: input, content: input.Content}
	for _, statement := range statements {
		if err := ctx.Err(); err != nil {
			return b.Finish(), err
		}
		e.parseStatement(statement)
	}
	return b.Finish(), nil
}

func (e *extractor) parseStatement(statement ast.Stmt) {
	switch statement := statement.(type) {
	case *ast.CreateTableStmt:
		e.parseCreateTable(statement)
	case *ast.CreateVirtualTableStmt:
		e.parseCreateVirtualTable(statement)
	case *ast.CreateViewStmt:
		e.parseCreateView(statement)
	case *ast.CreateIndexStmt:
		e.parseCreateIndex(statement)
	case *ast.CreateTriggerStmt:
		e.parseCreateTrigger(statement)
	default:
		e.emitAccesses(e.b.FileID(), statement)
	}
}

func (e *extractor) parseCreateTable(statement *ast.CreateTableStmt) {
	qualified := qualifiedName(statement.Name)
	if qualified == "" {
		return
	}
	properties := map[string]string{"object_kind": "table"}
	if statement.IfNotExists {
		properties["if_not_exists"] = "true"
	}
	if statement.Temp {
		properties["persistence"] = "temporary"
	}
	for _, option := range statement.Options {
		if option == nil || option.Name == nil {
			continue
		}
		switch {
		case option.Without && strings.EqualFold(option.Name.Name, "rowid"):
			properties["without_rowid"] = "true"
		case !option.Without && strings.EqualFold(option.Name.Name, "strict"):
			properties["strict"] = "true"
		}
	}
	tableID := e.b.Declare(e.b.FileID(), graph.Node{
		Kind: graph.KindTable, Name: statement.Name.Name.Name, QualifiedName: qualified,
		Location: e.location(statement.Name), Properties: properties,
	})

	for _, column := range statement.Columns {
		if column == nil || column.Name == nil || column.Name.Name == "" {
			continue
		}
		columnProperties := map[string]string{"type": ""}
		if column.Type != nil {
			columnProperties["type"] = strings.TrimSpace(column.Type.Raw)
		}
		for _, constraint := range column.Constraints {
			if constraint == nil {
				continue
			}
			switch constraint.Kind {
			case ast.ColumnNotNull:
				columnProperties["not_null"] = "true"
			case ast.ColumnPrimaryKey:
				columnProperties["primary_key"] = "true"
				if constraint.AutoIncrement {
					columnProperties["autoincrement"] = "true"
				}
			case ast.ColumnUnique:
				columnProperties["unique"] = "true"
			case ast.ColumnGenerated:
				columnProperties["generated"] = "true"
				if constraint.GeneratedKind != nil {
					columnProperties["generated_kind"] = strings.ToLower(constraint.GeneratedKind.Name)
				}
			}
		}
		location := e.location(column.Name)
		columnID := e.b.AddNode(graph.Node{
			Kind: graph.KindColumn, Name: column.Name.Name,
			QualifiedName: qualified + "." + column.Name.Name,
			Location:      location, Properties: columnProperties,
		})
		e.b.AddFact(tableID, graph.EdgeHasField, columnID, "", "", location, nil)
	}
	e.emitForeignKeys(tableID, statement)
	if statement.Select != nil {
		e.emitAccesses(tableID, statement.Select)
	}
}

func (e *extractor) parseCreateVirtualTable(statement *ast.CreateVirtualTableStmt) {
	qualified := qualifiedName(statement.Name)
	if qualified == "" {
		return
	}
	properties := map[string]string{"object_kind": "virtual_table"}
	if statement.IfNotExists {
		properties["if_not_exists"] = "true"
	}
	if statement.Module != nil {
		properties["module"] = statement.Module.Name
	}
	e.b.Declare(e.b.FileID(), graph.Node{
		Kind: graph.KindTable, Name: statement.Name.Name.Name, QualifiedName: qualified,
		Location: e.location(statement.Name), Properties: properties,
	})
}

func (e *extractor) parseCreateView(statement *ast.CreateViewStmt) {
	qualified := qualifiedName(statement.Name)
	if qualified == "" {
		return
	}
	properties := map[string]string{"object_kind": "view"}
	if statement.IfNotExists {
		properties["if_not_exists"] = "true"
	}
	if statement.Temp {
		properties["persistence"] = "temporary"
	}
	viewID := e.b.Declare(e.b.FileID(), graph.Node{
		Kind: graph.KindView, Name: statement.Name.Name.Name, QualifiedName: qualified,
		Location: e.location(statement.Name), Properties: properties,
	})
	e.emitAccesses(viewID, statement.Select)
}

func (e *extractor) parseCreateIndex(statement *ast.CreateIndexStmt) {
	qualified := qualifiedName(statement.Name)
	if qualified == "" || statement.Table == nil || statement.Table.Name == "" {
		return
	}
	properties := map[string]string{"object_kind": "index"}
	if statement.IfNotExists {
		properties["if_not_exists"] = "true"
	}
	if statement.Unique {
		properties["unique"] = "true"
	}
	indexID := e.b.Declare(e.b.FileID(), graph.Node{
		Kind: graph.KindIndex, Name: statement.Name.Name.Name, QualifiedName: qualified,
		Location: e.location(statement.Name), Properties: properties,
	})
	e.b.AddFact(indexID, graph.EdgeReferences, "", statement.Table.Name, graph.KindTable, e.location(statement.Table), nil)
}

func (e *extractor) parseCreateTrigger(statement *ast.CreateTriggerStmt) {
	qualified := qualifiedName(statement.Name)
	if qualified == "" {
		return
	}
	properties := map[string]string{
		"object_kind": "trigger",
		"event":       strings.ToLower(statement.Event),
	}
	if statement.IfNotExists {
		properties["if_not_exists"] = "true"
	}
	if statement.Temp {
		properties["persistence"] = "temporary"
	}
	if statement.HasTime {
		properties["timing"] = strings.ToLower(statement.Time.String())
	}
	triggerID := e.b.Declare(e.b.FileID(), graph.Node{
		Kind: graph.KindFunction, Name: statement.Name.Name.Name, QualifiedName: qualified,
		Location: e.location(statement.Name), Properties: properties,
	})
	if target := qualifiedName(statement.Table); target != "" {
		e.b.AddFact(triggerID, graph.EdgeReferences, "", target, "", e.location(statement.Table), nil)
	}
	e.emitAccesses(triggerID, statement)
}

func (e *extractor) emitForeignKeys(fromID string, root ast.Node) {
	seen := map[string]bool{}
	ast.Walk(root, func(node ast.Node) bool {
		reference, ok := node.(*ast.ForeignKeyClause)
		if !ok || reference.Table == nil || reference.Table.Name == "" || seen[reference.Table.Name] {
			return true
		}
		seen[reference.Table.Name] = true
		e.b.AddFact(fromID, graph.EdgeReferences, "", reference.Table.Name, graph.KindTable, e.location(reference.Table), nil)
		return true
	})
}

func (e *extractor) emitAccesses(fromID string, root ast.Node) {
	if root == nil {
		return
	}
	cteReferences := markCTEReferences(root)
	seen := map[string]bool{}
	emit := func(kind graph.EdgeKind, name string, node ast.Node) {
		if name == "" {
			return
		}
		key := string(kind) + "\x00" + name
		if seen[key] {
			return
		}
		seen[key] = true
		e.b.AddFact(fromID, kind, "", name, "", e.location(node), nil)
	}
	ast.Walk(root, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.TableRef:
			if !cteReferences[node] {
				emit(graph.EdgeReads, qualifiedName(node.Name), node.Name)
			}
		case *ast.InsertStmt:
			emit(graph.EdgeWrites, qualifiedName(node.Table), node.Table)
		case *ast.UpdateStmt:
			emit(graph.EdgeWrites, qualifiedName(node.Table), node.Table)
		case *ast.DeleteStmt:
			emit(graph.EdgeWrites, qualifiedName(node.Table), node.Table)
		case *ast.AlterTableStmt:
			emit(graph.EdgeWrites, qualifiedName(node.Table), node.Table)
		}
		return true
	})
}

// markCTEReferences distinguishes statement-local CTEs from persistent
// relations. Each recursive walk receives its enclosing CTE scope, and a
// nested WITH extends only its own subtree.
func markCTEReferences(root ast.Node) map[*ast.TableRef]bool {
	result := map[*ast.TableRef]bool{}
	var walk func(ast.Node, map[string]bool)
	walk = func(node ast.Node, scope map[string]bool) {
		if node == nil {
			return
		}
		var with *ast.With
		switch node := node.(type) {
		case *ast.SelectStmt:
			with = node.With
		case *ast.InsertStmt:
			with = node.With
		case *ast.UpdateStmt:
			with = node.With
		case *ast.DeleteStmt:
			with = node.With
		}
		if with != nil && len(with.CTEs) > 0 {
			extended := make(map[string]bool, len(scope)+len(with.CTEs))
			for name := range scope {
				extended[name] = true
			}
			for _, cte := range with.CTEs {
				if cte != nil && cte.Name != nil {
					extended[strings.ToLower(cte.Name.Name)] = true
				}
			}
			scope = extended
		}
		if table, ok := node.(*ast.TableRef); ok && table.Name != nil && table.Name.Schema == nil && !table.HasArgs && scope[strings.ToLower(table.Name.Name.Name)] {
			result[table] = true
		}
		for _, child := range node.Children() {
			walk(child, scope)
		}
	}
	walk(root, nil)
	return result
}

func (e *extractor) location(node ast.Node) graph.Location {
	if node == nil {
		return graph.Location{Path: e.input.Path, Line: 1, Column: 1, EndLine: 1}
	}
	start, end := node.Pos(), node.End()
	if start < 0 {
		start = 0
	}
	if start > len(e.content) {
		start = len(e.content)
	}
	if end < start {
		end = start
	}
	if end > len(e.content) {
		end = len(e.content)
	}
	prefix := e.content[:start]
	line := bytes.Count(prefix, []byte{'\n'}) + 1
	lastNewline := bytes.LastIndexByte(prefix, '\n')
	column := len(prefix) - lastNewline
	endLine := line + bytes.Count(e.content[start:end], []byte{'\n'})
	return graph.Location{Path: e.input.Path, Line: line, Column: column, EndLine: endLine}
}

func qualifiedName(name *ast.QualifiedName) string {
	if name == nil || name.Name == nil || name.Name.Name == "" {
		return ""
	}
	if name.Schema != nil && name.Schema.Name != "" {
		return name.Schema.Name + "." + name.Name.Name
	}
	return name.Name.Name
}
