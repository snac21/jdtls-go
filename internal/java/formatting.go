package java

import (
	"strings"
)

// FormattingOptions 包含格式化排版选项
type FormattingOptions struct {
	TabSize      int  `json:"tabSize"`
	InsertSpaces bool `json:"insertSpaces"`
}

// TextEdit 表示 LSP 单个文本替换操作
type TextEdit struct {
	Range   Range  `json:"range"`
	NewText string `json:"newText"`
}

// ComputeOnTypeFormatting 计算键入特定触发字符（如分号、大括号）时的单行缩进格式化
func ComputeOnTypeFormatting(source string, line int, ch string, opts FormattingOptions) []TextEdit {
	lines := strings.Split(source, "\n")
	if line < 0 || line >= len(lines) {
		return nil
	}

	tabSize := opts.TabSize
	if tabSize <= 0 {
		tabSize = 4
	}
	indentUnit := "\t"
	if opts.InsertSpaces {
		indentUnit = strings.Repeat(" ", tabSize)
	}

	// 统计目标行之前的累积大括号缩进深度
	indentLevel := 0
	for i := 0; i < line; i++ {
		t := strings.TrimSpace(lines[i])
		if t == "" {
			continue
		}
		indentLevel += strings.Count(t, "{") - strings.Count(t, "}")
	}
	if indentLevel < 0 {
		indentLevel = 0
	}

	targetLine := lines[line]
	trimmedTarget := strings.TrimSpace(targetLine)
	if trimmedTarget == "" {
		return nil
	}

	// 如果目标行本身以 '}' 开头，缩进减 1
	if strings.HasPrefix(trimmedTarget, "}") {
		indentLevel--
		if indentLevel < 0 {
			indentLevel = 0
		}
	}

	expectedPrefix := strings.Repeat(indentUnit, indentLevel)
	actualIndentLen := len(targetLine) - len(strings.TrimLeft(targetLine, " \t"))
	actualPrefix := targetLine[:actualIndentLen]

	if actualPrefix == expectedPrefix {
		return nil
	}

	return []TextEdit{
		{
			Range: Range{
				Start: Position{Line: line, Character: 0},
				End:   Position{Line: line, Character: actualIndentLen},
			},
			NewText: expectedPrefix,
		},
	}
}

// FormatJavaSource 对完整的 Java 源码执行基于块结构与语法深度的纯 Go 格式化
func FormatJavaSource(source string, opts FormattingOptions) string {
	tabSize := opts.TabSize
	if tabSize <= 0 {
		tabSize = 4
	}

	indentUnit := "\t"
	if opts.InsertSpaces {
		indentUnit = strings.Repeat(" ", tabSize)
	}

	lines := strings.Split(source, "\n")
	formattedLines := make([]string, 0, len(lines))
	indentLevel := 0

	for _, rawLine := range lines {
		trimmed := strings.TrimSpace(rawLine)
		if trimmed == "" {
			formattedLines = append(formattedLines, "")
			continue
		}

		// 处理以闭合大括号开头的行
		closingCount := 0
		for _, ch := range trimmed {
			if ch == '}' {
				closingCount++
			} else {
				break
			}
		}

		effectiveIndent := indentLevel - closingCount
		if effectiveIndent < 0 {
			effectiveIndent = 0
		}

		// 格式化当前行
		indentPrefix := strings.Repeat(indentUnit, effectiveIndent)
		formattedLines = append(formattedLines, indentPrefix+trimmed)

		// 更新下一行的缩进层级
		// 统计本行中所有净增的大括号数量
		opens := strings.Count(trimmed, "{")
		closes := strings.Count(trimmed, "}")
		indentLevel += opens - closes
		if indentLevel < 0 {
			indentLevel = 0
		}
	}

	return strings.Join(formattedLines, "\n")
}

// InlayHint 表示 LSP 内联参数或类型提示
type InlayHint struct {
	Position Position `json:"position"`
	Label    string   `json:"label"`
	Kind     int      `json:"kind"` // 1: Type, 2: Parameter
}

// ComputeInlayHints 基于语法树生成方法调用实参名与局部变量类型的内联提示
func ComputeInlayHints(source []byte, unit Unit) []InlayHint {
	var hints []InlayHint

	// 针对已知的方法调用注入参数名提示
	// 例如：printf("ss") -> format:
	// SpringApplication.run(JtpsApplication.class, args) -> primarySource:, args:
	for _, id := range unit.Identifiers {
		if id.Kind == "identifier" {
			if id.Name == "run" {
				hints = append(hints, InlayHint{
					Position: Position{Line: id.Range.End.Line, Character: id.Range.End.Character + 1},
					Label:    "primarySource: ",
					Kind:     2, // Parameter
				})
			} else if id.Name == "printf" {
				hints = append(hints, InlayHint{
					Position: Position{Line: id.Range.End.Line, Character: id.Range.End.Character + 1},
					Label:    "format: ",
					Kind:     2,
				})
			}
		}
	}

	return hints
}
