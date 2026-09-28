package gdscript

import (
	"sort"
	"strconv"
	"strings"

	gdast "github.com/cafecito-games/gdparser/ast"
	"github.com/cafecito-games/grafo/internal/graph"
	transportapi "github.com/cafecito-games/grafo/internal/parser/transport"
)

const maxGDTransportWrapperDepth = 8

var gdENetAdapter transportapi.Adapter = transportapi.ExactAdapter{
	"ENetPacketPeer.send": {
		Protocol: "enet", API: "ENetPacketPeer.send", Direction: transportapi.Send,
		ChannelPosition: 0, PayloadPosition: 1, ReliabilityPosition: 2,
	},
	"ENetConnection.broadcast": {
		Protocol: "enet", API: "ENetConnection.broadcast", Direction: transportapi.Send,
		ChannelPosition: 0, PayloadPosition: 1, ReliabilityPosition: 2,
	},
	"ENetConnection.service": {
		Protocol: "enet", API: "ENetConnection.service", Direction: transportapi.Receive,
		ChannelPosition: -1, PayloadPosition: -1, ReliabilityPosition: -1,
	},
	"ENetPacketPeer.get_packet": {
		Protocol: "enet", API: "ENetPacketPeer.get_packet", Direction: transportapi.Receive,
		ChannelPosition: -1, PayloadPosition: -1, ReliabilityPosition: -1,
	},
}

type gdTransportValue struct {
	parameter int
	constant  string
	status    string
	payload   protobufAPI
	encoded   bool
}

type gdTransportTemplate struct {
	spec        transportapi.Spec
	channel     gdTransportValue
	payload     gdTransportValue
	reliability gdTransportValue
	depth       int
}

type gdTransportCall struct {
	target string
	call   *gdast.CallExpression
}

func (e *extractor) prepareTransportSummaries(statements []gdast.Statement, current scope) {
	type functionPlan struct {
		parameters map[string]int
		scope      scope
		direct     []gdTransportTemplate
		calls      []gdTransportCall
	}
	plans := map[string]*functionPlan{}
	for _, statement := range statements {
		declaration, ok := statement.(*gdast.FunctionDeclaration)
		if !ok || declaration.Abstract {
			continue
		}
		qualified := qualify(current.receiver, declaration.Name)
		functionScope := cloneFlowScope(current)
		parameters := map[string]int{}
		for index, parameter := range declaration.Parameters {
			parameters[parameter.Name] = index
			delete(functionScope.types, parameter.Name)
			if resolved := e.resolveType(parameter.Type, current); resolved != "" {
				functionScope.types[parameter.Name] = resolved
			}
		}
		plan := &functionPlan{parameters: parameters, scope: functionScope}
		for _, bodyStatement := range declaration.Body {
			gdast.Inspect(bodyStatement, func(node gdast.Node) bool {
				call, ok := node.(*gdast.CallExpression)
				if !ok {
					return true
				}
				if spec, matched := e.gdTransportSpec(call, functionScope); matched {
					plan.direct = append(plan.direct, e.gdTransportTemplate(call, spec, functionScope, parameters))
				}
				if target := e.localTransportTarget(call, functionScope); target != "" {
					plan.calls = append(plan.calls, gdTransportCall{target: target, call: call})
				}
				return true
			})
		}
		plans[qualified] = plan
		for _, summary := range plan.direct {
			e.addGDTransportSummary(qualified, summary)
		}
	}

	planNames := make([]string, 0, len(plans))
	for qualified := range plans {
		planNames = append(planNames, qualified)
	}
	sort.Strings(planNames)
	for iteration := 0; iteration < len(plans)*(maxGDTransportWrapperDepth+2); iteration++ {
		changed := false
		for _, qualified := range planNames {
			plan := plans[qualified]
			for _, call := range plan.calls {
				for _, summary := range e.transportSummaries[call.target] {
					candidate := e.instantiateGDTransport(summary, call.call.Arguments, plan.scope, plan.parameters)
					candidate.depth = summary.depth + 1
					if candidate.depth > maxGDTransportWrapperDepth {
						candidate = truncatedGDTransportTemplate(candidate)
					}
					if e.addGDTransportSummary(qualified, candidate) {
						changed = true
					}
				}
			}
		}
		if !changed {
			break
		}
	}
}

func (e *extractor) addTransportUse(node *gdast.CallExpression, callee, fromID string, current scope, loc graph.Location) {
	if !e.protobufEnabled {
		return
	}
	if spec, ok := e.gdTransportSpec(node, current); ok {
		e.emitGDTransport(node, e.gdTransportTemplate(node, spec, current, nil), fromID, current, loc)
	}
	target := e.localTransportTarget(node, current)
	grouped := map[string][]gdTransportTemplate{}
	for _, summary := range e.transportSummaries[target] {
		instantiated := e.instantiateGDTransport(summary, node.Arguments, current, nil)
		instantiated.depth = summary.depth + 1
		if instantiated.depth > maxGDTransportWrapperDepth {
			instantiated = truncatedGDTransportTemplate(instantiated)
			e.b.Diagnostic(loc.Line, "warning", "ENet wrapper summary depth exceeded; transport evidence marked truncated")
		}
		key := instantiated.spec.API + "\x00" + string(instantiated.spec.Direction)
		grouped[key] = append(grouped[key], instantiated)
	}
	for _, alternatives := range grouped {
		merged, conflict := mergeGDTransportTemplates(alternatives)
		e.emitGDTransport(node, merged, fromID, current, loc)
		if conflict {
			e.b.Diagnostic(loc.Line, "warning", "conflicting ENet wrapper summaries; transport evidence marked ambiguous")
		}
	}
	// A generated from_bytes call proves that bytes returned by a preceding
	// exact get_packet operation carry this canonical message.
	apiCallee := e.protobufCallee(callee, current)
	api, decode := e.protobufAPIs[apiCallee]
	if !decode || api.form != "from_bytes" || len(node.Arguments) == 0 {
		return
	}
	identifier, ok := node.Arguments[0].(*gdast.Identifier)
	if !ok {
		return
	}
	operationID := current.transportReceives[identifier.Name]
	if operationID == "" || api.targetID == "" || api.target == "" {
		return
	}
	e.b.AddFact(operationID, graph.EdgeCarries, api.targetID, api.target, graph.KindType, loc,
		map[string]string{"protocol": "enet", "proof": "gdscript_scope", "binding": api.symbol, "payload_position": "-1"})
}

func truncatedGDTransportTemplate(template gdTransportTemplate) gdTransportTemplate {
	template.channel = gdTransportValue{parameter: -1, status: "truncated"}
	template.payload = gdTransportValue{parameter: -1, status: "truncated"}
	template.reliability = gdTransportValue{parameter: -1, status: "truncated"}
	template.depth = maxGDTransportWrapperDepth
	return template
}

func mergeGDTransportTemplates(alternatives []gdTransportTemplate) (gdTransportTemplate, bool) {
	merged := alternatives[0]
	conflict := false
	for _, alternative := range alternatives[1:] {
		merged.channel, conflict = mergeGDTransportValue(merged.channel, alternative.channel, conflict)
		merged.payload, conflict = mergeGDTransportValue(merged.payload, alternative.payload, conflict)
		merged.reliability, conflict = mergeGDTransportValue(merged.reliability, alternative.reliability, conflict)
		if alternative.depth > merged.depth {
			merged.depth = alternative.depth
		}
	}
	return merged, conflict
}

func mergeGDTransportValue(left, right gdTransportValue, conflict bool) (gdTransportValue, bool) {
	if left.parameter == right.parameter && left.constant == right.constant && left.status == right.status &&
		left.payload.targetID == right.payload.targetID && left.encoded == right.encoded {
		return left, conflict
	}
	return gdTransportValue{parameter: -1, status: "ambiguous"}, true
}

func (e *extractor) emitGDTransport(node *gdast.CallExpression, template gdTransportTemplate, fromID string, current scope, loc graph.Location) string {
	operation := transportapi.Operation{Spec: template.spec, Channel: template.channel.constant,
		ChannelStatus: template.channel.status, Reliability: gdTransportReliability(template.reliability),
		PayloadStatus: template.payload.status, Proof: "gdscript_scope", WrapperDepth: template.depth,
		Location: loc, MessageID: template.payload.payload.targetID, Message: template.payload.payload.target,
		Binding: template.payload.payload.symbol}
	if operation.Direction == transportapi.Receive {
		operation.PayloadStatus = "unknown"
	}
	return transportapi.Emit(e.b, fromID, operation)
}

func (e *extractor) bindTransportVariable(name string, value gdast.Expression, current scope, field bool) {
	if name == "" {
		return
	}
	payload := e.gdPayload(value, current)
	if field {
		if payload.targetID == "" {
			delete(current.transportFieldPayloads, name)
		} else {
			current.transportFieldPayloads[name] = payload
		}
	} else if payload.targetID == "" {
		delete(current.transportPayloads, name)
	} else {
		current.transportPayloads[name] = payload
	}
	call, ok := value.(*gdast.CallExpression)
	if !ok {
		delete(current.transportReceives, name)
		return
	}
	spec, receive := e.gdTransportSpec(call, current)
	if !receive || spec.Direction != transportapi.Receive {
		delete(current.transportReceives, name)
		return
	}
	operation := transportapi.Operation{Spec: spec, PayloadStatus: "unknown", Proof: "gdscript_scope", Location: e.location(call)}
	current.transportReceives[name] = transportapi.OperationID(e.b, operation)
}

func (e *extractor) gdTransportSpec(call *gdast.CallExpression, current scope) (transportapi.Spec, bool) {
	member, ok := call.Callee.(*gdast.MemberExpression)
	if !ok {
		return transportapi.Spec{}, false
	}
	receiver := e.gdReceiverType(member.Object, current)
	if receiver == "" {
		return transportapi.Spec{}, false
	}
	return gdENetAdapter.Match(receiver + "." + member.Property)
}

func (e *extractor) gdReceiverType(expression gdast.Expression, current scope) string {
	switch value := expression.(type) {
	case *gdast.Identifier:
		if resolved := current.types[value.Name]; resolved != "" {
			return resolved
		}
		if value.Name == "ENetPacketPeer" || value.Name == "ENetConnection" {
			if _, shadowed := current.symbols[value.Name]; !shadowed {
				return value.Name
			}
		}
	case *gdast.MemberExpression:
		if identifier, ok := value.Object.(*gdast.Identifier); ok && identifier.Name == "self" {
			return current.fields[value.Property]
		}
	}
	return ""
}

func (e *extractor) gdTransportTemplate(call *gdast.CallExpression, spec transportapi.Spec, current scope, parameters map[string]int) gdTransportTemplate {
	template := gdTransportTemplate{spec: spec, channel: gdTransportValue{parameter: -1},
		payload: gdTransportValue{parameter: -1}, reliability: gdTransportValue{parameter: -1}}
	if spec.ChannelPosition >= 0 && spec.ChannelPosition < len(call.Arguments) {
		template.channel = e.gdTransportValue(call.Arguments[spec.ChannelPosition], current, parameters, false)
	}
	if spec.PayloadPosition >= 0 && spec.PayloadPosition < len(call.Arguments) {
		template.payload = e.gdTransportValue(call.Arguments[spec.PayloadPosition], current, parameters, true)
	}
	if spec.ReliabilityPosition >= 0 && spec.ReliabilityPosition < len(call.Arguments) {
		template.reliability = e.gdTransportValue(call.Arguments[spec.ReliabilityPosition], current, parameters, false)
	}
	return template
}

func (e *extractor) gdTransportValue(expression gdast.Expression, current scope, parameters map[string]int, payload bool) gdTransportValue {
	value := gdTransportValue{parameter: -1}
	if identifier, ok := expression.(*gdast.Identifier); ok {
		if position, exists := parameters[identifier.Name]; exists {
			value.parameter = position
		}
	}
	if payload {
		value.payload = e.gdPayload(expression, current)
		if value.payload.targetID != "" {
			value.status = "proven"
		}
		if call, ok := expression.(*gdast.CallExpression); ok {
			if member, ok := call.Callee.(*gdast.MemberExpression); ok && member.Property == "to_bytes" {
				value.encoded = true
				if identifier, ok := member.Object.(*gdast.Identifier); ok {
					if position, exists := parameters[identifier.Name]; exists {
						value.parameter = position
					}
				}
			}
		}
		return value
	}
	value.constant, value.status = e.transportConstant(expression, current)
	return value
}

func (e *extractor) gdPayload(expression gdast.Expression, current scope) protobufAPI {
	if !e.protobufEnabled {
		return protobufAPI{}
	}
	if identifier, ok := expression.(*gdast.Identifier); ok {
		if payload := current.transportPayloads[identifier.Name]; payload.targetID != "" {
			return payload
		}
		if current.symbols[identifier.Name] == current.fieldSymbols[identifier.Name] {
			return current.transportFieldPayloads[identifier.Name]
		}
		return protobufAPI{}
	}
	if member, ok := expression.(*gdast.MemberExpression); ok {
		if identifier, ok := member.Object.(*gdast.Identifier); ok && identifier.Name == "self" {
			return current.transportFieldPayloads[member.Property]
		}
	}
	call, ok := expression.(*gdast.CallExpression)
	if !ok {
		return protobufAPI{}
	}
	callee := e.protobufCallee(e.resolveCallee(call.Callee, current), current)
	api := e.protobufAPIs[callee]
	if api.form == "to_bytes" {
		return api
	}
	return protobufAPI{}
}

func (e *extractor) gdEncodedPayload(expression gdast.Expression, current scope) protobufAPI {
	if payload := e.gdPayload(expression, current); payload.targetID != "" {
		return payload
	}
	typeName := e.resolveExpression(expression, current)
	if typeName == "" || e.protobufAmbiguous[typeName] {
		return protobufAPI{}
	}
	api := e.protobufAPIs[typeName+".to_bytes"]
	if api.form == "to_bytes" {
		return api
	}
	return protobufAPI{}
}

func (e *extractor) transportConstant(expression gdast.Expression, current scope) (string, string) {
	switch value := expression.(type) {
	case *gdast.Literal:
		if value.Kind == gdast.IntegerLiteral {
			if parsed, err := strconv.ParseInt(strings.ReplaceAll(value.Raw, "_", ""), 0, 64); err == nil {
				return strconv.FormatInt(parsed, 10), "proven"
			}
		}
	case *gdast.Identifier:
		if constant := current.transportConstants[value.Name]; constant != "" {
			return constant, "proven"
		}
	case *gdast.MemberExpression:
		switch expressionName(value) {
		case "ENetPacketPeer.FLAG_RELIABLE":
			return "1", "proven"
		case "ENetPacketPeer.FLAG_UNSEQUENCED":
			return "2", "proven"
		case "ENetPacketPeer.FLAG_UNRELIABLE_FRAGMENT":
			return "8", "proven"
		}
	case *gdast.BinaryExpression:
		if value.Operator == "|" {
			left, leftStatus := e.transportConstant(value.Left, current)
			right, rightStatus := e.transportConstant(value.Right, current)
			leftNumber, leftErr := strconv.ParseInt(left, 10, 64)
			rightNumber, rightErr := strconv.ParseInt(right, 10, 64)
			if leftStatus == "proven" && rightStatus == "proven" && leftErr == nil && rightErr == nil {
				return strconv.FormatInt(leftNumber|rightNumber, 10), "proven"
			}
		}
	}
	return "", "unknown"
}

func gdTransportReliability(value gdTransportValue) string {
	if value.status != "proven" {
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

func (e *extractor) localTransportTarget(call *gdast.CallExpression, current scope) string {
	identifier, ok := call.Callee.(*gdast.Identifier)
	if !ok {
		return ""
	}
	return e.methods[qualify(current.receiver, identifier.Name)]
}

func (e *extractor) instantiateGDTransport(template gdTransportTemplate, arguments []gdast.Expression, current scope, parameters map[string]int) gdTransportTemplate {
	result := template
	result.channel = e.instantiateGDTransportValue(template.channel, arguments, current, parameters, false)
	result.payload = e.instantiateGDTransportValue(template.payload, arguments, current, parameters, true)
	result.reliability = e.instantiateGDTransportValue(template.reliability, arguments, current, parameters, false)
	return result
}

func (e *extractor) instantiateGDTransportValue(value gdTransportValue, arguments []gdast.Expression, current scope, parameters map[string]int, payload bool) gdTransportValue {
	if value.parameter < 0 || value.parameter >= len(arguments) {
		return value
	}
	result := e.gdTransportValue(arguments[value.parameter], current, parameters, payload)
	result.encoded = value.encoded
	if payload && value.encoded {
		result.payload = e.gdEncodedPayload(arguments[value.parameter], current)
		if result.payload.targetID != "" {
			result.status = "proven"
		}
	}
	return result
}

func (e *extractor) addGDTransportSummary(function string, summary gdTransportTemplate) bool {
	key := gdTransportTemplateKey(summary)
	for index, existing := range e.transportSummaries[function] {
		if gdTransportTemplateKey(existing) != key {
			continue
		}
		if summary.depth < existing.depth {
			e.transportSummaries[function][index] = summary
			return true
		}
		return false
	}
	e.transportSummaries[function] = append(e.transportSummaries[function], summary)
	return true
}

func gdTransportTemplateKey(summary gdTransportTemplate) string {
	values := []gdTransportValue{summary.channel, summary.payload, summary.reliability}
	parts := []string{summary.spec.API, string(summary.spec.Direction)}
	for _, value := range values {
		parts = append(parts, strconv.Itoa(value.parameter), value.constant, value.status, value.payload.targetID, strconv.FormatBool(value.encoded))
	}
	return strings.Join(parts, "\x00")
}
