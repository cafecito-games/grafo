package golang

import (
	goast "go/ast"
	"go/constant"
	"go/types"
	"sort"
	"strconv"
	"strings"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/parser/transport"
	"golang.org/x/tools/go/packages"
)

const maxTransportWrapperDepth = 8

var goENetAdapter transport.Adapter = transport.ExactAdapter{
	"github.com/codecat/go-enet.Peer.SendBytes": {
		Protocol: "enet", API: "github.com/codecat/go-enet.Peer.SendBytes", Direction: transport.Send,
		ChannelPosition: 1, PayloadPosition: 0, ReliabilityPosition: 2,
	},
	"github.com/codecat/go-enet.Peer.SendString": {
		Protocol: "enet", API: "github.com/codecat/go-enet.Peer.SendString", Direction: transport.Send,
		ChannelPosition: 1, PayloadPosition: 0, ReliabilityPosition: 2,
	},
	"github.com/codecat/go-enet.Peer.SendPacket": {
		Protocol: "enet", API: "github.com/codecat/go-enet.Peer.SendPacket", Direction: transport.Send,
		ChannelPosition: 1, PayloadPosition: 0, ReliabilityPosition: -1,
	},
	"github.com/codecat/go-enet.Packet.GetData": {
		Protocol: "enet", API: "github.com/codecat/go-enet.Packet.GetData", Direction: transport.Receive,
		ChannelPosition: -1, PayloadPosition: -1, ReliabilityPosition: -1,
	},
}

type transportValue struct {
	parameter int
	constant  string
	status    string
	binding   string
}

type transportTemplate struct {
	spec        transport.Spec
	channel     transportValue
	payload     transportValue
	reliability transportValue
	depth       int
}

type transportCall struct {
	target   string
	call     *goast.CallExpr
	location graph.Location
}

type transportFunction struct {
	name        string
	path        string
	decl        *goast.FuncDecl
	parameters  map[types.Object]int
	bytes       map[types.Object]string
	receives    map[*goast.CallExpr]types.Object
	decode      map[types.Object]string
	direct      []transportTemplate
	summaries   []transportTemplate
	calls       []transportCall
	channel     string
	channelMode string
}

func collectTransportPackageViews(root string, pkg *packages.Package, views map[string]SemanticView) {
	if pkg == nil || pkg.TypesInfo == nil || pkg.Fset == nil {
		return
	}
	functions := map[string]*transportFunction{}
	for _, file := range pkg.Syntax {
		for _, declaration := range file.Decls {
			decl, ok := declaration.(*goast.FuncDecl)
			if !ok || decl.Body == nil {
				continue
			}
			name := objectTarget(pkg.TypesInfo.Defs[decl.Name])
			position := pkg.Fset.Position(decl.Pos())
			path, ok := relativeSourcePath(root, position.Filename)
			if name == "" || !ok {
				continue
			}
			function := &transportFunction{name: name, path: path, decl: decl,
				parameters: map[types.Object]int{}, bytes: map[types.Object]string{},
				receives: map[*goast.CallExpr]types.Object{}, decode: map[types.Object]string{}}
			index := 0
			if decl.Type.Params != nil {
				for _, field := range decl.Type.Params.List {
					for _, parameter := range field.Names {
						if object := pkg.TypesInfo.Defs[parameter]; object != nil {
							function.parameters[object] = index
						}
						index++
					}
				}
			}
			functions[name] = function
		}
	}
	for _, function := range sortedTransportFunctions(functions) {
		collectTransportFunction(pkg, function)
	}

	for iteration := 0; iteration < len(functions)*(maxTransportWrapperDepth+2); iteration++ {
		changed := false
		for _, function := range sortedTransportFunctions(functions) {
			for _, call := range function.calls {
				callee := functions[call.target]
				if callee == nil {
					continue
				}
				for _, summary := range callee.summaries {
					candidate := instantiateTransportTemplate(summary, call.call.Args, function, pkg.TypesInfo)
					candidate.depth = summary.depth + 1
					if candidate.depth > maxTransportWrapperDepth {
						candidate = truncatedTransportTemplate(candidate)
					}
					if addTransportSummary(function, candidate) {
						changed = true
					}
				}
			}
		}
		if !changed {
			break
		}
	}

	for _, function := range sortedTransportFunctions(functions) {
		uses := directTransportUses(pkg, function)
		for _, call := range function.calls {
			callee := functions[call.target]
			if callee == nil {
				continue
			}
			grouped := map[string][]transportTemplate{}
			for _, summary := range callee.summaries {
				instantiated := instantiateTransportTemplate(summary, call.call.Args, function, pkg.TypesInfo)
				instantiated.depth = summary.depth + 1
				if instantiated.depth > maxTransportWrapperDepth {
					instantiated = truncatedTransportTemplate(instantiated)
					view := views[function.path]
					view.Diagnostics = append(view.Diagnostics, graph.Diagnostic{Path: function.path, Line: call.location.Line,
						Level: "warning", Message: "ENet wrapper summary depth exceeded; transport evidence marked truncated"})
					views[function.path] = view
				}
				key := instantiated.spec.API + "\x00" + string(instantiated.spec.Direction)
				grouped[key] = append(grouped[key], instantiated)
			}
			groupKeys := make([]string, 0, len(grouped))
			for key := range grouped {
				groupKeys = append(groupKeys, key)
			}
			sort.Strings(groupKeys)
			for _, key := range groupKeys {
				alternatives := grouped[key]
				merged, conflict := mergeTransportTemplates(alternatives)
				uses = append(uses, semanticTransportUse(function.name, merged, call.location))
				if conflict {
					view := views[function.path]
					view.Diagnostics = append(view.Diagnostics, graph.Diagnostic{Path: function.path, Line: call.location.Line,
						Level: "warning", Message: "conflicting ENet wrapper summaries; transport evidence marked ambiguous"})
					views[function.path] = view
				}
			}
		}
		view := views[function.path]
		view.TransportUses = append(view.TransportUses, uses...)
		sort.Slice(view.TransportUses, func(i, j int) bool {
			left, right := view.TransportUses[i], view.TransportUses[j]
			if left.Location.Line != right.Location.Line {
				return left.Location.Line < right.Location.Line
			}
			if left.Location.Column != right.Location.Column {
				return left.Location.Column < right.Location.Column
			}
			if left.Spec.API != right.Spec.API {
				return left.Spec.API < right.Spec.API
			}
			return left.WrapperDepth < right.WrapperDepth
		})
		views[function.path] = view
	}
}

func truncatedTransportTemplate(template transportTemplate) transportTemplate {
	template.channel = transportValue{parameter: -1, status: "truncated"}
	template.payload = transportValue{parameter: -1, status: "truncated"}
	template.reliability = transportValue{parameter: -1, status: "truncated"}
	template.depth = maxTransportWrapperDepth
	return template
}

func mergeTransportTemplates(alternatives []transportTemplate) (transportTemplate, bool) {
	merged := alternatives[0]
	conflict := false
	for _, alternative := range alternatives[1:] {
		merged.channel, conflict = mergeTransportValue(merged.channel, alternative.channel, conflict)
		merged.payload, conflict = mergeTransportValue(merged.payload, alternative.payload, conflict)
		merged.reliability, conflict = mergeTransportValue(merged.reliability, alternative.reliability, conflict)
		if alternative.depth > merged.depth {
			merged.depth = alternative.depth
		}
	}
	return merged, conflict
}

func mergeTransportValue(left, right transportValue, conflict bool) (transportValue, bool) {
	if left == right {
		return left, conflict
	}
	return transportValue{parameter: -1, status: "ambiguous"}, true
}

func collectTransportFunction(pkg *packages.Package, function *transportFunction) {
	info := pkg.TypesInfo
	var assignments []*goast.AssignStmt
	var valueSpecs []*goast.ValueSpec
	var calls []*goast.CallExpr
	goast.Inspect(function.decl.Body, func(node goast.Node) bool {
		switch value := node.(type) {
		case *goast.AssignStmt:
			assignments = append(assignments, value)
		case *goast.ValueSpec:
			valueSpecs = append(valueSpecs, value)
		case *goast.CallExpr:
			calls = append(calls, value)
		}
		return true
	})
	assignmentCounts := map[types.Object]int{}
	for _, assignment := range assignments {
		for _, lhs := range assignment.Lhs {
			if object := expressionObject(lhs, info); object != nil {
				assignmentCounts[object]++
			}
		}
	}
	for _, spec := range valueSpecs {
		for _, name := range spec.Names {
			if object := info.Defs[name]; object != nil {
				assignmentCounts[object]++
			}
		}
	}
	for iteration := 0; iteration < maxTransportWrapperDepth; iteration++ {
		changed := false
		for _, assignment := range assignments {
			for index, lhs := range assignment.Lhs {
				if index >= len(assignment.Rhs) {
					break
				}
				identifier, ok := lhs.(*goast.Ident)
				if !ok {
					continue
				}
				object := info.Defs[identifier]
				if object == nil {
					object = info.Uses[identifier]
				}
				if assignmentCounts[object] != 1 {
					continue
				}
				binding := protobufBytesBinding(assignment.Rhs[index], function.bytes, info)
				if object != nil && binding != "" && function.bytes[object] != binding {
					function.bytes[object] = binding
					changed = true
				}
			}
			if len(assignment.Rhs) == 1 {
				call, ok := assignment.Rhs[0].(*goast.CallExpr)
				if ok && callableTarget(call.Fun, info, nil) == "google.golang.org/protobuf/proto.Marshal" && len(assignment.Lhs) > 0 {
					if identifier, ok := assignment.Lhs[0].(*goast.Ident); ok {
						object := info.Defs[identifier]
						if object == nil {
							object = info.Uses[identifier]
						}
						binding := semanticNamedType(info.TypeOf(call.Args[0]))
						if object != nil && assignmentCounts[object] == 1 && binding != "" && function.bytes[object] != binding {
							function.bytes[object] = binding
							changed = true
						}
					}
				}
			}
		}
		for _, spec := range valueSpecs {
			for index, name := range spec.Names {
				if index >= len(spec.Values) {
					break
				}
				binding := protobufBytesBinding(spec.Values[index], function.bytes, info)
				if object := info.Defs[name]; object != nil && assignmentCounts[object] == 1 && binding != "" && function.bytes[object] != binding {
					function.bytes[object] = binding
					changed = true
				}
			}
		}
		if !changed {
			break
		}
	}

	channelCandidates := map[string]bool{}
	for _, call := range calls {
		target := callableTarget(call.Fun, info, nil)
		if target == "github.com/codecat/go-enet.Event.GetChannelID" {
			channelCandidates[render(pkg.Fset, call)] = true
		}
		if target == "google.golang.org/protobuf/proto.Unmarshal" && len(call.Args) > 1 {
			if object := expressionObject(call.Args[0], info); object != nil {
				function.decode[object] = semanticNamedType(info.TypeOf(call.Args[1]))
			}
		}
	}
	if len(channelCandidates) == 1 {
		for channel := range channelCandidates {
			function.channel, function.channelMode = channel, "symbolic"
		}
	} else if len(channelCandidates) > 1 {
		function.channelMode = "ambiguous"
	}

	for _, assignment := range assignments {
		if len(assignment.Lhs) != 1 || len(assignment.Rhs) != 1 {
			continue
		}
		call, ok := assignment.Rhs[0].(*goast.CallExpr)
		if !ok || callableTarget(call.Fun, info, nil) != "github.com/codecat/go-enet.Packet.GetData" {
			continue
		}
		if identifier, ok := assignment.Lhs[0].(*goast.Ident); ok {
			object := info.Defs[identifier]
			if object == nil {
				object = info.Uses[identifier]
			}
			if assignmentCounts[object] == 1 {
				function.receives[call] = object
			}
		}
	}

	for _, call := range calls {
		target := callableTarget(call.Fun, info, nil)
		if spec, ok := goENetAdapter.Match(target); ok {
			template := transportTemplate{spec: spec, depth: 0,
				channel: transportValue{parameter: -1}, payload: transportValue{parameter: -1}, reliability: transportValue{parameter: -1}}
			if spec.ChannelPosition >= 0 && spec.ChannelPosition < len(call.Args) {
				template.channel = transportValueForExpression(call.Args[spec.ChannelPosition], function, info)
			} else if spec.Direction == transport.Receive {
				template.channel = transportValue{parameter: -1, constant: function.channel, status: function.channelMode}
			}
			if spec.PayloadPosition >= 0 && spec.PayloadPosition < len(call.Args) {
				template.payload = transportValueForExpression(call.Args[spec.PayloadPosition], function, info)
			} else if object := function.receives[call]; object != nil {
				template.payload.binding = function.decode[object]
				if template.payload.binding != "" {
					template.payload.status = "proven"
				}
			}
			if spec.ReliabilityPosition >= 0 && spec.ReliabilityPosition < len(call.Args) {
				template.reliability = transportValueForExpression(call.Args[spec.ReliabilityPosition], function, info)
			}
			function.direct = append(function.direct, template)
			addTransportSummary(function, template)
		}
		if strings.HasPrefix(target, pkg.PkgPath+".") {
			function.calls = append(function.calls, transportCall{target: target, call: call,
				location: semanticLocation(function.path, pkg.Fset, call.Pos(), call.End())})
		}
	}
}

func directTransportUses(pkg *packages.Package, function *transportFunction) []SemanticTransportUse {
	var result []SemanticTransportUse
	directIndex := 0
	goast.Inspect(function.decl.Body, func(node goast.Node) bool {
		call, ok := node.(*goast.CallExpr)
		if !ok {
			return true
		}
		if _, matched := goENetAdapter.Match(callableTarget(call.Fun, pkg.TypesInfo, nil)); !matched {
			return true
		}
		if directIndex < len(function.direct) {
			result = append(result, semanticTransportUse(function.name, function.direct[directIndex],
				semanticLocation(function.path, pkg.Fset, call.Pos(), call.End())))
			directIndex++
		}
		return true
	})
	return result
}

func semanticTransportUse(function string, template transportTemplate, location graph.Location) SemanticTransportUse {
	use := SemanticTransportUse{Function: function, Spec: template.spec, Channel: template.channel.constant,
		ChannelStatus: template.channel.status, PayloadBinding: template.payload.binding,
		PayloadStatus: template.payload.status, WrapperDepth: template.depth, Location: location}
	use.Reliability = transportReliability(template.reliability)
	if use.PayloadBinding != "" {
		use.PayloadStatus = "proven"
	}
	return use
}

func transportValueForExpression(expression goast.Expr, function *transportFunction, info *types.Info) transportValue {
	value := transportValue{parameter: -1}
	if object := expressionObject(expression, info); object != nil {
		if parameter, ok := function.parameters[object]; ok {
			value.parameter = parameter
		}
		if binding := function.bytes[object]; binding != "" {
			value.binding, value.status = binding, "proven"
		}
	}
	if binding := protobufBytesBinding(expression, function.bytes, info); binding != "" {
		value.binding, value.status = binding, "proven"
	}
	if exact := info.Types[expression].Value; exact != nil {
		value.constant, value.status = constantValue(exact), "proven"
	}
	return value
}

func protobufBytesBinding(expression goast.Expr, bindings map[types.Object]string, info *types.Info) string {
	if object := expressionObject(expression, info); object != nil {
		return bindings[object]
	}
	call, ok := expression.(*goast.CallExpr)
	if ok && callableTarget(call.Fun, info, nil) == "google.golang.org/protobuf/proto.Marshal" && len(call.Args) > 0 {
		return semanticNamedType(info.TypeOf(call.Args[0]))
	}
	return ""
}

func expressionObject(expression goast.Expr, info *types.Info) types.Object {
	identifier, ok := expression.(*goast.Ident)
	if !ok {
		return nil
	}
	object := info.Uses[identifier]
	if object == nil {
		object = info.Defs[identifier]
	}
	return object
}

func instantiateTransportTemplate(template transportTemplate, arguments []goast.Expr, caller *transportFunction, info *types.Info) transportTemplate {
	result := template
	result.channel = instantiateTransportValue(template.channel, arguments, caller, info)
	result.payload = instantiateTransportValue(template.payload, arguments, caller, info)
	result.reliability = instantiateTransportValue(template.reliability, arguments, caller, info)
	return result
}

func instantiateTransportValue(value transportValue, arguments []goast.Expr, caller *transportFunction, info *types.Info) transportValue {
	if value.parameter < 0 || value.parameter >= len(arguments) {
		return value
	}
	return transportValueForExpression(arguments[value.parameter], caller, info)
}

func addTransportSummary(function *transportFunction, summary transportTemplate) bool {
	key := transportTemplateKey(summary)
	for index, existing := range function.summaries {
		if transportTemplateKey(existing) == key {
			if summary.depth < existing.depth {
				function.summaries[index] = summary
				return true
			}
			return false
		}
	}
	function.summaries = append(function.summaries, summary)
	return true
}

func transportTemplateKey(summary transportTemplate) string {
	values := []transportValue{summary.channel, summary.payload, summary.reliability}
	parts := []string{summary.spec.API, string(summary.spec.Direction)}
	for _, value := range values {
		parts = append(parts, strconv.Itoa(value.parameter), value.constant, value.status, value.binding)
	}
	return strings.Join(parts, "\x00")
}

func transportReliability(value transportValue) string {
	if value.status != "proven" || value.constant == "" {
		return "unknown"
	}
	flags, err := strconv.ParseInt(value.constant, 10, 64)
	if err != nil {
		return "unknown"
	}
	if flags&1 != 0 {
		return "reliable"
	}
	return "unreliable"
}

func constantValue(value constant.Value) string {
	if integer := constant.ToInt(value); integer.Kind() == constant.Int {
		return integer.ExactString()
	}
	return value.ExactString()
}

func sortedTransportFunctions(functions map[string]*transportFunction) []*transportFunction {
	names := make([]string, 0, len(functions))
	for name := range functions {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]*transportFunction, 0, len(names))
	for _, name := range names {
		result = append(result, functions[name])
	}
	return result
}
