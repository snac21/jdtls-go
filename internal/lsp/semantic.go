package lsp

import (
	"strings"

	"github.com/cage/jdtls-go/internal/classfile"
	javaparser "github.com/cage/jdtls-go/internal/java"
)

type resolvedType struct {
	Name   string
	Source *javaparser.TypeInfo
	Binary *classfile.Type
}

func resolveSemanticType(units map[string]javaparser.Unit, binaries *classfile.Index, unit javaparser.Unit, name string) (resolvedType, bool) {
	name = baseJavaType(name)
	if name == "" || isPrimitiveType(name) {
		return resolvedType{}, false
	}
	candidates := []string{}
	if strings.Contains(name, ".") {
		candidates = append(candidates, name)
	}
	if unit.Package != "" {
		candidates = append(candidates, unit.Package+"."+name)
	}
	for _, imported := range unit.Imports {
		if strings.HasSuffix(imported, ".*") {
			candidates = append(candidates, strings.TrimSuffix(imported, ".*")+"."+name)
		} else if imported == name || strings.HasSuffix(imported, "."+name) {
			candidates = append(candidates, imported)
		}
	}
	candidates = append(candidates, "java.lang."+name)
	for _, candidate := range candidates {
		for _, sourceUnit := range units {
			for i := range sourceUnit.Types {
				if sourceUnit.Types[i].QualifiedName == candidate {
					typeInfo := sourceUnit.Types[i]
					return resolvedType{Name: candidate, Source: &typeInfo}, true
				}
			}
		}
		if binaries != nil {
			if binary, ok := binaries.Types[candidate]; ok {
				return resolvedType{Name: candidate, Binary: &binary}, true
			}
		}
	}
	var found resolvedType
	for _, sourceUnit := range units {
		for i := range sourceUnit.Types {
			typeInfo := sourceUnit.Types[i]
			if typeInfo.Name != name && !strings.HasSuffix(typeInfo.QualifiedName, "$"+name) {
				continue
			}
			if found.Name != "" {
				return resolvedType{}, false
			}
			found = resolvedType{Name: typeInfo.QualifiedName, Source: &typeInfo}
		}
	}
	if binaries != nil {
		for _, binary := range binaries.Types {
			if !strings.HasSuffix(binary.Name, "."+name) && binary.Name != name {
				continue
			}
			if found.Name != "" {
				return resolvedType{}, false
			}
			copy := binary
			found = resolvedType{Name: binary.Name, Binary: &copy}
		}
	}
	return found, found.Name != ""
}

func baseJavaType(typeName string) string {
	typeName = strings.TrimSpace(typeName)
	for strings.HasPrefix(typeName, "@") {
		space := strings.IndexAny(typeName, " \t")
		if space < 0 {
			return ""
		}
		typeName = strings.TrimSpace(typeName[space:])
	}
	if typeName == "?" || strings.HasPrefix(typeName, "?") {
		if index := strings.Index(typeName, "extends"); index >= 0 {
			typeName = strings.TrimSpace(typeName[index+len("extends"):])
		} else if index := strings.Index(typeName, "super"); index >= 0 {
			typeName = strings.TrimSpace(typeName[index+len("super"):])
		} else {
			return ""
		}
	}
	if generic := strings.IndexByte(typeName, '<'); generic >= 0 {
		typeName = typeName[:generic]
	}
	typeName = strings.TrimSpace(strings.TrimSuffix(typeName, "..."))
	for strings.HasSuffix(typeName, "[]") {
		typeName = strings.TrimSpace(strings.TrimSuffix(typeName, "[]"))
	}
	return strings.TrimSpace(typeName)
}

func isPrimitiveType(name string) bool {
	switch name {
	case "boolean", "byte", "char", "short", "int", "long", "float", "double", "void", "var":
		return true
	default:
		return false
	}
}

func sourceTypeAt(unit javaparser.Unit, pos position) *javaparser.TypeInfo {
	var found *javaparser.TypeInfo
	for i := range unit.Types {
		typeInfo := &unit.Types[i]
		if pos.Line < typeInfo.Range.Start.Line || pos.Line > typeInfo.Range.End.Line {
			continue
		}
		if found == nil || typeInfo.Range.End.Line-typeInfo.Range.Start.Line <= found.Range.End.Line-found.Range.Start.Line {
			found = typeInfo
		}
	}
	return found
}

func sourceTypeMember(typeInfo javaparser.TypeInfo, name string) (javaparser.MemberInfo, bool) {
	for _, member := range typeInfo.Members {
		if member.Name == name {
			return member, true
		}
	}
	return javaparser.MemberInfo{}, false
}

func binaryMemberType(member classfile.Member) string {
	descriptor := member.Descriptor
	if strings.HasPrefix(descriptor, "(") {
		if closing := strings.IndexByte(descriptor, ')'); closing >= 0 && closing+1 < len(descriptor) {
			descriptor = descriptor[closing+1:]
		}
	}
	return javaDescriptorType(descriptor)
}

func javaDescriptorType(descriptor string) string {
	array := strings.HasPrefix(descriptor, "[")
	descriptor = strings.TrimLeft(descriptor, "[")
	var name string
	if len(descriptor) == 0 {
		return ""
	}
	switch descriptor[0] {
	case 'Z':
		name = "boolean"
	case 'B':
		name = "byte"
	case 'C':
		name = "char"
	case 'S':
		name = "short"
	case 'I':
		name = "int"
	case 'J':
		name = "long"
	case 'F':
		name = "float"
	case 'D':
		name = "double"
	case 'V':
		name = "void"
	case 'L':
		name = strings.ReplaceAll(strings.TrimSuffix(strings.TrimPrefix(descriptor, "L"), ";"), "/", ".")
	default:
		return ""
	}
	if array {
		name += "[]"
	}
	return name
}

func (s *server) completeMembers(uri string, pos position, qualifier, prefix string) []map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	unit, ok := s.units[uri]
	if !ok {
		return nil
	}
	current := sourceTypeAt(unit, pos)
	resolved, ok := s.resolveReceiverType(unit, current, pos, qualifier)
	if !ok {
		return nil
	}
	seenTypes := map[string]bool{}
	seenMembers := map[string]bool{}
	items := make([]map[string]any, 0)
	var visit func(resolvedType)
	visit = func(value resolvedType) {
		if value.Name == "" || seenTypes[value.Name] {
			return
		}
		seenTypes[value.Name] = true
		if value.Source != nil {
			for _, member := range value.Source.Members {
				if !strings.HasPrefix(member.Name, prefix) || seenMembers[member.Name] {
					continue
				}
				seenMembers[member.Name] = true
				kind := 5
				if member.Kind == "method" {
					kind = 2
				}
				items = append(items, map[string]any{"label": member.Name, "kind": kind, "detail": memberSignature(member), "insertText": member.Name})
			}
			for _, parent := range append([]string{value.Source.Super}, value.Source.Interfaces...) {
				if parent == "" {
					continue
				}
				if parentType, found := resolveSemanticType(s.units, s.typeIndex, unit, parent); found {
					visit(parentType)
				}
			}
			return
		}
		if value.Binary == nil {
			return
		}
		for _, member := range append(append([]classfile.Member{}, value.Binary.Fields...), value.Binary.Methods...) {
			if !strings.HasPrefix(member.Name, prefix) || seenMembers[member.Name] || member.Name == "<init>" || member.Name == "<clinit>" {
				continue
			}
			seenMembers[member.Name] = true
			kind := 5
			if strings.HasPrefix(member.Descriptor, "(") {
				kind = 2
			}
			items = append(items, map[string]any{"label": member.Name, "kind": kind, "detail": binaryMemberType(member) + " " + member.Name})
		}
		parents := append([]string{value.Binary.Super}, value.Binary.Interfaces...)
		for _, parent := range parents {
			if parentType, found := resolveSemanticType(s.units, s.typeIndex, unit, parent); found {
				visit(parentType)
			}
		}
	}
	visit(resolved)
	return items
}

func (s *server) resolveReceiverType(unit javaparser.Unit, current *javaparser.TypeInfo, pos position, expression string) (resolvedType, bool) {
	parts := strings.Split(expression, ".")
	if len(parts) == 0 || parts[0] == "" {
		return resolvedType{}, false
	}
	var resolved resolvedType
	var ok bool
	first := parts[0]
	switch first {
	case "this":
		if current == nil {
			return resolvedType{}, false
		}
		resolved, ok = resolveSemanticType(s.units, s.typeIndex, unit, current.QualifiedName)
	case "super":
		if current == nil {
			return resolvedType{}, false
		}
		resolved, ok = resolveSemanticType(s.units, s.typeIndex, unit, current.Super)
	default:
		if current != nil {
			if field, found := sourceTypeMember(*current, first); found && field.Kind == "field" {
				resolved, ok = resolveSemanticType(s.units, s.typeIndex, unit, field.Type)
			}
			if !ok {
				for _, method := range current.Members {
					if method.Kind != "method" || pos.Line < method.Range.Start.Line || pos.Line > method.Range.End.Line {
						continue
					}
					for _, parameter := range method.Parameters {
						if parameter.Name == first {
							resolved, ok = resolveSemanticType(s.units, s.typeIndex, unit, parameter.Type)
							break
						}
					}
					if ok {
						break
					}
					var selected *javaparser.VariableInfo
					for i := range method.Locals {
						variable := &method.Locals[i]
						if variable.Name != first || pos.Line < variable.Scope.Start.Line || pos.Line > variable.Scope.End.Line || pos.Line < variable.Range.Start.Line {
							continue
						}
						if selected == nil || variable.Range.Start.Line > selected.Range.Start.Line {
							selected = variable
						}
					}
					if selected != nil {
						resolved, ok = resolveSemanticType(s.units, s.typeIndex, unit, selected.Type)
					}
				}
			}
		}
		if !ok {
			resolved, ok = resolveSemanticType(s.units, s.typeIndex, unit, first)
		}
	}
	if !ok {
		return resolvedType{}, false
	}
	for _, memberName := range parts[1:] {
		member, found := s.lookupMember(unit, resolved, memberName, map[string]bool{})
		if !found || member.Kind != "field" {
			return resolvedType{}, false
		}
		resolved, ok = resolveSemanticType(s.units, s.typeIndex, unit, member.Type)
		if !ok {
			return resolvedType{}, false
		}
	}
	return resolved, true
}

func (s *server) lookupMember(unit javaparser.Unit, value resolvedType, name string, seen map[string]bool) (javaparser.MemberInfo, bool) {
	if value.Name == "" || seen[value.Name] {
		return javaparser.MemberInfo{}, false
	}
	seen[value.Name] = true
	if value.Source != nil {
		if member, ok := sourceTypeMember(*value.Source, name); ok {
			return member, true
		}
		for _, parent := range append([]string{value.Source.Super}, value.Source.Interfaces...) {
			if parentType, ok := resolveSemanticType(s.units, s.typeIndex, unit, parent); ok {
				if member, found := s.lookupMember(unit, parentType, name, seen); found {
					return member, true
				}
			}
		}
	}
	if value.Binary != nil {
		for _, member := range value.Binary.Fields {
			if member.Name == name {
				return javaparser.MemberInfo{Name: member.Name, Kind: "field", Type: binaryMemberType(member)}, true
			}
		}
		for _, member := range value.Binary.Methods {
			if member.Name == name {
				return javaparser.MemberInfo{Name: member.Name, Kind: "method", Type: binaryMemberType(member), Signature: binaryMemberType(member) + " " + member.Name}, true
			}
		}
		for _, parent := range append([]string{value.Binary.Super}, value.Binary.Interfaces...) {
			if parentType, ok := resolveSemanticType(s.units, s.typeIndex, unit, parent); ok {
				if member, found := s.lookupMember(unit, parentType, name, seen); found {
					return member, true
				}
			}
		}
	}
	return javaparser.MemberInfo{}, false
}

func memberSignature(member javaparser.MemberInfo) string {
	if member.Signature != "" {
		return member.Signature
	}
	return strings.TrimSpace(member.Type + " " + member.Name)
}
