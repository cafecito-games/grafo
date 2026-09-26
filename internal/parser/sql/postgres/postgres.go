package postgres

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	pg_query "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Parser extracts PostgreSQL declarations and relation access from SQL files.
// It uses PostgreSQL's parser, so dialect-specific syntax is interpreted with
// the same grammar as the database rather than with text matching.
type Parser struct{}

func New() *Parser                   { return &Parser{} }
func (*Parser) Name() string         { return "postgres" }
func (*Parser) Language() string     { return "postgresql" }
func (*Parser) Extensions() []string { return []string{".pgsql", ".psql"} }

func (*Parser) Probe(ctx context.Context, input parserapi.Input) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := pg_query.Parse(string(input.Content))
	return err
}

type extractor struct {
	b       *parserapi.Builder
	input   parserapi.Input
	content []byte
}

func (*Parser) Parse(ctx context.Context, input parserapi.Input) (graph.ParseResult, error) {
	b := parserapi.NewBuilder(input, "postgresql")
	if err := ctx.Err(); err != nil {
		return b.Finish(), err
	}
	tree, err := pg_query.Parse(string(input.Content))
	if err != nil {
		return b.Finish(), fmt.Errorf("parse PostgreSQL: %w", err)
	}
	e := &extractor{b: b, input: input, content: input.Content}
	for _, statement := range tree.GetStmts() {
		if err := ctx.Err(); err != nil {
			return b.Finish(), err
		}
		e.parseStatement(statement)
	}
	return b.Finish(), nil
}

func (e *extractor) parseStatement(raw *pg_query.RawStmt) {
	if raw == nil || raw.GetStmt() == nil {
		return
	}
	statement := raw.GetStmt()
	switch {
	case statement.GetCreateStmt() != nil:
		e.parseCreateTable(raw, statement.GetCreateStmt())
	case statement.GetViewStmt() != nil:
		e.parseCreateView(raw, statement.GetViewStmt())
	case statement.GetCreateTableAsStmt() != nil:
		e.parseCreateTableAs(raw, statement.GetCreateTableAsStmt())
	case statement.GetCreateFunctionStmt() != nil:
		e.parseCreateFunction(raw, statement.GetCreateFunctionStmt())
	case statement.GetIndexStmt() != nil:
		e.parseCreateIndex(raw, statement.GetIndexStmt())
	default:
		e.emitAccesses(e.b.FileID(), statement.ProtoReflect())
	}
}

func (e *extractor) parseCreateTable(raw *pg_query.RawStmt, statement *pg_query.CreateStmt) {
	relation := statement.GetRelation()
	if relation == nil || relation.GetRelname() == "" {
		return
	}
	qualified := relationName(relation)
	properties := map[string]string{"object_kind": "table"}
	if statement.GetIfNotExists() {
		properties["if_not_exists"] = "true"
	}
	if method := statement.GetAccessMethod(); method != "" {
		properties["access_method"] = method
	}
	if persistence := relationPersistence(relation.GetRelpersistence()); persistence != "" {
		properties["persistence"] = persistence
	}
	tableID := e.b.Declare(e.b.FileID(), graph.Node{Kind: graph.KindTable, Name: relation.GetRelname(),
		QualifiedName: qualified, Location: e.location(relation.GetLocation(), raw.GetStmtLocation()), Properties: properties})

	for _, item := range statement.GetTableElts() {
		column := item.GetColumnDef()
		if column == nil || column.GetColname() == "" {
			continue
		}
		columnProperties := map[string]string{"type": typeName(column.GetTypeName())}
		if column.GetIsNotNull() {
			columnProperties["not_null"] = "true"
		}
		if column.GetIdentity() != "" {
			columnProperties["identity"] = column.GetIdentity()
		}
		if column.GetGenerated() != "" {
			columnProperties["generated"] = column.GetGenerated()
		}
		loc := e.location(column.GetLocation(), raw.GetStmtLocation())
		columnID := e.b.AddNode(graph.Node{Kind: graph.KindColumn, Name: column.GetColname(),
			QualifiedName: qualified + "." + column.GetColname(), Location: loc, Properties: columnProperties})
		e.b.AddFact(tableID, graph.EdgeHasField, columnID, "", "", loc, nil)
	}
	for _, inherited := range statement.GetInhRelations() {
		if relation := inherited.GetRangeVar(); relation != nil {
			e.b.AddFact(tableID, graph.EdgeExtends, "", relationName(relation), graph.KindTable,
				e.location(relation.GetLocation(), raw.GetStmtLocation()), nil)
		}
	}
	for _, item := range statement.GetTableElts() {
		e.emitReferences(tableID, item.ProtoReflect())
	}
	for _, constraint := range statement.GetConstraints() {
		e.emitReferences(tableID, constraint.ProtoReflect())
	}
}

func (e *extractor) parseCreateView(raw *pg_query.RawStmt, statement *pg_query.ViewStmt) {
	view := statement.GetView()
	if view == nil || view.GetRelname() == "" {
		return
	}
	properties := map[string]string{"object_kind": "view"}
	if statement.GetReplace() {
		properties["replace"] = "true"
	}
	viewID := e.b.Declare(e.b.FileID(), graph.Node{Kind: graph.KindView, Name: view.GetRelname(),
		QualifiedName: relationName(view), Location: e.location(view.GetLocation(), raw.GetStmtLocation()), Properties: properties})
	if statement.GetQuery() != nil {
		e.emitAccesses(viewID, statement.GetQuery().ProtoReflect())
	}
}

func (e *extractor) parseCreateTableAs(raw *pg_query.RawStmt, statement *pg_query.CreateTableAsStmt) {
	if statement.GetInto() == nil || statement.GetInto().GetRel() == nil {
		return
	}
	relation := statement.GetInto().GetRel()
	kind := graph.KindTable
	objectKind := "table"
	if statement.GetObjtype() == pg_query.ObjectType_OBJECT_MATVIEW {
		kind = graph.KindView
		objectKind = "materialized_view"
	}
	properties := map[string]string{"object_kind": objectKind}
	if statement.GetIfNotExists() {
		properties["if_not_exists"] = "true"
	}
	id := e.b.Declare(e.b.FileID(), graph.Node{Kind: kind, Name: relation.GetRelname(),
		QualifiedName: relationName(relation), Location: e.location(relation.GetLocation(), raw.GetStmtLocation()), Properties: properties})
	if statement.GetQuery() != nil {
		e.emitAccesses(id, statement.GetQuery().ProtoReflect())
	}
}

func (e *extractor) parseCreateFunction(raw *pg_query.RawStmt, statement *pg_query.CreateFunctionStmt) {
	qualified := nodeName(statement.GetFuncname())
	if qualified == "" {
		return
	}
	name := graph.SimpleName(qualified)
	objectKind := "function"
	if statement.GetIsProcedure() {
		objectKind = "procedure"
	}
	properties := map[string]string{"object_kind": objectKind}
	if statement.GetReplace() {
		properties["replace"] = "true"
	}
	if resultType := typeName(statement.GetReturnType()); resultType != "" {
		properties["returns"] = resultType
	}
	signature := functionSignature(statement.GetParameters())
	properties["signature"] = signature
	loc := e.location(e.statementOffset(raw), raw.GetStmtLocation())
	functionID := e.b.Declare(e.b.FileID(), graph.Node{ID: graph.NodeID(graph.KindFunction, qualified, signature, e.input.RepoID, e.input.Path),
		Kind: graph.KindFunction, Name: name,
		QualifiedName: qualified, Location: loc, Properties: properties})
	for index, item := range statement.GetParameters() {
		parameter := item.GetFunctionParameter()
		if parameter == nil {
			continue
		}
		parameterName := parameter.GetName()
		if parameterName == "" {
			parameterName = fmt.Sprintf("$%d", index+1)
		}
		parameterLoc := loc
		if parameter.GetArgType() != nil {
			parameterLoc = e.location(parameter.GetArgType().GetLocation(), raw.GetStmtLocation())
		}
		e.b.Declare(functionID, graph.Node{ID: graph.NodeID(graph.KindParameter, qualified+"."+parameterName, signature, e.input.RepoID, e.input.Path),
			Kind: graph.KindParameter, Name: parameterName,
			QualifiedName: qualified + "." + parameterName, Location: parameterLoc,
			Properties: map[string]string{"type": typeName(parameter.GetArgType()), "mode": parameterMode(parameter.GetMode())}})
	}
	if statement.GetSqlBody() != nil {
		e.emitAccesses(functionID, statement.GetSqlBody().ProtoReflect())
	}
	if strings.EqualFold(firstFunctionOption(statement, "language"), "sql") {
		for _, body := range functionOptions(statement, "as") {
			tree, err := pg_query.Parse(body)
			if err != nil {
				e.b.Diagnostic(loc.Line, "warning", fmt.Sprintf("parse SQL function body for %s: %v", qualified, err))
				continue
			}
			for _, bodyStatement := range tree.GetStmts() {
				if bodyStatement.GetStmt() != nil {
					e.emitAccesses(functionID, bodyStatement.GetStmt().ProtoReflect())
				}
			}
		}
	}
}

func (e *extractor) parseCreateIndex(raw *pg_query.RawStmt, statement *pg_query.IndexStmt) {
	if statement.GetIdxname() == "" || statement.GetRelation() == nil {
		return
	}
	relation := statement.GetRelation()
	qualified := statement.GetIdxname()
	if relation.GetSchemaname() != "" {
		qualified = relation.GetSchemaname() + "." + qualified
	}
	properties := map[string]string{"object_kind": "index"}
	if statement.GetUnique() {
		properties["unique"] = "true"
	}
	if method := statement.GetAccessMethod(); method != "" {
		properties["access_method"] = method
	}
	indexID := e.b.Declare(e.b.FileID(), graph.Node{Kind: graph.KindIndex, Name: statement.GetIdxname(),
		QualifiedName: qualified, Location: e.location(relation.GetLocation(), raw.GetStmtLocation()), Properties: properties})
	e.b.AddFact(indexID, graph.EdgeReferences, "", relationName(relation), "",
		e.location(relation.GetLocation(), raw.GetStmtLocation()), nil)
}

func (e *extractor) emitReferences(fromID string, root protoreflect.Message) {
	seen := map[string]bool{}
	walkMessage(root, func(message protoreflect.Message) {
		relation, ok := message.Interface().(*pg_query.RangeVar)
		if !ok || relation.GetRelname() == "" {
			return
		}
		qualified := relationName(relation)
		if seen[qualified] {
			return
		}
		seen[qualified] = true
		e.b.AddFact(fromID, graph.EdgeReferences, "", qualified, "", e.location(relation.GetLocation(), 0), nil)
	})
}

func (e *extractor) emitAccesses(fromID string, root protoreflect.Message) {
	writes := map[*pg_query.RangeVar]bool{}
	var relations []*pg_query.RangeVar
	walkMessage(root, func(message protoreflect.Message) {
		switch value := message.Interface().(type) {
		case *pg_query.RangeVar:
			relations = append(relations, value)
		case *pg_query.InsertStmt:
			writes[value.GetRelation()] = true
		case *pg_query.UpdateStmt:
			writes[value.GetRelation()] = true
		case *pg_query.DeleteStmt:
			writes[value.GetRelation()] = true
		case *pg_query.MergeStmt:
			writes[value.GetRelation()] = true
		case *pg_query.CopyStmt:
			if value.GetIsFrom() {
				writes[value.GetRelation()] = true
			}
		case *pg_query.TruncateStmt:
			for _, item := range value.GetRelations() {
				writes[item.GetRangeVar()] = true
			}
		case *pg_query.AlterTableStmt:
			writes[value.GetRelation()] = true
		case *pg_query.RenameStmt:
			writes[value.GetRelation()] = true
		}
	})
	seen := map[string]bool{}
	for _, relation := range relations {
		if relation == nil || relation.GetRelname() == "" {
			continue
		}
		kind := graph.EdgeReads
		if writes[relation] {
			kind = graph.EdgeWrites
		}
		qualified := relationName(relation)
		key := string(kind) + "\x00" + qualified
		if seen[key] {
			continue
		}
		seen[key] = true
		e.b.AddFact(fromID, kind, "", qualified, "", e.location(relation.GetLocation(), 0), nil)
	}
}

func (e *extractor) statementOffset(raw *pg_query.RawStmt) int32 {
	offset := raw.GetStmtLocation()
	if offset < 0 {
		offset = 0
	}
	for int(offset) < len(e.content) {
		switch e.content[offset] {
		case ' ', '\t', '\r', '\n', ';':
			offset++
		default:
			return offset
		}
	}
	return raw.GetStmtLocation()
}

func walkMessage(message protoreflect.Message, visit func(protoreflect.Message)) {
	if !message.IsValid() {
		return
	}
	visit(message)
	message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		if field.IsMap() {
			if field.MapValue().Message() != nil {
				value.Map().Range(func(_ protoreflect.MapKey, item protoreflect.Value) bool {
					walkMessage(item.Message(), visit)
					return true
				})
			}
			return true
		}
		if field.IsList() {
			if field.Message() != nil {
				items := value.List()
				for index := 0; index < items.Len(); index++ {
					walkMessage(items.Get(index).Message(), visit)
				}
			}
			return true
		}
		if field.Message() != nil {
			walkMessage(value.Message(), visit)
		}
		return true
	})
}

func (e *extractor) location(offset, fallback int32) graph.Location {
	if offset < 0 {
		offset = fallback
	}
	if offset < 0 {
		offset = 0
	}
	if int(offset) > len(e.content) {
		offset = int32(len(e.content))
	}
	prefix := e.content[:offset]
	line := bytes.Count(prefix, []byte{'\n'}) + 1
	lastNewline := bytes.LastIndexByte(prefix, '\n')
	column := len(prefix) - lastNewline
	return graph.Location{Path: e.input.Path, Line: line, Column: column, EndLine: line}
}

func relationName(relation *pg_query.RangeVar) string {
	if relation == nil {
		return ""
	}
	parts := make([]string, 0, 3)
	for _, part := range []string{relation.GetCatalogname(), relation.GetSchemaname(), relation.GetRelname()} {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, ".")
}

func nodeName(nodes []*pg_query.Node) string {
	parts := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if node != nil && node.GetString_() != nil && node.GetString_().GetSval() != "" {
			parts = append(parts, node.GetString_().GetSval())
		}
	}
	return strings.Join(parts, ".")
}

func functionOptions(statement *pg_query.CreateFunctionStmt, name string) []string {
	var result []string
	for _, item := range statement.GetOptions() {
		option := item.GetDefElem()
		if option == nil || !strings.EqualFold(option.GetDefname(), name) || option.GetArg() == nil {
			continue
		}
		walkMessage(option.GetArg().ProtoReflect(), func(message protoreflect.Message) {
			if value, ok := message.Interface().(*pg_query.String); ok {
				result = append(result, value.GetSval())
			}
		})
	}
	return result
}

func firstFunctionOption(statement *pg_query.CreateFunctionStmt, name string) string {
	values := functionOptions(statement, name)
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func functionSignature(nodes []*pg_query.Node) string {
	parameters := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if parameter := node.GetFunctionParameter(); parameter != nil {
			parameters = append(parameters, typeName(parameter.GetArgType()))
		}
	}
	return "(" + strings.Join(parameters, ", ") + ")"
}

func parameterMode(mode pg_query.FunctionParameterMode) string {
	value := strings.TrimPrefix(mode.String(), "FUNCTION_PARAMETER_MODE_")
	if value == "DEFAULT" || value == "UNDEFINED" {
		return "in"
	}
	return strings.ToLower(value)
}

func typeName(value *pg_query.TypeName) string {
	if value == nil {
		return ""
	}
	name := nodeName(value.GetNames())
	if len(value.GetArrayBounds()) > 0 {
		name += strings.Repeat("[]", len(value.GetArrayBounds()))
	}
	if value.GetSetof() {
		name = "setof " + name
	}
	return name
}

func relationPersistence(value string) string {
	switch value {
	case "p":
		return "permanent"
	case "u":
		return "unlogged"
	case "t":
		return "temporary"
	default:
		return ""
	}
}
