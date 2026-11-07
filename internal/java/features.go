package java

import (
	"fmt"
	"sort"
	"strings"

	ts "github.com/tree-sitter/go-tree-sitter"
	tree_sitter_java "github.com/tree-sitter/tree-sitter-java/bindings/go"
)

// FoldingRange 表示 LSP 代码折叠区间结构
type FoldingRange struct {
	StartLine      int    `json:"startLine"`
	StartCharacter int    `json:"startCharacter,omitempty"`
	EndLine        int    `json:"endLine"`
	EndCharacter   int    `json:"endCharacter,omitempty"`
	Kind           string `json:"kind,omitempty"`
}

// SelectionRange 表示智能逐级扩展选区结构
type SelectionRange struct {
	Range  Range           `json:"range"`
	Parent *SelectionRange `json:"parent,omitempty"`
}

// 预定义的语义高亮 TokenTypes 与 Modifiers
var SemanticTokenTypes = []string{
	"type",        // 0
	"class",       // 1
	"enum",        // 2
	"interface",   // 3
	"parameter",   // 4
	"variable",    // 5
	"property",    // 6
	"function",    // 7
	"method",      // 8
	"annotation",  // 9
	"keyword",     // 10
	"modifier",    // 11
}

var SemanticTokenModifiers = []string{
	"declaration", // 1 << 0
	"static",      // 1 << 1
	"definition",  // 1 << 2
}

type rawToken struct {
	line      int
	char      int
	length    int
	tokenType int
	modifiers int
}

// ComputeFoldingRanges 分析源码并提取所有可折叠的代码块（类、方法、导入块、注释）
func ComputeFoldingRanges(source []byte) ([]FoldingRange, error) {
	parser := ts.NewParser()
	defer parser.Close()
	language := ts.NewLanguage(tree_sitter_java.Language())
	if err := parser.SetLanguage(language); err != nil {
		return nil, err
	}
	tree := parser.Parse(source, nil)
	if tree == nil {
		return nil, fmt.Errorf("failed to parse syntax tree")
	}
	defer tree.Close()

	root := tree.RootNode()
	if root == nil {
		return nil, nil
	}

	var ranges []FoldingRange
	var firstImportLine = -1
	var lastImportLine = -1

	var walk func(*ts.Node)
	walk = func(node *ts.Node) {
		if node == nil {
			return
		}
		kind := node.Kind()

		// 处理 import 连续块折叠
		if kind == "import_declaration" {
			start := int(node.StartPosition().Row)
			end := int(node.EndPosition().Row)
			if firstImportLine == -1 {
				firstImportLine = start
			}
			lastImportLine = end
		} else if firstImportLine != -1 && lastImportLine > firstImportLine {
			ranges = append(ranges, FoldingRange{
				StartLine: firstImportLine,
				EndLine:   lastImportLine,
				Kind:      "imports",
			})
			firstImportLine = -1
			lastImportLine = -1
		}

		// 多行注释折叠
		if kind == "block_comment" {
			start := int(node.StartPosition().Row)
			end := int(node.EndPosition().Row)
			if end > start {
				ranges = append(ranges, FoldingRange{
					StartLine: start,
					EndLine:   end,
					Kind:      "comment",
				})
			}
		}

		// 类、接口、枚举体以及方法体折叠
		switch kind {
		case "class_body", "interface_body", "enum_body", "constructor_body", "block":
			start := int(node.StartPosition().Row)
			end := int(node.EndPosition().Row)
			if end > start {
				ranges = append(ranges, FoldingRange{
					StartLine: start,
					EndLine:   end,
					Kind:      "region",
				})
			}
		}

		for i := uint(0); i < node.NamedChildCount(); i++ {
			walk(node.NamedChild(i))
		}
	}

	walk(root)

	// 收尾未闭合的 import 块
	if firstImportLine != -1 && lastImportLine > firstImportLine {
		ranges = append(ranges, FoldingRange{
			StartLine: firstImportLine,
			EndLine:   lastImportLine,
			Kind:      "imports",
		})
	}

	return ranges, nil
}

// ComputeSelectionRanges 针对给定的一组位置，基于语法树层次构建嵌套的 SelectionRange
func ComputeSelectionRanges(source []byte, positions []Position) ([]SelectionRange, error) {
	parser := ts.NewParser()
	defer parser.Close()
	language := ts.NewLanguage(tree_sitter_java.Language())
	if err := parser.SetLanguage(language); err != nil {
		return nil, err
	}
	tree := parser.Parse(source, nil)
	if tree == nil {
		return nil, fmt.Errorf("failed to parse syntax tree")
	}
	defer tree.Close()

	root := tree.RootNode()
	if root == nil {
		return nil, nil
	}

	results := make([]SelectionRange, 0, len(positions))
	for _, pos := range positions {
		// 寻找包含此位置的最小节点
		p := ts.Point{Row: uint(pos.Line), Column: uint(pos.Character)}
		curr := root.NamedDescendantForPointRange(p, p)
		if curr == nil {
			curr = root
		}

		var currentSel *SelectionRange
		for curr != nil {
			rng := nodeRange(source, curr)
			if currentSel == nil || (currentSel.Range.Start != rng.Start || currentSel.Range.End != rng.End) {
				newSel := &SelectionRange{
					Range:  rng,
					Parent: currentSel,
				}
				currentSel = newSel
			}
			curr = curr.Parent()
		}

		if currentSel != nil {
			// 将由外到内的链反转为由内到外（内层在前，外层为 Parent）
			results = append(results, invertSelectionRange(currentSel))
		}
	}

	return results, nil
}

func invertSelectionRange(outer *SelectionRange) SelectionRange {
	var list []*SelectionRange
	curr := outer
	for curr != nil {
		list = append(list, curr)
		curr = curr.Parent
	}

	var root *SelectionRange
	for i := len(list) - 1; i >= 0; i-- {
		node := &SelectionRange{
			Range:  list[i].Range,
			Parent: root,
		}
		root = node
	}
	if root != nil {
		return *root
	}
	return SelectionRange{}
}

// ComputeSemanticTokens 遍历语法树并生成符合 LSP 3.17 规范的 5-tuple 增量编码数组
func ComputeSemanticTokens(source []byte) ([]uint32, error) {
	parser := ts.NewParser()
	defer parser.Close()
	language := ts.NewLanguage(tree_sitter_java.Language())
	if err := parser.SetLanguage(language); err != nil {
		return nil, err
	}
	tree := parser.Parse(source, nil)
	if tree == nil {
		return nil, fmt.Errorf("failed to parse syntax tree")
	}
	defer tree.Close()

	root := tree.RootNode()
	if root == nil {
		return []uint32{}, nil
	}

	var tokens []rawToken
	var walk func(*ts.Node)
	walk = func(node *ts.Node) {
		if node == nil {
			return
		}
		kind := node.Kind()

		// 注解
		if kind == "marker_annotation" || kind == "annotation" {
			if name := node.ChildByFieldName("name"); name != nil {
				pos := pointPosition(source, name.StartPosition())
				tokens = append(tokens, rawToken{
					line:      pos.Line,
					char:      pos.Character,
					length:    int(name.EndByte() - name.StartByte()),
					tokenType: 9, // annotation
					modifiers: 0,
				})
			}
		}

		// 类、接口、枚举声明
		if kind == "class_declaration" || kind == "record_declaration" {
			if name := node.ChildByFieldName("name"); name != nil {
				pos := pointPosition(source, name.StartPosition())
				tokens = append(tokens, rawToken{
					line:      pos.Line,
					char:      pos.Character,
					length:    int(name.EndByte() - name.StartByte()),
					tokenType: 1, // class
					modifiers: 1, // declaration
				})
			}
		} else if kind == "interface_declaration" {
			if name := node.ChildByFieldName("name"); name != nil {
				pos := pointPosition(source, name.StartPosition())
				tokens = append(tokens, rawToken{
					line:      pos.Line,
					char:      pos.Character,
					length:    int(name.EndByte() - name.StartByte()),
					tokenType: 3, // interface
					modifiers: 1,
				})
			}
		}

		// 方法声明与调用
		if kind == "method_declaration" {
			if name := node.ChildByFieldName("name"); name != nil {
				pos := pointPosition(source, name.StartPosition())
				tokens = append(tokens, rawToken{
					line:      pos.Line,
					char:      pos.Character,
					length:    int(name.EndByte() - name.StartByte()),
					tokenType: 8, // method
					modifiers: 1,
				})
			}
		} else if kind == "method_invocation" {
			if name := node.ChildByFieldName("name"); name != nil {
				pos := pointPosition(source, name.StartPosition())
				tokens = append(tokens, rawToken{
					line:      pos.Line,
					char:      pos.Character,
					length:    int(name.EndByte() - name.StartByte()),
					tokenType: 8, // method
					modifiers: 0,
				})
			}
		}

		// 字段与形参
		if kind == "formal_parameter" {
			if name := node.ChildByFieldName("name"); name != nil {
				pos := pointPosition(source, name.StartPosition())
				tokens = append(tokens, rawToken{
					line:      pos.Line,
					char:      pos.Character,
					length:    int(name.EndByte() - name.StartByte()),
					tokenType: 4, // parameter
					modifiers: 1,
				})
			}
		} else if kind == "field_declaration" {
			if decl := node.ChildByFieldName("declarator"); decl != nil {
				if name := decl.ChildByFieldName("name"); name != nil {
					pos := pointPosition(source, name.StartPosition())
					tokens = append(tokens, rawToken{
						line:      pos.Line,
						char:      pos.Character,
						length:    int(name.EndByte() - name.StartByte()),
						tokenType: 6, // property
						modifiers: 1,
					})
				}
			}
		}

		for i := uint(0); i < node.NamedChildCount(); i++ {
			walk(node.NamedChild(i))
		}
	}

	walk(root)

	// 按行和字符升序排序
	sort.Slice(tokens, func(i, j int) bool {
		if tokens[i].line != tokens[j].line {
			return tokens[i].line < tokens[j].line
		}
		return tokens[i].char < tokens[j].char
	})

	// 转换为相对增量编码的 5-tuple
	data := make([]uint32, 0, len(tokens)*5)
	lastLine := 0
	lastChar := 0

	for _, tok := range tokens {
		deltaLine := tok.line - lastLine
		deltaChar := tok.char
		if deltaLine == 0 {
			deltaChar = tok.char - lastChar
		}

		data = append(data, uint32(deltaLine), uint32(deltaChar), uint32(tok.length), uint32(tok.tokenType), uint32(tok.modifiers))
		lastLine = tok.line
		lastChar = tok.char
	}

	return data, nil
}

// FindFullyQualifiedNameAt 给定光标行列号，返回该位置所在符号的 Java 全限定名
func (u Unit) FindFullyQualifiedNameAt(line, char int) string {
	pos := Position{Line: line, Character: char}

	// 优先在成员中查找（方法或字段）
	for _, t := range u.Types {
		for _, m := range t.Members {
			if containsPosition(m.Range, pos) || containsPosition(m.Selection, pos) {
				return t.QualifiedName + "#" + m.Name
			}
		}
	}

	// 其次在类类型中查找
	for _, t := range u.Types {
		if containsPosition(t.Range, pos) || containsPosition(t.Selection, pos) {
			return t.QualifiedName
		}
	}

	// 再次检查 identifiers
	for _, id := range u.Identifiers {
		if containsPosition(id.Range, pos) {
			if u.Package != "" {
				return u.Package + "." + id.Name
			}
			return id.Name
		}
	}

	return ""
}

func containsPosition(r Range, p Position) bool {
	if p.Line < r.Start.Line || p.Line > r.End.Line {
		return false
	}
	if p.Line == r.Start.Line && p.Character < r.Start.Character {
		return false
	}
	if p.Line == r.End.Line && p.Character > r.End.Character {
		return false
	}
	return true
}

// OrganizeImportsText 基于当前已解析的语法单元，自动剔除未使用的 import 并按字母序重新整理
func (u Unit) OrganizeImportsText(source string) (string, error) {
	if len(u.Imports) == 0 {
		return source, nil
	}

	// 统计正文引用的顶级简单类名
	usedIdentSet := make(map[string]bool)
	for _, id := range u.Identifiers {
		usedIdentSet[id.Name] = true
	}

	// 过滤保留有引用的 import
	var keptImports []string
	seen := make(map[string]bool)
	for _, imp := range u.Imports {
		parts := strings.Split(imp, ".")
		simpleName := parts[len(parts)-1]
		// 通配符或者被显式引用的导入保留
		if simpleName == "*" || usedIdentSet[simpleName] {
			if !seen[imp] {
				seen[imp] = true
				keptImports = append(keptImports, imp)
			}
		}
	}

	sort.Strings(keptImports)

	// 重构 import 块
	var importBlockBuilder strings.Builder
	for _, imp := range keptImports {
		importBlockBuilder.WriteString("import " + imp + ";\n")
	}
	newImportBlock := importBlockBuilder.String()

	// 替换源码中的原有 import 区域
	lines := strings.Split(source, "\n")
	firstImport := -1
	lastImport := -1
	for idx, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "import ") {
			if firstImport == -1 {
				firstImport = idx
			}
			lastImport = idx
		}
	}

	if firstImport == -1 {
		return source, nil
	}

	var finalLines []string
	finalLines = append(finalLines, lines[:firstImport]...)
	if newImportBlock != "" {
		finalLines = append(finalLines, strings.TrimRight(newImportBlock, "\n"))
	}
	if lastImport+1 < len(lines) {
		finalLines = append(finalLines, lines[lastImport+1:]...)
	}

	return strings.Join(finalLines, "\n"), nil
}

// CodeLens 表示 LSP 代码镜头结构
type CodeLens struct {
	Range   Range           `json:"range"`
	Command CodeLensCommand `json:"command,omitempty"`
}

// CodeLensCommand 表示代码镜头触发的命令
type CodeLensCommand struct {
	Title     string        `json:"title"`
	Command   string        `json:"command"`
	Arguments []interface{} `json:"arguments,omitempty"`
}

// ComputeCodeLenses 扫描 Unit 中的 main 方法与核心方法，生成 Run | Debug 等 CodeLens
func ComputeCodeLenses(uri string, unit Unit) []CodeLens {
	var lenses []CodeLens
	for _, t := range unit.Types {
		for _, m := range t.Members {
			// 检测 main 方法
			if m.Name == "main" && m.Static {
				lenses = append(lenses, CodeLens{
					Range: m.Range,
					Command: CodeLensCommand{
						Title:     "▶ Run",
						Command:   "java.dap.run",
						Arguments: []interface{}{uri, t.QualifiedName, m.Name},
					},
				})
				lenses = append(lenses, CodeLens{
					Range: m.Range,
					Command: CodeLensCommand{
						Title:     "Debug",
						Command:   "java.dap.debug",
						Arguments: []interface{}{uri, t.QualifiedName, m.Name},
					},
				})
			} else if m.Kind == "method" {
				// 普通方法展示引用导航镜头
				lenses = append(lenses, CodeLens{
					Range: m.Selection,
					Command: CodeLensCommand{
						Title:     fmt.Sprintf("%s References", m.Name),
						Command:   "editor.action.findReferences",
						Arguments: []interface{}{uri, m.Selection.Start},
					},
				})
			}
		}
	}
	return lenses
}

// HierarchicalDocumentSymbol 表示 LSP 3.16+ 树形文档符号结构
type HierarchicalDocumentSymbol struct {
	Name           string                       `json:"name"`
	Detail         string                       `json:"detail,omitempty"`
	Kind           int                          `json:"kind"`
	Range          Range                        `json:"range"`
	SelectionRange Range                        `json:"selectionRange"`
	Children       []HierarchicalDocumentSymbol `json:"children,omitempty"`
}

// ExtractHierarchicalSymbols 递归提取当前文件的树状结构符号
func ExtractHierarchicalSymbols(unit Unit) []HierarchicalDocumentSymbol {
	var roots []HierarchicalDocumentSymbol
	for _, t := range unit.Types {
		sym := HierarchicalDocumentSymbol{
			Name:           t.Name,
			Detail:         t.QualifiedName,
			Kind:           5, // Class
			Range:          t.Range,
			SelectionRange: t.Selection,
			Children:       make([]HierarchicalDocumentSymbol, 0),
		}
		for _, m := range t.Members {
			mKind := 6 // Method
			if m.Kind == "field" {
				mKind = 8 // Field
			} else if m.Kind == "constructor" {
				mKind = 9 // Constructor
			}
			sym.Children = append(sym.Children, HierarchicalDocumentSymbol{
				Name:           m.Name,
				Detail:         m.Signature,
				Kind:           mKind,
				Range:          m.Range,
				SelectionRange: m.Selection,
			})
		}
		roots = append(roots, sym)
	}
	return roots
}

// StackTraceLocation 表示堆栈行解析后的源文件与位置信息
type StackTraceLocation struct {
	URI       string `json:"uri"`
	Line      int    `json:"line"`
	ClassName string `json:"className"`
	Method    string `json:"method"`
}

// ResolveStackTraceLine 解析类似 "at com.example.jtps.HelloController.hello(HelloController.java:12)" 的堆栈行
func ResolveStackTraceLine(line string, rootDir string, units map[string]Unit) *StackTraceLocation {
	trimmed := strings.TrimSpace(line)
	trimmed = strings.TrimPrefix(trimmed, "at ")

	parenOpen := strings.Index(trimmed, "(")
	parenClose := strings.LastIndex(trimmed, ")")
	if parenOpen == -1 || parenClose == -1 || parenClose <= parenOpen {
		return nil
	}

	fullMethod := strings.TrimSpace(trimmed[:parenOpen])
	fileAndLine := trimmed[parenOpen+1 : parenClose]

	parts := strings.Split(fileAndLine, ":")
	if len(parts) != 2 {
		return nil
	}
	fileName := parts[0]
	var lineNum int
	fmt.Sscanf(parts[1], "%d", &lineNum)

	dotIdx := strings.LastIndex(fullMethod, ".")
	className := ""
	methodName := ""
	if dotIdx != -1 {
		className = fullMethod[:dotIdx]
		methodName = fullMethod[dotIdx+1:]
	} else {
		className = fullMethod
	}

	matchedURI := ""
	for uri, u := range units {
		if strings.HasSuffix(uri, "/"+fileName) {
			matchedURI = uri
			break
		}
		for _, t := range u.Types {
			if t.QualifiedName == className {
				matchedURI = uri
				break
			}
		}
		if matchedURI != "" {
			break
		}
	}

	return &StackTraceLocation{
		URI:       matchedURI,
		Line:      lineNum - 1,
		ClassName: className,
		Method:    methodName,
	}
}

// GenerateModuleInfo 为 Maven 模块生成 module-info.java 模板
func GenerateModuleInfo(moduleName string, packages []string) string {
	if moduleName == "" {
		moduleName = "app"
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("module %s {\n", moduleName))
	for _, pkg := range packages {
		if pkg != "" {
			sb.WriteString(fmt.Sprintf("    exports %s;\n", pkg))
		}
	}
	sb.WriteString("}\n")
	return sb.String()
}

// PasteEditResult 描述粘贴处理结果
type PasteEditResult struct {
	AddedImports []string   `json:"addedImports"`
	TextEdits    []TextEdit `json:"textEdits"`
}

// AnalyzePasteImports 分析粘贴文本中引用的潜在类型并生成 import 插入 TextEdit
func AnalyzePasteImports(pastedText string, existingImports []string) PasteEditResult {
	tokens := strings.FieldsFunc(pastedText, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_')
	})

	existingSet := make(map[string]bool)
	for _, imp := range existingImports {
		parts := strings.Split(imp, ".")
		existingSet[parts[len(parts)-1]] = true
	}

	commonPackages := map[string]string{
		"List":           "java.util.List",
		"Map":            "java.util.Map",
		"Set":            "java.util.Set",
		"ArrayList":      "java.util.ArrayList",
		"HashMap":        "java.util.HashMap",
		"Autowired":      "org.springframework.beans.factory.annotation.Autowired",
		"RestController": "org.springframework.web.bind.annotation.RestController",
		"GetMapping":     "org.springframework.web.bind.annotation.GetMapping",
		"PostMapping":    "org.springframework.web.bind.annotation.PostMapping",
		"RequestParam":   "org.springframework.web.bind.annotation.RequestParam",
	}

	var needed []string
	addedSet := make(map[string]bool)
	for _, tok := range tokens {
		if full, ok := commonPackages[tok]; ok {
			if !existingSet[tok] && !addedSet[full] {
				addedSet[full] = true
				needed = append(needed, full)
			}
		}
	}

	sort.Strings(needed)
	var edits []TextEdit
	if len(needed) > 0 {
		var insertText strings.Builder
		for _, imp := range needed {
			insertText.WriteString(fmt.Sprintf("import %s;\n", imp))
		}
		edits = append(edits, TextEdit{
			Range:   Range{Start: Position{Line: 2, Character: 0}, End: Position{Line: 2, Character: 0}},
			NewText: insertText.String(),
		})
	}

	return PasteEditResult{
		AddedImports: needed,
		TextEdits:    edits,
	}
}

// HoverResult 表示 LSP 悬停文档应答结构
type HoverResult struct {
	Contents map[string]string `json:"contents"`
	Range    Range             `json:"range"`
}

// ComputeDocHover 结合 AST 结构与源码注释，为光标所在符号生成富文本 Markdown 悬停卡片
func ComputeDocHover(source string, unit Unit, line int, character int) *HoverResult {
	pos := Position{Line: line, Character: character}

	lines := strings.Split(source, "\n")

	// 查找方法/成员
	for _, t := range unit.Types {
		for _, m := range t.Members {
			if containsPosition(m.Range, pos) || containsPosition(m.Selection, pos) {
				sig := m.Signature
				if sig == "" {
					sig = fmt.Sprintf("%s %s()", m.Type, m.Name)
				}
				// 提取方法上方的 javadoc 注释
				var docComments []string
				startLine := m.Range.Start.Line
				for i := startLine - 1; i >= 0 && i >= startLine-5; i-- {
					if i < len(lines) {
						l := strings.TrimSpace(lines[i])
						if strings.HasPrefix(l, "/**") || strings.HasPrefix(l, "*") || strings.HasPrefix(l, "//") {
							docComments = append([]string{l}, docComments...)
						} else {
							break
						}
					}
				}

				var md strings.Builder
				md.WriteString("```java\n")
				if m.Static {
					md.WriteString("static ")
				}
				md.WriteString(sig + "\n")
				md.WriteString("```\n")
				md.WriteString(fmt.Sprintf("\n*Declared in `%s`*\n", t.QualifiedName))
				if len(docComments) > 0 {
					md.WriteString("\n---\n" + strings.Join(docComments, "\n") + "\n")
				}

				return &HoverResult{
					Contents: map[string]string{
						"kind":  "markdown",
						"value": md.String(),
					},
					Range: m.Selection,
				}
			}
		}
	}

	// 查找类类型
	for _, t := range unit.Types {
		if containsPosition(t.Range, pos) || containsPosition(t.Selection, pos) {
			var md strings.Builder
			md.WriteString("```java\n")
			md.WriteString(fmt.Sprintf("class %s\n", t.Name))
			if t.Super != "" {
				md.WriteString(fmt.Sprintf("    extends %s\n", t.Super))
			}
			md.WriteString("```\n")
			md.WriteString(fmt.Sprintf("\nPackage: `%s`\n", t.QualifiedName))

			return &HoverResult{
				Contents: map[string]string{
					"kind":  "markdown",
					"value": md.String(),
				},
				Range: t.Selection,
			}
		}
	}

	return nil
}

// ComputeActiveSignatureHelp 计算光标在方法调用实参列表中的参数索引 (activeParameter)
func ComputeActiveSignatureHelp(source string, line int, character int) int {
	lines := strings.Split(source, "\n")
	if line < 0 || line >= len(lines) {
		return 0
	}
	currentLine := lines[line]
	if character > len(currentLine) {
		character = len(currentLine)
	}
	prefix := currentLine[:character]

	lastOpenParen := strings.LastIndex(prefix, "(")
	if lastOpenParen == -1 {
		return 0
	}

	argSlice := prefix[lastOpenParen+1:]
	return strings.Count(argSlice, ",")
}
