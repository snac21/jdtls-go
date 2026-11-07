package dap

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/cage/jdtls-go/internal/jdwp"
	"github.com/cage/jdtls-go/internal/maven"
)

type variableHandle struct {
	frame  *frameRef
	object uint64
	tag    byte
}

type localValue struct {
	name     string
	typeName string
	text     string
	value    jdwp.Value
	variable jdwp.Variable
}

type evaluatedValue struct {
	value     jdwp.Value
	signature string
	constant  constant.Value
	text      string
	typeName  string
}

func parseJavaExpression(expression string) (ast.Expr, error) {
	return parser.ParseExpr(strings.TrimSpace(expression))
}

func (s *server) stackTrace(req request) error {
	if s.vm == nil {
		return fmt.Errorf("no JVM is attached")
	}
	var args struct {
		ThreadID uint64 `json:"threadId"`
		Start    int    `json:"startFrame"`
		Levels   int    `json:"levels"`
	}
	if err := json.Unmarshal(req.Arguments, &args); err != nil {
		return err
	}
	frames, err := s.vm.Frames(args.ThreadID)
	if err != nil {
		s.trace("stackTrace failed for thread=%d: %v", args.ThreadID, err)
		return err
	}
	s.trace("stackTrace thread=%d returned %d JDWP frames", args.ThreadID, len(frames))
	start := max(args.Start, 0)
	if start > len(frames) {
		start = len(frames)
	}
	end := len(frames)
	if args.Levels > 0 && start+args.Levels < end {
		end = start + args.Levels
	}
	s.frameRefs = map[int]frameRef{}
	out := make([]map[string]any, 0, end-start)
	for _, frame := range frames[start:end] {
		signature, sigErr := s.vm.ClassSignature(frame.Location.ClassID)
		if sigErr != nil {
			continue
		}
		methods, methodErr := s.vm.Methods(frame.Location.ClassID)
		if methodErr != nil {
			continue
		}
		methodName := fmt.Sprintf("method-%d", frame.Location.MethodID)
		for _, method := range methods {
			if method.ID == frame.Location.MethodID {
				methodName = method.Name
				break
			}
		}
		line := 0
		if lines, lineErr := s.vm.LineTable(frame.Location.ClassID, frame.Location.MethodID); lineErr == nil {
			for _, entry := range lines {
				if entry.CodeIndex > frame.Location.Index {
					break
				}
				line = entry.Line
			}
		}
		source := s.sourceForClass(frame.Location.ClassID, signature)
		s.nextFrameID++
		frameID := s.nextFrameID
		s.frameRefs[frameID] = frameRef{ThreadID: args.ThreadID, FrameID: frame.ID, ClassID: frame.Location.ClassID, MethodID: frame.Location.MethodID, CodeIndex: frame.Location.Index}
		frameBody := map[string]any{"id": frameID, "name": methodName, "column": 1, "presentationHint": "normal"}
		if line > 0 {
			frameBody["line"] = line
		}
		if source != "" {
			frameBody["source"] = map[string]any{"name": filepath.Base(source), "path": source, "sourceReference": 0}
		}
		out = append(out, frameBody)
	}
	return s.response(req, true, map[string]any{"stackFrames": out, "totalFrames": len(frames)})
}

func (s *server) sourceForClass(classID uint64, signature string) string {
	sourceName, err := s.vm.SourceFile(classID)
	if err != nil || sourceName == "" {
		return ""
	}
	packagePath, _, ok := classPackageAndName(signature)
	if !ok {
		return ""
	}
	for _, root := range s.sourceRoots {
		candidate := filepath.Join(root, packagePath, sourceName)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	entry := filepath.ToSlash(filepath.Join(packagePath, sourceName))
	archives := make([]string, 0, len(s.sourceAttachments))
	for _, archive := range s.sourceAttachments {
		archives = append(archives, archive)
	}
	sort.Strings(archives)
	for _, archive := range archives {
		if _, readErr := maven.ReadArchiveFile(archive, entry); readErr == nil {
			return sourceArchiveURI(archive, entry)
		}
	}
	s.bpMu.Lock()
	defer s.bpMu.Unlock()
	suffix := filepath.ToSlash(filepath.Join(packagePath, sourceName))
	for source := range s.bps {
		if strings.HasSuffix(filepath.ToSlash(source), suffix) {
			return source
		}
	}
	return ""
}

func sourceArchiveURI(archive, entry string) string {
	uri := url.URL{Scheme: "jdt", Host: "contents", Path: "/" + filepath.Base(archive)}
	query := uri.Query()
	query.Set("archive", archive)
	query.Set("entry", filepath.ToSlash(entry))
	uri.RawQuery = query.Encode()
	return uri.String()
}

func (s *server) scopes(req request) error {
	var args struct {
		FrameID int `json:"frameId"`
	}
	if err := json.Unmarshal(req.Arguments, &args); err != nil {
		return err
	}
	frame, ok := s.frameRefs[args.FrameID]
	if !ok {
		return fmt.Errorf("unknown stack frame %d", args.FrameID)
	}
	variables, err := s.localValues(frame)
	if err != nil {
		return err
	}
	_ = variables
	ref := s.addHandle(variableHandle{frame: &frame})
	return s.response(req, true, map[string]any{"scopes": []map[string]any{{"name": "Locals", "variablesReference": ref, "expensive": false}}})
}

func (s *server) evaluate(req request) error {
	var args struct {
		Expression string `json:"expression"`
		FrameID    int    `json:"frameId"`
	}
	if err := json.Unmarshal(req.Arguments, &args); err != nil {
		return err
	}
	frame, ok := s.frameRefs[args.FrameID]
	if !ok {
		return fmt.Errorf("evaluate requires a valid frameId")
	}
	expression, err := parser.ParseExpr(strings.TrimSpace(args.Expression))
	if err != nil {
		return fmt.Errorf("invalid Java expression: %w", err)
	}
	locals, err := s.localValues(frame)
	if err != nil {
		return err
	}
	value, err := s.evalExpression(frame, expression, locals)
	if err != nil {
		return err
	}
	ref := 0
	if value.value.ObjectID != 0 {
		ref = s.addHandle(variableHandle{object: value.value.ObjectID, tag: value.value.Tag})
	}
	return s.response(req, true, map[string]any{"result": value.text, "type": value.typeName, "variablesReference": ref})
}

func (s *server) completions(req request) error {
	var args struct {
		FrameID int    `json:"frameId"`
		Text    string `json:"text"`
		Line    int    `json:"line"`
		Column  int    `json:"column"`
	}
	if err := json.Unmarshal(req.Arguments, &args); err != nil {
		return err
	}
	if args.FrameID == 0 {
		return s.response(req, true, map[string]any{"targets": []any{}})
	}
	frame, ok := s.frameRefs[args.FrameID]
	if !ok {
		return fmt.Errorf("unknown stack frame %d", args.FrameID)
	}
	values, err := s.localValues(frame)
	if err != nil {
		return err
	}
	prefix := args.Text
	if dot := strings.LastIndexAny(prefix, ". 	\n"); dot >= 0 {
		prefix = prefix[dot+1:]
	}
	targets := make([]map[string]any, 0, len(values))
	for _, value := range values {
		if !strings.HasPrefix(value.name, prefix) {
			continue
		}
		targets = append(targets, map[string]any{"label": value.name, "text": value.name, "sortText": value.name, "type": value.typeName})
	}
	return s.response(req, true, map[string]any{"targets": targets})
}

func (s *server) dataBreakpointInfo(req request) error {
	if s.vm == nil {
		return fmt.Errorf("no JVM is attached")
	}
	var args struct {
		Name               string `json:"name"`
		VariablesReference int    `json:"variablesReference"`
	}
	if err := json.Unmarshal(req.Arguments, &args); err != nil {
		return err
	}
	handle, ok := s.handles[args.VariablesReference]
	if !ok || handle.object == 0 {
		return s.response(req, true, map[string]any{"dataId": nil, "description": "Java local variables do not support data breakpoints", "accessTypes": []any{}, "canPersist": false})
	}
	tag, classID, err := s.vm.ObjectType(handle.object)
	if err != nil {
		return err
	}
	fields, err := s.vm.Fields(classID)
	if err != nil {
		return err
	}
	for _, field := range fields {
		if field.Name != args.Name {
			continue
		}
		dataID := fmt.Sprintf("%d:%d:%d", tag, classID, field.ID)
		return s.response(req, true, map[string]any{"dataId": dataID, "description": args.Name + " : " + field.Signature, "accessTypes": []string{"read", "write", "readWrite"}, "canPersist": true})
	}
	return s.response(req, true, map[string]any{"dataId": nil, "description": "field not found on object", "accessTypes": []any{}, "canPersist": false})
}

func (s *server) setDataBreakpoints(req request) error {
	if s.vm == nil {
		return fmt.Errorf("no JVM is attached")
	}
	var args struct {
		Breakpoints []struct {
			DataID     string `json:"dataId"`
			AccessType string `json:"accessType"`
		} `json:"breakpoints"`
	}
	if err := json.Unmarshal(req.Arguments, &args); err != nil {
		return err
	}
	s.bpMu.Lock()
	for requestID := range s.dataWatchpoints {
		_ = s.vm.ClearEvent(jdwp.EventAccessWatchpoint, requestID)
		_ = s.vm.ClearEvent(jdwp.EventModificationWatchpoint, requestID)
		delete(s.dataWatchpoints, requestID)
	}
	s.bpMu.Unlock()
	items := make([]map[string]any, 0, len(args.Breakpoints))
	for index, breakpoint := range args.Breakpoints {
		parts := strings.Split(breakpoint.DataID, ":")
		if len(parts) != 3 {
			items = append(items, map[string]any{"id": index + 1, "verified": false, "message": "invalid Java field dataId"})
			continue
		}
		tagValue, tagErr := strconv.ParseUint(parts[0], 10, 8)
		classID, classErr := strconv.ParseUint(parts[1], 10, 64)
		fieldID, fieldErr := strconv.ParseUint(parts[2], 10, 64)
		if tagErr != nil || classErr != nil || fieldErr != nil {
			items = append(items, map[string]any{"id": index + 1, "verified": false, "message": "invalid Java field dataId"})
			continue
		}
		access := breakpoint.AccessType == "read" || breakpoint.AccessType == "readWrite"
		write := breakpoint.AccessType == "" || breakpoint.AccessType == "write" || breakpoint.AccessType == "readWrite"
		var requestIDs []uint32
		var err error
		if access {
			var requestID uint32
			requestID, err = s.vm.SetFieldWatch(byte(tagValue), classID, fieldID, true)
			if err == nil {
				requestIDs = append(requestIDs, requestID)
			}
		}
		if err == nil && write {
			var requestID uint32
			requestID, err = s.vm.SetFieldWatch(byte(tagValue), classID, fieldID, false)
			if err == nil {
				requestIDs = append(requestIDs, requestID)
			}
		}
		if err != nil || len(requestIDs) == 0 {
			for _, requestID := range requestIDs {
				_ = s.vm.ClearEvent(jdwp.EventAccessWatchpoint, requestID)
				_ = s.vm.ClearEvent(jdwp.EventModificationWatchpoint, requestID)
			}
			message := "unsupported data breakpoint access type"
			if err != nil {
				message = err.Error()
			}
			items = append(items, map[string]any{"id": index + 1, "verified": false, "message": message})
			continue
		}
		s.bpMu.Lock()
		for _, requestID := range requestIDs {
			s.dataWatchpoints[requestID] = index + 1
		}
		s.bpMu.Unlock()
		items = append(items, map[string]any{"id": index + 1, "verified": true})
	}
	return s.response(req, true, map[string]any{"breakpoints": items})
}

func (s *server) setVariable(req request) error {
	var args struct {
		Reference int    `json:"variablesReference"`
		Name      string `json:"name"`
		Value     string `json:"value"`
	}
	if err := json.Unmarshal(req.Arguments, &args); err != nil {
		return err
	}
	handle, ok := s.handles[args.Reference]
	if !ok {
		return fmt.Errorf("unknown variables reference %d", args.Reference)
	}
	expression, err := parseJavaExpression(args.Value)
	if err != nil {
		return fmt.Errorf("invalid Java expression: %w", err)
	}
	var frame frameRef
	var locals []localValue
	if handle.frame != nil {
		frame = *handle.frame
		locals, err = s.localValues(frame)
		if err != nil {
			return err
		}
	}
	evaluated, err := s.evalExpression(frame, expression, locals)
	if err != nil {
		return err
	}
	var signature string
	if handle.frame != nil {
		found := false
		for _, local := range locals {
			if local.name == args.Name {
				signature = local.variable.Signature
				value, valueErr := convertValue(evaluated, signature)
				if valueErr != nil {
					return valueErr
				}
				if err := s.vm.SetFrameValue(frame.ThreadID, frame.FrameID, local.variable, value); err != nil {
					return err
				}
				evaluated.value = value
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("local variable %q is not visible in this frame", args.Name)
		}
	} else {
		_, classID, typeErr := s.vm.ObjectType(handle.object)
		if typeErr != nil {
			return typeErr
		}
		fields, fieldErr := s.vm.Fields(classID)
		if fieldErr != nil {
			return fieldErr
		}
		found := false
		for _, field := range fields {
			if field.Name != args.Name || field.Modifiers&0x0008 != 0 {
				continue
			}
			signature = field.Signature
			value, valueErr := convertValue(evaluated, signature)
			if valueErr != nil {
				return valueErr
			}
			if err := s.vm.SetObjectValue(handle.object, field, value); err != nil {
				return err
			}
			evaluated.value = value
			found = true
			break
		}
		if !found {
			return fmt.Errorf("instance field %q was not found", args.Name)
		}
	}
	text, typeName := s.formatValue(evaluated.value, signature)
	ref := 0
	if evaluated.value.ObjectID != 0 {
		ref = s.addHandle(variableHandle{frame: handle.frame, object: evaluated.value.ObjectID, tag: evaluated.value.Tag})
	}
	return s.response(req, true, map[string]any{"value": text, "type": typeName, "variablesReference": ref})
}

func convertValue(value evaluatedValue, signature string) (jdwp.Value, error) {
	if signature == "" {
		return jdwp.Value{}, fmt.Errorf("target variable has no Java type")
	}
	switch signature[0] {
	case 'Z':
		if value.constant == nil || value.constant.Kind() != constant.Bool {
			return jdwp.Value{}, fmt.Errorf("boolean variable requires a boolean value")
		}
		converted := jdwp.Value{Tag: 'Z'}
		if constant.BoolVal(value.constant) {
			converted.Integer = 1
		}
		return converted, nil
	case 'B', 'C', 'S', 'I', 'J':
		if value.constant == nil || value.constant.Kind() != constant.Int {
			return jdwp.Value{}, fmt.Errorf("%s variable requires an integer value", javaTypeName(signature))
		}
		integer, ok := constant.Int64Val(value.constant)
		if !ok {
			return jdwp.Value{}, fmt.Errorf("integer value is out of range")
		}
		return jdwp.Value{Tag: signature[0], Integer: integer}, nil
	case 'F', 'D':
		if value.constant == nil || (value.constant.Kind() != constant.Int && value.constant.Kind() != constant.Float) {
			return jdwp.Value{}, fmt.Errorf("%s variable requires a numeric value", javaTypeName(signature))
		}
		floating, ok := constant.Float64Val(value.constant)
		if !ok {
			return jdwp.Value{}, fmt.Errorf("floating-point value is out of range")
		}
		return jdwp.Value{Tag: signature[0], Float: floating}, nil
	case 'L', '[':
		if value.value.ObjectID != 0 || value.text == "null" {
			return value.value, nil
		}
		return jdwp.Value{}, fmt.Errorf("reference variable requires an object value or null")
	default:
		return jdwp.Value{}, fmt.Errorf("cannot assign to Java type %q", signature)
	}
}

func (s *server) evalExpression(frame frameRef, expression ast.Expr, locals []localValue) (evaluatedValue, error) {
	switch node := expression.(type) {
	case *ast.ParenExpr:
		return s.evalExpression(frame, node.X, locals)
	case *ast.BasicLit:
		value := constant.MakeFromLiteral(node.Value, node.Kind, 0)
		if value.Kind() == constant.Unknown {
			return evaluatedValue{}, fmt.Errorf("invalid literal %q", node.Value)
		}
		return constantResult(value), nil
	case *ast.Ident:
		switch node.Name {
		case "true":
			return constantResult(constant.MakeBool(true)), nil
		case "false":
			return constantResult(constant.MakeBool(false)), nil
		case "null":
			return evaluatedValue{value: jdwp.Value{}, signature: "Ljava/lang/Object;", text: "null", typeName: "null"}, nil
		}
		for _, local := range locals {
			if local.name == node.Name {
				result := evaluatedValue{value: local.value, signature: local.typeName, text: local.text, typeName: local.typeName}
				if constantValue, ok := localConstant(local.value); ok {
					result.constant = constantValue
				}
				return result, nil
			}
		}
		return evaluatedValue{}, fmt.Errorf("unknown local variable %q", node.Name)
	case *ast.UnaryExpr:
		operand, err := s.evalExpression(frame, node.X, locals)
		if err != nil {
			return evaluatedValue{}, err
		}
		if operand.constant == nil {
			return evaluatedValue{}, fmt.Errorf("operator %s requires a primitive value", node.Op)
		}
		result := constant.UnaryOp(node.Op, operand.constant, 0)
		if result.Kind() == constant.Unknown {
			return evaluatedValue{}, fmt.Errorf("unsupported unary operator %s", node.Op)
		}
		return constantResult(result), nil
	case *ast.BinaryExpr:
		left, err := s.evalExpression(frame, node.X, locals)
		if err != nil {
			return evaluatedValue{}, err
		}
		right, err := s.evalExpression(frame, node.Y, locals)
		if err != nil {
			return evaluatedValue{}, err
		}
		if left.constant == nil || right.constant == nil {
			return evaluatedValue{}, fmt.Errorf("operator %s requires primitive values", node.Op)
		}
		if node.Op == token.EQL || node.Op == token.NEQ || node.Op == token.LSS || node.Op == token.LEQ || node.Op == token.GTR || node.Op == token.GEQ {
			return constantResult(constant.MakeBool(constant.Compare(left.constant, node.Op, right.constant))), nil
		}
		if node.Op == token.QUO && right.constant.Kind() == constant.Int && constant.Sign(right.constant) == 0 {
			return evaluatedValue{}, fmt.Errorf("division by zero")
		}
		result := constant.BinaryOp(left.constant, node.Op, right.constant)
		if result.Kind() == constant.Unknown {
			return evaluatedValue{}, fmt.Errorf("unsupported expression operator %s", node.Op)
		}
		return constantResult(result), nil
	case *ast.SelectorExpr:
		parent, err := s.evalExpression(frame, node.X, locals)
		if err != nil {
			return evaluatedValue{}, err
		}
		if parent.value.ObjectID == 0 {
			return evaluatedValue{}, fmt.Errorf("cannot read %s from null", node.Sel.Name)
		}
		_, classID, err := s.vm.ObjectType(parent.value.ObjectID)
		if err != nil {
			return evaluatedValue{}, err
		}
		fields, err := s.vm.Fields(classID)
		if err != nil {
			return evaluatedValue{}, err
		}
		for _, field := range fields {
			if field.Name != node.Sel.Name || field.Modifiers&0x0008 != 0 {
				continue
			}
			values, err := s.vm.ObjectValues(parent.value.ObjectID, []jdwp.Field{field})
			if err != nil {
				return evaluatedValue{}, err
			}
			text, typeName := s.formatValue(values[0], field.Signature)
			result := evaluatedValue{value: values[0], signature: field.Signature, text: text, typeName: typeName}
			if constantValue, ok := localConstant(values[0]); ok {
				result.constant = constantValue
			}
			return result, nil
		}
		return evaluatedValue{}, fmt.Errorf("field %q was not found", node.Sel.Name)
	case *ast.IndexExpr:
		array, err := s.evalExpression(frame, node.X, locals)
		if err != nil {
			return evaluatedValue{}, err
		}
		index, err := s.evalExpression(frame, node.Index, locals)
		if err != nil {
			return evaluatedValue{}, err
		}
		if array.value.ObjectID == 0 || array.value.Tag != '[' {
			return evaluatedValue{}, fmt.Errorf("indexing requires a non-null array")
		}
		if index.constant == nil {
			return evaluatedValue{}, fmt.Errorf("array index must be an integer")
		}
		position, ok := constant.Int64Val(constant.ToInt(index.constant))
		if !ok || position < 0 {
			return evaluatedValue{}, fmt.Errorf("array index is outside range")
		}
		values, err := s.vm.ArrayValues(array.value.ObjectID, int(position), 1)
		if err != nil {
			return evaluatedValue{}, err
		}
		text, typeName := s.formatValue(values[0], "")
		result := evaluatedValue{value: values[0], text: text, typeName: typeName}
		if constantValue, ok := localConstant(values[0]); ok {
			result.constant = constantValue
		}
		return result, nil
	default:
		return evaluatedValue{}, fmt.Errorf("expression form %T is not supported yet", expression)
	}
}

func localConstant(value jdwp.Value) (constant.Value, bool) {
	switch value.Tag {
	case 'Z':
		return constant.MakeBool(value.Integer != 0), true
	case 'B', 'C', 'I', 'J', 'S':
		return constant.MakeInt64(value.Integer), true
	case 'F', 'D':
		return constant.MakeFloat64(value.Float), true
	default:
		return nil, false
	}
}

func constantResult(value constant.Value) evaluatedValue {
	result := evaluatedValue{constant: value}
	switch value.Kind() {
	case constant.Bool:
		result.typeName = "boolean"
		result.text = strconv.FormatBool(constant.BoolVal(value))
		result.value = jdwp.Value{Tag: 'Z'}
		if constant.BoolVal(value) {
			result.value.Integer = 1
		}
	case constant.Int:
		result.typeName = "int"
		result.text = value.ExactString()
		if integer, ok := constant.Int64Val(value); ok {
			result.value = jdwp.Value{Tag: 'I', Integer: integer}
		}
	case constant.Float:
		result.typeName = "double"
		if floating, ok := constant.Float64Val(value); ok {
			result.text = strconv.FormatFloat(floating, 'g', -1, 64)
			result.value = jdwp.Value{Tag: 'D', Float: floating}
		}
	case constant.String:
		result.typeName = "java.lang.String"
		result.text = strconv.Quote(constant.StringVal(value))
	default:
		result.text = value.ExactString()
	}
	return result
}

func (s *server) variables(req request) error {
	var args struct {
		Reference int `json:"variablesReference"`
		Start     int `json:"start"`
		Count     int `json:"count"`
	}
	if err := json.Unmarshal(req.Arguments, &args); err != nil {
		return err
	}
	handle, ok := s.handles[args.Reference]
	if !ok {
		return fmt.Errorf("unknown variables reference %d", args.Reference)
	}
	if handle.frame != nil {
		locals, err := s.localValues(*handle.frame)
		if err != nil {
			return err
		}
		body := make([]map[string]any, 0, len(locals))
		for _, local := range locals {
			ref := 0
			if local.value.ObjectID != 0 {
				ref = s.addHandle(variableHandle{frame: handle.frame, object: local.value.ObjectID, tag: local.value.Tag})
			}
			body = append(body, map[string]any{"name": local.name, "value": local.text, "type": local.typeName, "variablesReference": ref})
		}
		return s.response(req, true, map[string]any{"variables": body})
	}
	return s.objectVariables(req, handle, args.Start, args.Count)
}

func (s *server) addHandle(handle variableHandle) int {
	s.nextHandle++
	s.handles[s.nextHandle] = handle
	return s.nextHandle
}

func (s *server) localValues(frame frameRef) ([]localValue, error) {
	variables, err := s.vm.Variables(frame.ClassID, frame.MethodID)
	if err != nil {
		s.trace("read local variable table failed class=%d method=%d: %v", frame.ClassID, frame.MethodID, err)
		return nil, err
	}
	active := make([]jdwp.Variable, 0, len(variables))
	for _, variable := range variables {
		if frame.CodeIndex >= variable.CodeIndex && frame.CodeIndex < variable.CodeIndex+int64(variable.Length) {
			active = append(active, variable)
		}
	}
	if len(active) == 0 {
		s.trace("no locals active at class=%d method=%d codeIndex=%d; table entries=%d", frame.ClassID, frame.MethodID, frame.CodeIndex, len(variables))
	}
	values, err := s.vm.FrameValues(frame.ThreadID, frame.FrameID, active)
	if err != nil {
		s.trace("read local values failed thread=%d frame=%d: %v", frame.ThreadID, frame.FrameID, err)
		return nil, err
	}
	out := make([]localValue, 0, len(active))
	for i, variable := range active {
		text, typeName := s.formatValue(values[i], variable.Signature)
		out = append(out, localValue{name: variable.Name, typeName: typeName, text: text, value: values[i], variable: variable})
	}
	return out, nil
}

func (s *server) formatValue(value jdwp.Value, signature string) (string, string) {
	typeName := javaTypeName(signature)
	switch value.Tag {
	case 'Z':
		return strconv.FormatBool(value.Integer != 0), "boolean"
	case 'B', 'C', 'D', 'F', 'I', 'J', 'S':
		if value.Tag == 'C' {
			return string(rune(value.Integer)), "char"
		}
		if value.Tag == 'F' || value.Tag == 'D' {
			return strconv.FormatFloat(value.Float, 'g', -1, 64), typeName
		}
		return strconv.FormatInt(value.Integer, 10), typeName
	case 'L', '[', 's', 't', 'g', 'l', 'c':
		if value.ObjectID == 0 {
			return "null", typeName
		}
		if value.Tag == 's' || typeName == "java.lang.String" {
			if text, err := s.vm.StringValue(value.ObjectID); err == nil {
				return strconv.Quote(text), "java.lang.String"
			}
		}
		return fmt.Sprintf("%s@%x", typeName, value.ObjectID), typeName
	default:
		return "<unavailable>", typeName
	}
}

func (s *server) objectVariables(req request, handle variableHandle, start, count int) error {
	if handle.object == 0 {
		return s.response(req, true, map[string]any{"variables": []any{}})
	}
	if handle.tag == '[' {
		length, err := s.vm.ArrayLength(handle.object)
		if err != nil {
			return err
		}
		if start < 0 {
			start = 0
		}
		if start > length {
			start = length
		}
		end := length
		if count > 0 && start+count < end {
			end = start + count
		}
		values, err := s.vm.ArrayValues(handle.object, start, end-start)
		if err != nil {
			return err
		}
		body := make([]map[string]any, 0, len(values))
		for index, value := range values {
			text, typeName := s.formatValue(value, "")
			ref := 0
			if value.ObjectID != 0 {
				ref = s.addHandle(variableHandle{frame: handle.frame, object: value.ObjectID, tag: value.Tag})
			}
			body = append(body, map[string]any{"name": fmt.Sprintf("[%d]", start+index), "value": text, "type": typeName, "variablesReference": ref})
		}
		return s.response(req, true, map[string]any{"variables": body})
	}
	_, classID, err := s.vm.ObjectType(handle.object)
	if err != nil {
		return err
	}
	fields, err := s.vm.Fields(classID)
	if err != nil {
		return err
	}
	instance := make([]jdwp.Field, 0, len(fields))
	for _, field := range fields {
		if field.Modifiers&0x0008 == 0 {
			instance = append(instance, field)
		}
	}
	if start < 0 {
		start = 0
	}
	if start > len(instance) {
		start = len(instance)
	}
	end := len(instance)
	if count > 0 && start+count < end {
		end = start + count
	}
	fields = instance[start:end]
	values, err := s.vm.ObjectValues(handle.object, fields)
	if err != nil {
		return err
	}
	body := make([]map[string]any, 0, len(fields))
	for index, field := range fields {
		text, typeName := s.formatValue(values[index], field.Signature)
		ref := 0
		if values[index].ObjectID != 0 {
			ref = s.addHandle(variableHandle{frame: handle.frame, object: values[index].ObjectID, tag: values[index].Tag})
		}
		body = append(body, map[string]any{"name": field.Name, "value": text, "type": typeName, "variablesReference": ref})
	}
	return s.response(req, true, map[string]any{"variables": body})
}

func javaTypeName(signature string) string {
	if signature == "" {
		return "Object"
	}
	for strings.HasPrefix(signature, "[") {
		return javaTypeName(signature[1:]) + "[]"
	}
	primitive := map[byte]string{'Z': "boolean", 'B': "byte", 'C': "char", 'D': "double", 'F': "float", 'I': "int", 'J': "long", 'S': "short", 'V': "void"}
	if name, ok := primitive[signature[0]]; ok {
		return name
	}
	if strings.HasPrefix(signature, "L") && strings.HasSuffix(signature, ";") {
		return strings.ReplaceAll(signature[1:len(signature)-1], "/", ".")
	}
	return signature
}
