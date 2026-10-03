package golang

import (
	goast "go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"sort"
	"strconv"
	"strings"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/parser/transport"
	"golang.org/x/tools/go/packages"
)

const maxTransportWrapperDepth = 8

// goENetPackage is the import path of the ENet binding this parser understands.
const goENetPackage = "github.com/cafecito-games/goenet/pkg"

const (
	goENetEventType = goENetPackage + ".Event"
	// goENetReceiveAPI names the Event payload field read that anchors receive
	// evidence. goenet delivers packets as struct fields rather than through a
	// getter, so there is no receive call to match on.
	goENetReceiveAPI = goENetPackage + ".Event.Packet.Data"
)

// goENetAdapter matches goenet's send APIs. All of them carry the channel
// positionally and keep the payload and reliability inside the *Packet argument.
//
// PeerSender.Send is listed alongside the concrete Peer.Send because goenet
// exports PeerSender specifically so consumers can embed it when narrowing a
// peer behind a local interface. Embedding preserves the package but changes the
// receiver, so go/types reports the call against PeerSender and the concrete
// identity alone would miss every such site. An interface that redeclares Send
// instead of embedding PeerSender gets a local identity and is deliberately not
// matched: it is indistinguishable from an unrelated same-signature method.
var goENetAdapter transport.Adapter = transport.ExactAdapter{
	goENetPackage + ".Peer.Send": {
		Protocol: "enet", API: goENetPackage + ".Peer.Send", Direction: transport.Send,
		ChannelPosition: 0, PayloadPosition: 1, PayloadField: "Data",
		ReliabilityPosition: 1, ReliabilityField: "Flags",
	},
	goENetPackage + ".PeerSender.Send": {
		Protocol: "enet", API: goENetPackage + ".PeerSender.Send", Direction: transport.Send,
		ChannelPosition: 0, PayloadPosition: 1, PayloadField: "Data",
		ReliabilityPosition: 1, ReliabilityField: "Flags",
	},
	goENetPackage + ".Host.Broadcast": {
		Protocol: "enet", API: goENetPackage + ".Host.Broadcast", Direction: transport.Send,
		ChannelPosition: 0, PayloadPosition: 1, PayloadField: "Data",
		ReliabilityPosition: 1, ReliabilityField: "Flags",
	},
}

// goENetReceiveSpec describes receive evidence anchored on an Event payload
// field read. Neither value is an argument: the channel comes from the
// function-level symbolic scan of Event.ChannelID reads and the payload from
// proto.Unmarshal linkage on the bytes the read is bound to.
var goENetReceiveSpec = transport.Spec{
	Protocol: "enet", API: goENetReceiveAPI, Direction: transport.Receive,
	ChannelPosition: transport.NoPosition, PayloadPosition: transport.NoPosition,
	ReliabilityPosition: transport.NoPosition,
	ChannelField:        "ChannelID", PayloadField: "Packet.Data",
}

type transportValue struct {
	parameter int
	// field is a dotted path resolved through the value at parameter, kept so
	// that wrapper instantiation can re-resolve it against the caller argument.
	field    string
	constant string
	status   string
	binding  string
}

type transportTemplate struct {
	spec        transport.Spec
	channel     transportValue
	payload     transportValue
	reliability transportValue
	depth       int
	// location anchors a directly observed operation. It is unset on summaries,
	// which are re-anchored at each call site that instantiates them.
	location graph.Location
}

type transportCall struct {
	target   string
	call     *goast.CallExpr
	location graph.Location
}

type transportFunction struct {
	name       string
	path       string
	decl       *goast.FuncDecl
	parameters map[types.Object]int
	bytes      map[types.Object]string
	// packets maps a variable assigned exactly once to a composite literal onto
	// that literal, so a packet built on one line and sent on the next resolves.
	packets     map[types.Object]*goast.CompositeLit
	receives    map[goast.Expr]types.Object
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
				packets:  map[types.Object]*goast.CompositeLit{},
				receives: map[goast.Expr]types.Object{}, decode: map[types.Object]string{}}
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
		uses := directTransportUses(function)
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
				use := semanticTransportUse(function.name, merged, call.location)
				if conflict {
					use.PayloadAlternatives = distinctPayloadBindings(alternatives)
				}
				uses = append(uses, use)
				if conflict {
					view := views[function.path]
					view.Diagnostics = append(view.Diagnostics, graph.Diagnostic{Path: function.path, Line: call.location.Line,
						Level: "warning", Message: "conflicting ENet wrapper summaries for " + merged.spec.API + "; transport evidence marked ambiguous"})
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
	template.location = graph.Location{}
	template.depth = maxTransportWrapperDepth
	return template
}

// distinctPayloadBindings reports every payload binding the alternatives
// resolved, deduplicated and ordered so the evidence is deterministic.
func distinctPayloadBindings(alternatives []transportTemplate) []string {
	seen := map[string]bool{}
	var bindings []string
	for _, alternative := range alternatives {
		binding := alternative.payload.binding
		if binding == "" || seen[binding] {
			continue
		}
		seen[binding] = true
		bindings = append(bindings, binding)
	}
	sort.Strings(bindings)
	return bindings
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
	var selectors []*goast.SelectorExpr
	goast.Inspect(function.decl.Body, func(node goast.Node) bool {
		switch value := node.(type) {
		case *goast.AssignStmt:
			assignments = append(assignments, value)
		case *goast.ValueSpec:
			valueSpecs = append(valueSpecs, value)
		case *goast.CallExpr:
			calls = append(calls, value)
		case *goast.SelectorExpr:
			selectors = append(selectors, value)
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
	for _, selector := range selectors {
		if goENetEventChannel(selector, info) {
			channelCandidates[render(pkg.Fset, selector)] = true
		}
	}
	for _, call := range calls {
		if callableTarget(call.Fun, info, nil) == "google.golang.org/protobuf/proto.Unmarshal" && len(call.Args) > 1 {
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
		identifier, ok := assignment.Lhs[0].(*goast.Ident)
		if !ok {
			continue
		}
		object := info.Defs[identifier]
		if object == nil {
			object = info.Uses[identifier]
		}
		if object == nil || assignmentCounts[object] != 1 {
			continue
		}
		if selector, ok := assignment.Rhs[0].(*goast.SelectorExpr); ok && goENetEventPayload(selector, info) {
			function.receives[selector] = object
		}
		if literal := compositeLiteral(assignment.Rhs[0]); literal != nil {
			function.packets[object] = literal
		}
	}
	for _, spec := range valueSpecs {
		for index, name := range spec.Names {
			if index >= len(spec.Values) {
				break
			}
			object := info.Defs[name]
			if object == nil || assignmentCounts[object] != 1 {
				continue
			}
			if literal := compositeLiteral(spec.Values[index]); literal != nil {
				function.packets[object] = literal
			}
		}
	}

	for _, selector := range selectors {
		if !goENetEventPayload(selector, info) {
			continue
		}
		template := transportTemplate{spec: goENetReceiveSpec, depth: 0,
			channel: transportValue{parameter: -1, constant: function.channel, status: function.channelMode},
			payload: transportValue{parameter: -1}, reliability: transportValue{parameter: -1},
			location: semanticLocation(function.path, pkg.Fset, selector.Pos(), selector.End())}
		if object := function.receives[selector]; object != nil {
			template.payload.binding = function.decode[object]
			if template.payload.binding != "" {
				template.payload.status = "proven"
			}
		}
		function.direct = append(function.direct, template)
		addTransportSummary(function, template)
	}

	for _, call := range calls {
		target := callableTarget(call.Fun, info, nil)
		if spec, ok := goENetAdapter.Match(target); ok {
			template := transportTemplate{spec: spec, depth: 0,
				channel: transportValue{parameter: -1}, payload: transportValue{parameter: -1},
				reliability: transportValue{parameter: -1},
				location:    semanticLocation(function.path, pkg.Fset, call.Pos(), call.End())}
			template.channel = transportArgumentValue(call.Args, spec.ChannelPosition, spec.ChannelField, function, info)
			template.payload = transportArgumentValue(call.Args, spec.PayloadPosition, spec.PayloadField, function, info)
			template.reliability = transportArgumentValue(call.Args, spec.ReliabilityPosition, spec.ReliabilityField, function, info)
			function.direct = append(function.direct, template)
			addTransportSummary(function, template)
		}
		if strings.HasPrefix(target, pkg.PkgPath+".") {
			function.calls = append(function.calls, transportCall{target: target, call: call,
				location: semanticLocation(function.path, pkg.Fset, call.Pos(), call.End())})
		}
	}
}

// transportArgumentValue resolves the value a spec places at position, optionally
// reached through a dotted field path of the argument found there.
func transportArgumentValue(arguments []goast.Expr, position int, field string,
	function *transportFunction, info *types.Info) transportValue {
	if position < 0 || position >= len(arguments) {
		return transportValue{parameter: -1}
	}
	return transportValueForFieldPath(arguments[position], field, function, info)
}

// goENetEventPayload reports whether selector reads the payload bytes of a
// goenet Event, that is <event>.Packet.Data.
func goENetEventPayload(selector *goast.SelectorExpr, info *types.Info) bool {
	if selector.Sel.Name != "Data" {
		return false
	}
	inner, ok := unwrapTransportExpression(selector.X).(*goast.SelectorExpr)
	return ok && inner.Sel.Name == "Packet" && isGoENetEvent(inner.X, info)
}

// goENetEventChannel reports whether selector reads <event>.ChannelID.
func goENetEventChannel(selector *goast.SelectorExpr, info *types.Info) bool {
	return selector.Sel.Name == "ChannelID" && isGoENetEvent(selector.X, info)
}

func isGoENetEvent(expression goast.Expr, info *types.Info) bool {
	return semanticNamedType(info.TypeOf(expression)) == goENetEventType
}

// transportValueForFieldPath resolves a value held in a struct field of
// expression, as goenet's payload and reliability are held in its *Packet
// argument. An empty field resolves expression itself. When expression is a
// parameter the field path is carried on the value so that wrapper
// instantiation can resolve it against the caller's argument instead.
func transportValueForFieldPath(expression goast.Expr, field string,
	function *transportFunction, info *types.Info) transportValue {
	if field == "" {
		return transportValueForExpression(expression, function, info)
	}
	name, rest := splitFieldPath(field)
	switch value := unwrapTransportExpression(expression).(type) {
	case *goast.CompositeLit:
		return transportValueForLiteralField(value, name, rest, function, info)
	case *goast.Ident:
		object := expressionObject(value, info)
		if object == nil {
			break
		}
		if literal := function.packets[object]; literal != nil {
			return transportValueForLiteralField(literal, name, rest, function, info)
		}
		if parameter, ok := function.parameters[object]; ok {
			return transportValue{parameter: parameter, field: field}
		}
	}
	return transportValue{parameter: -1}
}

func transportValueForLiteralField(literal *goast.CompositeLit, name, rest string,
	function *transportFunction, info *types.Info) transportValue {
	element := compositeLiteralField(literal, name)
	if element == nil {
		return transportValue{parameter: -1}
	}
	return transportValueForFieldPath(element, rest, function, info)
}

// compositeLiteralField returns the value keyed by name, or nil when the literal
// omits it or is written positionally.
func compositeLiteralField(literal *goast.CompositeLit, name string) goast.Expr {
	for _, element := range literal.Elts {
		pair, ok := element.(*goast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := pair.Key.(*goast.Ident); ok && key.Name == name {
			return pair.Value
		}
	}
	return nil
}

func compositeLiteral(expression goast.Expr) *goast.CompositeLit {
	literal, _ := unwrapTransportExpression(expression).(*goast.CompositeLit)
	return literal
}

// unwrapTransportExpression strips address-of and parentheses so that a packet
// passed as &Packet{...} reads the same as one passed by value.
func unwrapTransportExpression(expression goast.Expr) goast.Expr {
	for {
		switch value := expression.(type) {
		case *goast.ParenExpr:
			expression = value.X
		case *goast.UnaryExpr:
			if value.Op != token.AND {
				return expression
			}
			expression = value.X
		default:
			return expression
		}
	}
}

func splitFieldPath(field string) (string, string) {
	if head, rest, found := strings.Cut(field, "."); found {
		return head, rest
	}
	return field, ""
}

// directTransportUses emits the operations observed directly in the function
// body. Each template carries the location of the call or field read that
// anchors it, because goenet receive evidence is a field read rather than a call.
func directTransportUses(function *transportFunction) []SemanticTransportUse {
	result := make([]SemanticTransportUse, 0, len(function.direct))
	for _, template := range function.direct {
		result = append(result, semanticTransportUse(function.name, template, template.location))
	}
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
	result.location = graph.Location{}
	result.channel = instantiateTransportValue(template.channel, arguments, caller, info)
	result.payload = instantiateTransportValue(template.payload, arguments, caller, info)
	result.reliability = instantiateTransportValue(template.reliability, arguments, caller, info)
	return result
}

func instantiateTransportValue(value transportValue, arguments []goast.Expr, caller *transportFunction, info *types.Info) transportValue {
	if value.parameter < 0 || value.parameter >= len(arguments) {
		return value
	}
	return transportValueForFieldPath(arguments[value.parameter], value.field, caller, info)
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
		parts = append(parts, strconv.Itoa(value.parameter), value.field, value.constant, value.status, value.binding)
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
