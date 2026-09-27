// Package protobuf adapts protobuf source files to Grafo's shared graph model.
package protobuf

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/bufbuild/protocompile/ast"
	protoparser "github.com/bufbuild/protocompile/parser"
	"github.com/bufbuild/protocompile/reporter"
	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	"google.golang.org/protobuf/types/descriptorpb"
)

type Parser struct{}

func New() *Parser               { return &Parser{} }
func (*Parser) Language() string { return "protobuf" }

func (*Parser) Supports(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".proto")
}

func (*Parser) Parse(ctx context.Context, input parserapi.Input) (graph.ParseResult, error) {
	b := parserapi.NewBuilder(input, "protobuf")
	if err := ctx.Err(); err != nil {
		return b.Finish(), err
	}

	diagnostics := newDiagnostics(input.Path)
	handler := diagnostics.handler()
	file, parseErr := protoparser.Parse(input.Path, bytes.NewReader(input.Content), handler)
	if parseErr != nil && !errors.Is(parseErr, reporter.ErrInvalidSource) {
		return b.Finish(), fmt.Errorf("parse protobuf source: %w", parseErr)
	}
	if file == nil {
		return b.Finish(), errors.New("parse protobuf source: parser returned no syntax tree")
	}
	if err := ctx.Err(); err != nil {
		return b.Finish(), err
	}

	// Converting the recoverable AST to an unlinked descriptor gives us a
	// stable, typed view without requiring imports to exist on disk. Linking is
	// deliberately left to Grafo's evidence-based reconciliation step.
	parsed, descriptorErr := protoparser.ResultFromAST(file, false, diagnostics.handler())
	if descriptorErr != nil && !errors.Is(descriptorErr, reporter.ErrInvalidSource) {
		return b.Finish(), fmt.Errorf("build protobuf descriptor: %w", descriptorErr)
	}

	b.Result.Diagnostics = append(b.Result.Diagnostics, diagnostics.items...)
	if parsed == nil || parsed.FileDescriptorProto() == nil {
		return b.Finish(), nil
	}
	extractor := extractor{b: b, input: input, file: file, parsed: parsed}
	extractor.extract()
	return b.Finish(), nil
}

type diagnosticCollector struct {
	path  string
	seen  map[string]bool
	items []graph.Diagnostic
}

func newDiagnostics(path string) *diagnosticCollector {
	return &diagnosticCollector{path: path, seen: map[string]bool{}}
}

func (d *diagnosticCollector) handler() *reporter.Handler {
	return reporter.NewHandler(reporter.NewReporter(
		func(err reporter.ErrorWithPos) error {
			d.add("warning", err)
			return nil // Continue so protocompile can return its recoverable AST.
		},
		func(err reporter.ErrorWithPos) { d.add("warning", err) },
	))
}

func (d *diagnosticCollector) add(level string, err reporter.ErrorWithPos) {
	message := err.Error()
	if cause := err.Unwrap(); cause != nil {
		message = cause.Error()
	}
	line := err.GetPosition().Line
	key := level + "\x00" + strconv.Itoa(line) + "\x00" + message
	if d.seen[key] {
		return
	}
	d.seen[key] = true
	d.items = append(d.items, graph.Diagnostic{Path: d.path, Line: line, Level: level, Message: message})
}

type extractor struct {
	b      *parserapi.Builder
	input  parserapi.Input
	file   *ast.FileNode
	parsed protoparser.Result
	pkg    string
}

func (e *extractor) extract() {
	descriptor := e.parsed.FileDescriptorProto()
	e.pkg = descriptor.GetPackage()
	properties := map[string]string{}
	if e.pkg != "" {
		properties["package"] = e.pkg
	}
	if e.file.Edition != nil {
		properties["edition"] = e.file.Edition.Edition.AsString()
	} else if e.file.Syntax != nil {
		properties["syntax"] = e.file.Syntax.Syntax.AsString()
	} else {
		properties["syntax"] = "proto2"
	}
	e.b.Result.Nodes[0].Properties = properties

	e.extractImports()
	for _, message := range descriptor.GetMessageType() {
		e.extractMessage(message, e.b.FileID(), e.pkg)
	}
	for _, enum := range descriptor.GetEnumType() {
		e.extractEnum(enum, e.b.FileID(), e.pkg)
	}
	for _, service := range descriptor.GetService() {
		e.extractService(service, e.b.FileID())
	}
	for _, extension := range descriptor.GetExtension() {
		e.extractExtension(extension, e.b.FileID(), e.pkg)
	}
}

func (e *extractor) extractImports() {
	for _, declaration := range e.file.Decls {
		importNode, ok := declaration.(*ast.ImportNode)
		if !ok {
			continue
		}
		properties := map[string]string{}
		if importNode.Public != nil {
			properties["visibility"] = "public"
		} else if importNode.Weak != nil {
			properties["visibility"] = "weak"
		}
		e.b.AddFact(e.b.FileID(), graph.EdgeImports, "", importNode.Name.AsString(), graph.KindFile,
			e.location(importNode), properties)
	}
}

func (e *extractor) extractMessage(message *descriptorpb.DescriptorProto, parentID, container string) {
	if message.GetOptions().GetMapEntry() {
		return
	}
	qualified := qualify(container, message.GetName())
	messageID := e.b.Declare(parentID, graph.Node{
		Kind: graph.KindType, Name: message.GetName(), QualifiedName: qualified,
		Location:   e.location(e.parsed.MessageNode(message)),
		Properties: map[string]string{"declaration": "message"},
	})
	for _, field := range message.GetField() {
		e.extractField(field, message, messageID, qualified)
	}
	for _, nested := range message.GetNestedType() {
		e.extractMessage(nested, messageID, qualified)
	}
	for _, enum := range message.GetEnumType() {
		e.extractEnum(enum, messageID, qualified)
	}
	for _, extension := range message.GetExtension() {
		e.extractExtension(extension, messageID, qualified)
	}
}

func (e *extractor) extractField(field *descriptorpb.FieldDescriptorProto, owner *descriptorpb.DescriptorProto, ownerID, container string) {
	node := e.parsed.FieldNode(field)
	loc := e.location(node)
	properties := map[string]string{
		"number":      strconv.FormatInt(int64(field.GetNumber()), 10),
		"cardinality": fieldCardinality(field),
		"type":        fieldType(field),
	}
	if field.GetJsonName() != "" && field.GetJsonName() != field.GetName() {
		properties["json_name"] = field.GetJsonName()
	}
	if mapNode, ok := node.(*ast.MapFieldNode); ok {
		keyType := mapNode.MapType.KeyType.Val
		valueType := normalizeTypeName(string(mapNode.MapType.ValueType.AsIdentifier()))
		properties["cardinality"] = "map"
		properties["key_type"] = keyType
		properties["value_type"] = valueType
		properties["type"] = "map<" + keyType + ", " + valueType + ">"
	}
	if field.OneofIndex != nil && !field.GetProto3Optional() {
		index := int(field.GetOneofIndex())
		if index >= 0 && index < len(owner.GetOneofDecl()) {
			properties["oneof"] = owner.GetOneofDecl()[index].GetName()
		}
	}
	fieldID := e.b.AddNode(graph.Node{
		Kind: graph.KindField, Name: field.GetName(), QualifiedName: qualify(container, field.GetName()),
		Location: loc, Properties: properties,
	})
	e.b.AddFact(ownerID, graph.EdgeHasField, fieldID, "", graph.KindField, loc, nil)

	if mapNode, ok := node.(*ast.MapFieldNode); ok {
		valueType := normalizeTypeName(string(mapNode.MapType.ValueType.AsIdentifier()))
		if !isScalar(valueType) {
			e.addTypeReference(fieldID, valueType, e.location(mapNode.MapType.ValueType), "map_value", nil)
		}
		return
	}
	if target := field.GetTypeName(); target != "" {
		e.addTypeReference(fieldID, target, e.location(node.FieldType()), "field_type", nil)
	}
}

func (e *extractor) extractEnum(enum *descriptorpb.EnumDescriptorProto, parentID, container string) {
	qualified := qualify(container, enum.GetName())
	loc := e.location(e.parsed.EnumNode(enum))
	enumID := e.b.Declare(parentID, graph.Node{
		Kind: graph.KindType, Name: enum.GetName(), QualifiedName: qualified, Location: loc,
		Properties: map[string]string{"declaration": "enum"},
	})
	for _, value := range enum.GetValue() {
		valueLoc := e.location(e.parsed.EnumValueNode(value))
		valueID := e.b.AddNode(graph.Node{
			Kind: graph.KindField, Name: value.GetName(), QualifiedName: qualify(qualified, value.GetName()),
			Location: valueLoc, Properties: map[string]string{
				"declaration": "enum_value", "number": strconv.FormatInt(int64(value.GetNumber()), 10),
			},
		})
		e.b.AddFact(enumID, graph.EdgeHasField, valueID, "", graph.KindField, valueLoc, nil)
	}
}

func (e *extractor) extractService(service *descriptorpb.ServiceDescriptorProto, parentID string) {
	qualified := qualify(e.pkg, service.GetName())
	serviceID := e.b.Declare(parentID, graph.Node{
		Kind: graph.KindInterface, Name: service.GetName(), QualifiedName: qualified,
		Location:   e.location(e.parsed.ServiceNode(service)),
		Properties: map[string]string{"declaration": "service"},
	})
	for _, method := range service.GetMethod() {
		rpcNode := e.parsed.MethodNode(method)
		loc := e.location(rpcNode)
		properties := map[string]string{
			"input": strings.TrimPrefix(method.GetInputType(), "."), "output": strings.TrimPrefix(method.GetOutputType(), "."),
		}
		if method.GetClientStreaming() {
			properties["client_streaming"] = "true"
		}
		if method.GetServerStreaming() {
			properties["server_streaming"] = "true"
		}
		methodID := e.b.Declare(serviceID, graph.Node{
			Kind: graph.KindMethod, Name: method.GetName(), QualifiedName: qualify(qualified, method.GetName()),
			Location: loc, Properties: properties,
		})
		e.addTypeReference(methodID, method.GetInputType(), e.location(rpcNode.GetInputType()), "input", streamProperties(method.GetClientStreaming()))
		e.addTypeReference(methodID, method.GetOutputType(), e.location(rpcNode.GetOutputType()), "output", streamProperties(method.GetServerStreaming()))
	}
}

func (e *extractor) extractExtension(field *descriptorpb.FieldDescriptorProto, parentID, container string) {
	node := e.parsed.FieldNode(field)
	loc := e.location(node)
	properties := map[string]string{
		"declaration": "extension", "number": strconv.FormatInt(int64(field.GetNumber()), 10),
		"cardinality": fieldCardinality(field), "type": fieldType(field),
	}
	fieldID := e.b.Declare(parentID, graph.Node{
		Kind: graph.KindField, Name: field.GetName(), QualifiedName: qualify(container, field.GetName()),
		Location: loc, Properties: properties,
	})
	if target := field.GetTypeName(); target != "" {
		e.addTypeReference(fieldID, target, e.location(node.FieldType()), "field_type", nil)
	}
	if extendee := field.GetExtendee(); extendee != "" {
		e.addTypeReference(fieldID, extendee, e.location(node.FieldExtendee()), "extendee", nil)
	}
}

func (e *extractor) addTypeReference(fromID, target string, loc graph.Location, role string, properties map[string]string) {
	target = normalizeTypeName(target)
	if target == "" || isScalar(target) {
		return
	}
	if properties == nil {
		properties = map[string]string{}
	}
	properties["role"] = role
	e.b.AddFact(fromID, graph.EdgeReferences, "", target, graph.KindType, loc, properties)
}

func (e *extractor) location(node ast.Node) graph.Location {
	if node == nil {
		return graph.Location{Path: e.input.Path, Line: 1, Column: 1, EndLine: 1}
	}
	info := e.file.NodeInfo(node)
	if !info.IsValid() {
		return graph.Location{Path: e.input.Path, Line: 1, Column: 1, EndLine: 1}
	}
	start, end := info.Start(), info.End()
	return graph.Location{Path: e.input.Path, Line: start.Line, Column: start.Col, EndLine: end.Line}
}

func qualify(container, name string) string {
	if container == "" {
		return name
	}
	if name == "" {
		return container
	}
	return container + "." + name
}

func fieldCardinality(field *descriptorpb.FieldDescriptorProto) string {
	switch field.GetLabel() {
	case descriptorpb.FieldDescriptorProto_LABEL_REQUIRED:
		return "required"
	case descriptorpb.FieldDescriptorProto_LABEL_REPEATED:
		return "repeated"
	default:
		return "optional"
	}
}

func fieldType(field *descriptorpb.FieldDescriptorProto) string {
	if field.GetTypeName() != "" {
		return normalizeTypeName(field.GetTypeName())
	}
	return strings.ToLower(strings.TrimPrefix(field.GetType().String(), "TYPE_"))
}

func normalizeTypeName(name string) string {
	return strings.TrimPrefix(strings.TrimSpace(name), ".")
}

func streamProperties(streaming bool) map[string]string {
	if !streaming {
		return nil
	}
	return map[string]string{"streaming": "true"}
}

func isScalar(name string) bool {
	switch strings.TrimPrefix(name, ".") {
	case "double", "float", "int32", "int64", "uint32", "uint64", "sint32", "sint64",
		"fixed32", "fixed64", "sfixed32", "sfixed64", "bool", "string", "bytes":
		return true
	default:
		return false
	}
}
