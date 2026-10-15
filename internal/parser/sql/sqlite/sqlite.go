// Package sqlite parses SQLite SQL and extracts schema declarations and relation access.
package sqlite

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	"github.com/cafecito-games/grafo/internal/parser/sqltemplate"
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
	document := sqltemplate.Analyze(input.Content, "sqlite")
	for _, diagnostic := range document.Diagnostics {
		if diagnostic.Fatal {
			return fmt.Errorf("SQL template: %s", diagnostic.Message)
		}
	}
	_, err := parser.Parse(ctx, bytes.NewReader(document.SQL))
	return err
}

type extractor struct {
	b        *parserapi.Builder
	input    parserapi.Input
	content  []byte
	document sqltemplate.Document
	queries  map[string]string
}

func (*Parser) Parse(ctx context.Context, input parserapi.Input) (graph.ParseResult, error) {
	b := parserapi.NewBuilder(input, "sqlite")
	if err := ctx.Err(); err != nil {
		return b.Finish(), err
	}
	document := sqltemplate.Analyze(input.Content, "sqlite")
	for _, diagnostic := range document.Diagnostics {
		line, _ := document.Position(diagnostic.Offset)
		b.Diagnostic(line, "warning", "SQL template: "+diagnostic.Message)
	}
	if document.HasFatalDiagnostics() {
		return b.Finish(), nil
	}
	queries := declareQueries(b, input, document, "sqlite")
	statements, err := parser.Parse(ctx, bytes.NewReader(document.SQL))
	if err != nil {
		return b.Finish(), fmt.Errorf("parse SQLite: %w", err)
	}
	e := &extractor{b: b, input: input, content: input.Content, document: document, queries: queries}
	for _, statement := range statements {
		if err := ctx.Err(); err != nil {
			return b.Finish(), err
		}
		e.parseStatement(statement)
	}
	return b.Finish(), nil
}

func (e *extractor) parseStatement(statement ast.Stmt) {
	owner := e.b.FileID()
	if query, ok := e.document.QueryAt(statement.Pos()); ok {
		owner = e.queries[query.Name]
	}
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
		e.emitAccesses(owner, statement)
	}
}

func declareQueries(b *parserapi.Builder, input parserapi.Input, document sqltemplate.Document, dialect string) map[string]string {
	result := make(map[string]string, len(document.Queries))
	for _, query := range document.Queries {
		line, column := document.Position(query.NameStart)
		endLine, _ := document.Position(query.StatementEnd)
		properties := map[string]string{
			"command": query.Command, "dialect": dialect,
			"canonical_sql":   string(input.Content[query.StatementStart:query.StatementEnd]),
			"statement_start": strconv.Itoa(query.StatementStart), "statement_end": strconv.Itoa(query.StatementEnd),
		}
		var embeds, slices []string
		for _, macro := range document.Macros {
			if macro.Start < query.StatementStart || macro.Start >= query.StatementEnd {
				continue
			}
			if macro.Embed {
				embeds = append(embeds, macro.Argument)
			}
			if macro.Collection {
				slices = append(slices, macro.Argument)
			}
		}
		if len(embeds) > 0 {
			properties["sqlc_embed"] = strings.Join(embeds, ",")
		}
		if len(slices) > 0 {
			properties["sqlc_slice"] = strings.Join(slices, ",")
		}
		id := b.Declare(b.FileID(), graph.Node{
			Kind: graph.KindSQLQuery, Name: query.Name, QualifiedName: query.QualifiedName(input.Path),
			Location: graph.Location{Path: input.Path, Line: line, Column: column, EndLine: endLine}, Properties: properties,
		})
		result[query.Name] = id
	}
	return result
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
	aliases := map[string]string{}
	uniqueRelations := map[string]bool{}
	resultAliases := map[string]bool{}
	excludedIdentifiers := map[*ast.Ident]bool{}
	var qualifiedReferences []*ast.QualifiedRef
	var unqualifiedReferences []*ast.Ident
	type columnWrite struct {
		table, column string
		node          ast.Node
	}
	var columnWrites []columnWrite
	hasCTE := false
	hasNonTableSource := false
	registerAlias := func(name, table string) {
		if name == "" || table == "" {
			return
		}
		if existing, exists := aliases[name]; exists && existing != table {
			aliases[name] = ""
			return
		}
		aliases[name] = table
	}
	registerTable := func(name *ast.QualifiedName, alias *ast.Ident) {
		table := qualifiedName(name)
		if table == "" {
			return
		}
		uniqueRelations[table] = true
		registerAlias(name.Name.Name, table)
		registerAlias(table, table)
		if alias != nil && alias.Name != "" {
			registerAlias(alias.Name, table)
		}
	}
	ast.Walk(root, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.TableRef:
			if node.Name == nil || node.HasArgs || node.Select != nil || len(node.List) > 0 {
				hasNonTableSource = true
			}
			if !cteReferences[node] {
				table := qualifiedName(node.Name)
				if table != "" {
					uniqueRelations[table] = true
					registerAlias(node.Name.Name.Name, table)
					registerAlias(table, table)
					if node.Alias != nil && node.Alias.Name != "" {
						registerAlias(node.Alias.Name, table)
					}
				}
			}
			if node.Alias != nil {
				excludedIdentifiers[node.Alias] = true
			}
			if node.IndexedBy != nil {
				excludedIdentifiers[node.IndexedBy] = true
			}
		case *ast.QualifiedName:
			if node.Name != nil {
				excludedIdentifiers[node.Name] = true
			}
			if node.Schema != nil {
				excludedIdentifiers[node.Schema] = true
			}
		case *ast.QualifiedRef:
			qualifiedReferences = append(qualifiedReferences, node)
			for _, part := range node.Parts {
				excludedIdentifiers[part] = true
			}
		case *ast.FuncCall:
			if node.Name != nil {
				excludedIdentifiers[node.Name] = true
			}
		case *ast.ResultColumn:
			if node.Alias != nil {
				excludedIdentifiers[node.Alias] = true
				resultAliases[node.Alias.Name] = true
			}
		case *ast.WindowDef:
			if node.Name != nil {
				excludedIdentifiers[node.Name] = true
				resultAliases[node.Name.Name] = true
			}
			if node.Base != nil {
				excludedIdentifiers[node.Base] = true
				resultAliases[node.Base.Name] = true
			}
		case *ast.Star:
			if node.Table != nil {
				excludedIdentifiers[node.Table] = true
			}
		case *ast.CTE:
			hasCTE = true
			if node.Name != nil {
				excludedIdentifiers[node.Name] = true
			}
		}
		return true
	})
	if insert, ok := root.(*ast.InsertStmt); ok {
		table := qualifiedName(insert.Table)
		registerTable(insert.Table, insert.Alias)
		for _, column := range insert.Columns {
			excludedIdentifiers[column] = true
			columnWrites = append(columnWrites, columnWrite{table: table, column: column.Name, node: column})
		}
	}
	if update, ok := root.(*ast.UpdateStmt); ok {
		table := qualifiedName(update.Table)
		registerTable(update.Table, update.Alias)
		for _, pair := range update.Set {
			for _, column := range pair.Columns {
				excludedIdentifiers[column] = true
				columnWrites = append(columnWrites, columnWrite{table: table, column: column.Name, node: column})
			}
		}
	}
	if deleteStatement, ok := root.(*ast.DeleteStmt); ok {
		registerTable(deleteStatement.Table, deleteStatement.Alias)
	}
	ast.Walk(root, func(node ast.Node) bool {
		if identifier, ok := node.(*ast.Ident); ok && !excludedIdentifiers[identifier] {
			unqualifiedReferences = append(unqualifiedReferences, identifier)
		}
		return true
	})
	emitWithProperties := func(kind graph.EdgeKind, name string, node ast.Node, properties map[string]string) {
		if name == "" {
			return
		}
		key := string(kind) + "\x00" + name
		if seen[key] {
			return
		}
		seen[key] = true
		e.b.AddFact(fromID, kind, "", name, "", e.location(node), properties)
	}
	emit := func(kind graph.EdgeKind, name string, node ast.Node) { emitWithProperties(kind, name, node, nil) }
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
			emitWithProperties(graph.EdgeWrites, qualifiedName(node.Table), node.Table, map[string]string{"statement": "alter_table"})
		case *ast.DropTableStmt:
			emitWithProperties(graph.EdgeWrites, qualifiedName(node.Name), node.Name, map[string]string{"statement": "schema_mutation"})
		}
		return true
	})
	emitColumn := func(kind graph.EdgeKind, name string, node ast.Node) {
		if name == "" {
			return
		}
		key := string(kind) + "\x00column\x00" + name
		if seen[key] {
			return
		}
		seen[key] = true
		e.b.AddFact(fromID, kind, "", name, graph.KindColumn, e.location(node), nil)
	}
	for _, reference := range qualifiedReferences {
		parts := make([]string, 0, len(reference.Parts))
		for _, part := range reference.Parts {
			parts = append(parts, part.Name)
		}
		if len(parts) < 2 {
			continue
		}
		column := parts[len(parts)-1]
		if len(parts) == 2 {
			if table := aliases[parts[0]]; table != "" {
				emitColumn(graph.EdgeReads, table+"."+column, reference)
			}
		} else {
			emitColumn(graph.EdgeReads, strings.Join(parts[:len(parts)-1], ".")+"."+column, reference)
		}
	}
	if !hasCTE && !hasNonTableSource && len(uniqueRelations) == 1 {
		for table := range uniqueRelations {
			for _, reference := range unqualifiedReferences {
				if resultAliases[reference.Name] {
					continue
				}
				emitColumn(graph.EdgeReads, table+"."+reference.Name, reference)
			}
		}
	}
	for _, write := range columnWrites {
		emitColumn(graph.EdgeWrites, write.table+"."+write.column, write.node)
	}
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
