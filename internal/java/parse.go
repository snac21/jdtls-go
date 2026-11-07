package java

import (
	"fmt"
	"strings"

	ts "github.com/tree-sitter/go-tree-sitter"
	tree_sitter_java "github.com/tree-sitter/tree-sitter-java/bindings/go"
)

type Position struct {
	Line      int
	Character int
}

type Range struct {
	Start Position
	End   Position
}

type Diagnostic struct {
	Range   Range
	Message string
}

type Symbol struct {
	Name      string
	Detail    string
	Kind      int
	Container string
	Range     Range
	Selection Range
}

type Declaration struct {
	Name      string
	Detail    string
	Kind      int
	Container string
	Range     Range
	Selection Range
}

type Identifier struct {
	Name      string
	Kind      string
	Container string
	Range     Range
}

type TypeInfo struct {
	Name          string
	QualifiedName string
	Kind          string
	Super         string
	Interfaces    []string
	Members       []MemberInfo
	Range         Range
	Selection     Range
}

type MemberInfo struct {
	Name       string
	Kind       string
	Type       string
	Signature  string
	Parameters []ParameterInfo
	Locals     []VariableInfo
	Static     bool
	Range      Range
	Selection  Range
}

type ParameterInfo struct {
	Name string
	Type string
}

type VariableInfo struct {
	Name  string
	Type  string
	Range Range
	Scope Range
}

type MethodInvocation struct {
	Name          string
	Receiver      string
	ArgumentCount int
	Range         Range
}

type Unit struct {
	Package      string
	Imports      []string
	Symbols      []Symbol
	Declarations []Declaration
	Identifiers  []Identifier
	Invocations  []MethodInvocation
	Types        []TypeInfo
	Diagnostics  []Diagnostic
}

var typeKinds = map[string]int{
	"class_declaration":           5,
	"record_declaration":          5,
	"interface_declaration":       11,
	"annotation_type_declaration": 11,
	"enum_declaration":            10,
}

// Parse builds a recoverable syntax index. Tree-sitter keeps a partial tree for
// incomplete editor buffers and reports ERROR/MISSING nodes as diagnostics.
func Parse(source []byte) (Unit, error) {
	parser := ts.NewParser()
	defer parser.Close()
	language := ts.NewLanguage(tree_sitter_java.Language())
	if err := parser.SetLanguage(language); err != nil {
		return Unit{}, fmt.Errorf("initialize Java grammar: %w", err)
	}
	tree := parser.Parse(source, nil)
	if tree == nil {
		return Unit{}, fmt.Errorf("parse Java source: parser returned no syntax tree")
	}
	defer tree.Close()
	unit := Unit{}
	root := tree.RootNode()
	if root == nil {
		return unit, nil
	}
	var walk func(*ts.Node, string, string)
	walk = func(node *ts.Node, container, qualifiedContainer string) {
		if node == nil {
			return
		}
		kind := node.Kind()
		if node.IsError() || node.IsMissing() {
			message := "invalid or incomplete Java syntax"
			if node.IsMissing() {
				message = fmt.Sprintf("expected %s", node.Kind())
			}
			unit.Diagnostics = append(unit.Diagnostics, Diagnostic{Range: nodeRange(source, node), Message: message})
		}
		if kind == "package_declaration" {
			if name := node.ChildByFieldName("name"); name != nil {
				unit.Package = nodeText(source, name)
			} else {
				for i := uint(0); i < node.NamedChildCount(); i++ {
					child := node.NamedChild(i)
					if child != nil && (child.Kind() == "scoped_identifier" || child.Kind() == "identifier") {
						unit.Package = nodeText(source, child)
						break
					}
				}
				if unit.Package == "" {
					text := strings.TrimSpace(nodeText(source, node))
					text = strings.TrimPrefix(text, "package ")
					text = strings.TrimSuffix(text, ";")
					unit.Package = strings.TrimSpace(text)
				}
			}
		}
		if kind == "import_declaration" {
			name := strings.TrimSpace(strings.TrimPrefix(nodeText(source, node), "import"))
			name = strings.TrimSuffix(strings.TrimSpace(name), ";")
			name = strings.TrimPrefix(name, "static ")
			unit.Imports = append(unit.Imports, strings.TrimSpace(name))
		}
		if kind == "identifier" || kind == "type_identifier" {
			unit.Identifiers = append(unit.Identifiers, Identifier{Name: nodeText(source, node), Kind: kind, Container: container, Range: nodeRange(source, node)})
		}
		if kind == "method_invocation" {
			name := node.ChildByFieldName("name")
			if name != nil {
				receiver := ""
				if object := node.ChildByFieldName("object"); object != nil {
					receiver = nodeText(source, object)
				}
				argumentCount := 0
				if arguments := node.ChildByFieldName("arguments"); arguments != nil {
					argumentCount = int(arguments.NamedChildCount())
				}
				unit.Invocations = append(unit.Invocations, MethodInvocation{
					Name: nodeText(source, name), Receiver: receiver, ArgumentCount: argumentCount, Range: nodeRange(source, name),
				})
			}
		}
		if typeKind, ok := typeKinds[kind]; ok {
			nameNode := node.ChildByFieldName("name")
			if nameNode != nil {
				name := nodeText(source, nameNode)
				qualifiedName := name
				if qualifiedContainer != "" {
					qualifiedName = qualifiedContainer + "$" + name
				} else if unit.Package != "" {
					qualifiedName = unit.Package + "." + name
				}
				typeInfo := TypeInfo{Name: name, QualifiedName: qualifiedName, Kind: kind, Range: nodeRange(source, node), Selection: nodeRange(source, nameNode)}
				if superclass := node.ChildByFieldName("superclass"); superclass != nil {
					typeInfo.Super = strings.TrimSpace(strings.TrimPrefix(nodeText(source, superclass), "extends"))
				}
				if interfaces := node.ChildByFieldName("interfaces"); interfaces != nil {
					typeInfo.Interfaces = append(typeInfo.Interfaces, parseInterfaceList(source, interfaces)...)
				} else {
					for i := uint(0); i < node.NamedChildCount(); i++ {
						child := node.NamedChild(i)
						if child != nil && child.Kind() == "extends_interfaces" {
							typeInfo.Interfaces = append(typeInfo.Interfaces, parseInterfaceList(source, child)...)
						}
					}
				}
				if body := node.ChildByFieldName("body"); body != nil {
					typeInfo.Members = parseMembers(source, body)
				}
				unit.Types = append(unit.Types, typeInfo)
				detail := strings.TrimSpace(nodeText(source, node))
				if body := node.ChildByFieldName("body"); body != nil && body.StartByte() <= uint(len(source)) {
					detail = strings.TrimSpace(string(source[node.StartByte():body.StartByte()]))
				}
				symbol := Symbol{Name: name, Detail: detail, Kind: typeKind, Container: container, Range: nodeRange(source, node), Selection: nodeRange(source, nameNode)}
				unit.Symbols = append(unit.Symbols, symbol)
				unit.Declarations = append(unit.Declarations, Declaration{Name: name, Detail: detail, Kind: typeKind, Container: container, Range: symbol.Range, Selection: symbol.Selection})
				container = name
				qualifiedContainer = qualifiedName
			}
		}
		switch kind {
		case "method_declaration", "constructor_declaration", "annotation_type_element_declaration":
			nameNode := node.ChildByFieldName("name")
			if nameNode != nil {
				name := nodeText(source, nameNode)
				kindCode := 6
				if kind == "constructor_declaration" {
					kindCode = 9
				}
				returnType := node.ChildByFieldName("type")
				parameters := node.ChildByFieldName("parameters")
				detail := name
				if returnType != nil {
					detail = nodeText(source, returnType) + " " + detail
				}
				if parameters != nil {
					detail += nodeText(source, parameters)
				}
				symbol := Symbol{Name: name, Detail: detail, Kind: kindCode, Container: container, Range: nodeRange(source, node), Selection: nodeRange(source, nameNode)}
				unit.Symbols = append(unit.Symbols, symbol)
				unit.Declarations = append(unit.Declarations, Declaration{Name: name, Detail: detail, Kind: kindCode, Container: container, Range: symbol.Range, Selection: symbol.Selection})
			}
		case "field_declaration", "constant_declaration":
			typeNode := node.ChildByFieldName("type")
			fieldType := ""
			if typeNode != nil {
				fieldType = nodeText(source, typeNode)
			}
			for i := uint(0); i < node.NamedChildCount(); i++ {
				child := node.NamedChild(i)
				if child == nil || child.Kind() != "variable_declarator" {
					continue
				}
				nameNode := child.ChildByFieldName("name")
				if nameNode == nil {
					continue
				}
				name := nodeText(source, nameNode)
				detail := strings.TrimSpace(fieldType + " " + name)
				symbol := Symbol{Name: name, Detail: detail, Kind: 8, Container: container, Range: nodeRange(source, child), Selection: nodeRange(source, nameNode)}
				unit.Symbols = append(unit.Symbols, symbol)
				unit.Declarations = append(unit.Declarations, Declaration{Name: name, Detail: detail, Kind: 8, Container: container, Range: symbol.Range, Selection: symbol.Selection})
			}
		case "enum_constant":
			nameNode := node.ChildByFieldName("name")
			if nameNode != nil {
				name := nodeText(source, nameNode)
				symbol := Symbol{Name: name, Detail: "enum constant " + name, Kind: 22, Container: container, Range: nodeRange(source, node), Selection: nodeRange(source, nameNode)}
				unit.Symbols = append(unit.Symbols, symbol)
				unit.Declarations = append(unit.Declarations, Declaration{Name: name, Detail: symbol.Detail, Kind: symbol.Kind, Container: container, Range: symbol.Range, Selection: symbol.Selection})
			}
		}
		for i := uint(0); i < node.NamedChildCount(); i++ {
			walk(node.NamedChild(i), container, qualifiedContainer)
		}
	}
	walk(root, "", "")
	return unit, nil
}

func parseInterfaceList(source []byte, node *ts.Node) []string {
	list := strings.TrimSpace(nodeText(source, node))
	list = strings.TrimSpace(strings.TrimPrefix(list, "implements"))
	list = strings.TrimSpace(strings.TrimPrefix(list, "extends"))
	var interfaces []string
	for _, value := range strings.Split(list, ",") {
		if value = strings.TrimSpace(value); value != "" {
			interfaces = append(interfaces, value)
		}
	}
	return interfaces
}

func parseMembers(source []byte, body *ts.Node) []MemberInfo {
	var members []MemberInfo
	for i := uint(0); i < body.NamedChildCount(); i++ {
		node := body.NamedChild(i)
		if node == nil {
			continue
		}
		switch node.Kind() {
		case "field_declaration", "constant_declaration":
			fieldType := ""
			if typeNode := node.ChildByFieldName("type"); typeNode != nil {
				fieldType = nodeText(source, typeNode)
			}
			for j := uint(0); j < node.NamedChildCount(); j++ {
				declarator := node.NamedChild(j)
				if declarator == nil || declarator.Kind() != "variable_declarator" {
					continue
				}
				nameNode := declarator.ChildByFieldName("name")
				if nameNode == nil {
					continue
				}
				members = append(members, MemberInfo{Name: nodeText(source, nameNode), Kind: "field", Type: fieldType, Static: hasModifier(source, node, "static"), Range: nodeRange(source, declarator), Selection: nodeRange(source, nameNode)})
			}
		case "method_declaration", "constructor_declaration", "annotation_type_element_declaration":
			nameNode := node.ChildByFieldName("name")
			if nameNode == nil {
				continue
			}
			name := nodeText(source, nameNode)
			returnType := ""
			if typeNode := node.ChildByFieldName("type"); typeNode != nil {
				returnType = nodeText(source, typeNode)
			}
			if node.Kind() == "constructor_declaration" {
				returnType = ""
			}
			parameters := parseParameters(source, node.ChildByFieldName("parameters"))
			locals := parseLocals(source, node.ChildByFieldName("body"))
			parts := make([]string, 0, len(parameters))
			for _, parameter := range parameters {
				parts = append(parts, parameter.Type+" "+parameter.Name)
			}
			signature := name + "(" + strings.Join(parts, ", ") + ")"
			if returnType != "" {
				signature = returnType + " " + signature
			}
			members = append(members, MemberInfo{Name: name, Kind: "method", Type: returnType, Signature: signature, Parameters: parameters, Locals: locals, Static: hasModifier(source, node, "static"), Range: nodeRange(source, node), Selection: nodeRange(source, nameNode)})
		case "enum_constant":
			nameNode := node.ChildByFieldName("name")
			if nameNode != nil {
				members = append(members, MemberInfo{Name: nodeText(source, nameNode), Kind: "enumConstant", Range: nodeRange(source, node), Selection: nodeRange(source, nameNode)})
			}
		}
	}
	return members
}

func parseLocals(source []byte, body *ts.Node) []VariableInfo {
	if body == nil {
		return nil
	}
	var variables []VariableInfo
	var walk func(*ts.Node, *ts.Node)
	walk = func(node, scope *ts.Node) {
		if node == nil {
			return
		}
		if node.Kind() == "block" || node.Kind() == "for_statement" || node.Kind() == "enhanced_for_statement" || node.Kind() == "catch_clause" {
			scope = node
		}
		if node.Kind() == "local_variable_declaration" {
			typeNode := node.ChildByFieldName("type")
			if typeNode != nil {
				for i := uint(0); i < node.NamedChildCount(); i++ {
					declarator := node.NamedChild(i)
					if declarator == nil || declarator.Kind() != "variable_declarator" {
						continue
					}
					nameNode := declarator.ChildByFieldName("name")
					if nameNode == nil {
						continue
					}
					scopeRange := nodeRange(source, body)
					if scope != nil {
						scopeRange = nodeRange(source, scope)
					}
					variables = append(variables, VariableInfo{Name: nodeText(source, nameNode), Type: nodeText(source, typeNode), Range: nodeRange(source, nameNode), Scope: scopeRange})
				}
			}
		}
		for i := uint(0); i < node.NamedChildCount(); i++ {
			walk(node.NamedChild(i), scope)
		}
	}
	walk(body, body)
	return variables
}

func parseParameters(source []byte, node *ts.Node) []ParameterInfo {
	if node == nil {
		return nil
	}
	var parameters []ParameterInfo
	for i := uint(0); i < node.NamedChildCount(); i++ {
		child := node.NamedChild(i)
		if child == nil || child.Kind() != "formal_parameter" && child.Kind() != "spread_parameter" {
			continue
		}
		nameNode := child.ChildByFieldName("name")
		typeNode := child.ChildByFieldName("type")
		if nameNode == nil || typeNode == nil {
			continue
		}
		parameters = append(parameters, ParameterInfo{Name: nodeText(source, nameNode), Type: nodeText(source, typeNode)})
	}
	return parameters
}

func hasModifier(source []byte, node *ts.Node, modifier string) bool {
	for i := uint(0); i < node.NamedChildCount(); i++ {
		child := node.NamedChild(i)
		if child != nil && child.Kind() == "modifiers" && strings.Contains(" "+nodeText(source, child)+" ", " "+modifier+" ") {
			return true
		}
	}
	return false
}

func nodeText(source []byte, node *ts.Node) string {
	if node == nil {
		return ""
	}
	return node.Utf8Text(source)
}

func nodeRange(source []byte, node *ts.Node) Range {
	return Range{Start: pointPosition(source, node.StartPosition()), End: pointPosition(source, node.EndPosition())}
}

func pointPosition(source []byte, point ts.Point) Position {
	row := int(point.Row)
	byteColumn := int(point.Column)
	lineStart := 0
	for line := 0; line < row; line++ {
		newline := strings.IndexByte(string(source[lineStart:]), '\n')
		if newline < 0 {
			lineStart = len(source)
			break
		}
		lineStart += newline + 1
	}
	lineEnd := lineStart + byteColumn
	if lineEnd > len(source) {
		lineEnd = len(source)
	}
	if lineStart > len(source) {
		lineStart = len(source)
	}
	characters := 0
	for _, r := range string(source[lineStart:lineEnd]) {
		if r > 0xffff {
			characters += 2
		} else {
			characters++
		}
	}
	return Position{Line: row, Character: characters}
}
