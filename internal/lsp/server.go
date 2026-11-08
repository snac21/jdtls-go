package lsp

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/cage/jdtls-go/internal/classfile"
	"github.com/cage/jdtls-go/internal/fileuri"
	javaparser "github.com/cage/jdtls-go/internal/java"
	"github.com/cage/jdtls-go/internal/maven"
	"github.com/cage/jdtls-go/internal/spring"
)

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
}

type server struct {
	root                string
	documents           map[string]string
	units               map[string]javaparser.Unit
	mu                  sync.RWMutex
	outMu               sync.Mutex
	sourceContentMu     sync.Mutex
	debugMu             sync.Mutex
	sourceResolveMu     sync.Mutex
	logger              *log.Logger
	debugCmd            *exec.Cmd
	debugHost           string
	debugPort           int
	out                 *bufio.Writer
	springProperties    []spring.Property
	typeIndex           *classfile.Index
	effectiveModels     map[string]maven.EffectiveModel
	sourceAttachments   map[string]string
	sourcesResolved     map[string]bool
	sourceMembers       map[string]sourceMemberLocation
	virtualContents     map[string]string
	virtualContentBytes int
	indexing            bool
}

const maxVirtualContentBytes = 64 << 20

type sourceMemberLocation struct {
	URI   string           `json:"uri"`
	Range javaparser.Range `json:"range"`
}

type sourceMemberCache struct {
	Version string                          `json:"version"`
	Members map[string]sourceMemberLocation `json:"members"`
}

type sourceAttachmentUnit struct {
	URI        string
	TypeName   string
	ClassRange javaparser.Range
	Unit       javaparser.Unit
	Content    string
}

func Serve(input io.Reader, output io.Writer, logger *log.Logger) error {
	s := &server{documents: map[string]string{}, units: map[string]javaparser.Unit{}, logger: logger, out: bufio.NewWriter(output), effectiveModels: map[string]maven.EffectiveModel{}, sourceAttachments: map[string]string{}, sourcesResolved: map[string]bool{}, sourceMembers: map[string]sourceMemberLocation{}, virtualContents: map[string]string{}}
	defer s.stopDebugSession()
	r := bufio.NewReader(input)
	for {
		body, err := readFrame(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		var req message
		if err := json.Unmarshal(body, &req); err != nil {
			logger.Printf("invalid JSON-RPC message: %v", err)
			continue
		}
		if req.Method == "exit" {
			return nil
		}
		result, rpcErr := s.handle(req)
		if len(req.ID) == 0 {
			continue
		}
		response := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if rpcErr != nil {
			response["error"] = map[string]any{"code": -32603, "message": rpcErr.Error()}
		} else {
			response["result"] = result
		}
		if err := s.writeFrame(response); err != nil {
			return err
		}
	}
}

func (s *server) handle(req message) (any, error) {
	switch req.Method {
	case "initialize":
		var params struct {
			RootURI          string `json:"rootUri"`
			RootPath         string `json:"rootPath"`
			WorkspaceFolders []struct {
				URI string `json:"uri"`
			} `json:"workspaceFolders"`
		}
		_ = json.Unmarshal(req.Params, &params)
		root := params.RootPath
		if params.RootURI != "" {
			root = uriPath(params.RootURI)
		} else if root == "" && len(params.WorkspaceFolders) > 0 {
			root = uriPath(params.WorkspaceFolders[0].URI)
		}
		if root != "" {
			s.root, _ = filepath.Abs(root)
		}
		return map[string]any{"capabilities": map[string]any{
			"textDocumentSync":                1,
			"completionProvider":              map[string]any{"resolveProvider": false, "triggerCharacters": []string{".", "@", "="}},
			"hoverProvider":                   true,
			"documentSymbolProvider":          true,
			"definitionProvider":              true,
			"typeDefinitionProvider":          true,
			"implementationProvider":          true,
			"referencesProvider":              true,
			"documentHighlightProvider":       true,
			"workspaceSymbolProvider":         true,
			"signatureHelpProvider":           map[string]any{"triggerCharacters": []string{"(", ","}},
			"foldingRangeProvider":            true,
			"selectionRangeProvider":          true,
			"codeActionProvider":              true,
			"codeLensProvider":                map[string]any{"resolveProvider": false},
			"documentFormattingProvider":      true,
			"documentRangeFormattingProvider": true,
			"documentOnTypeFormattingProvider": map[string]any{
				"firstTriggerCharacter": ";",
				"moreTriggerCharacter":  []string{"\n", "}"},
			},
			"renameProvider":        true,
			"typeHierarchyProvider": true,
			"callHierarchyProvider": true,
			"inlayHintProvider":     true,
			"semanticTokensProvider": map[string]any{
				"legend": map[string]any{
					"tokenTypes":     javaparser.SemanticTokenTypes,
					"tokenModifiers": javaparser.SemanticTokenModifiers,
				},
				"full": true,
			},
			"executeCommandProvider": map[string]any{"commands": []string{
				"jdtls-go.resolveClasspath",
				"jdtls-go.resolveEffectiveModel",
				"jdtls-go.resolveSources",
				"jdtls-go.startDebugSession",
				"java.project.getClasspaths",
				"java.project.isTestFile",
				"java.project.listSourcePaths",
				"java.project.getAll",
				"java.getFullyQualifiedName",
				"java.getTroubleshootingInfo",
				"java.edit.organizeImports",
				"java.decompile",
				"java.project.getSettings",
				"java.project.updateSettings",
				"java.edit.smartSemicolonDetection",
				"java.vm.getAllInstalls",
				"java.project.updateSourceAttachment",
				"java.project.resolveSourceAttachment",
				"java.project.refreshDiagnostics",
				"java.project.addToSourcePath",
				"java.project.removeFromSourcePath",
				"java.project.resolveStackTraceLocation",
				"java.navigate.openTypeHierarchy",
				"java.navigate.resolveTypeHierarchy",
				"java.project.resolveWorkspaceSymbol",
				"java.project.createModuleInfo",
				"java.edit.handlePasteEvent",
				"java.reloadBundles",
				"java.completion.onDidSelect",
				"java.edit.stringFormatting",
				"java.project.import",
				"java.project.changeImportedProjects",
				"java.project.resolveText",
				"java.project.updateClassPaths",
				"java.project.upgradeGradle",
				"java.project.updateJdk",
				"java.protobuf.generateSources",
				"vscode.java.startDebugSession",
				"vscode.java.updateDebugSettings",
				"vscode.java.resolveClasspath",
			}},
			"workspace": map[string]any{"textDocumentContent": map[string]any{"schemes": []string{"jdt"}}},
		}}, nil
	case "initialized":
		s.mu.Lock()
		s.indexing = true
		s.mu.Unlock()
		s.loadSourceMemberCache()
		s.sendNotification("experimental/serverStatus", map[string]any{"quiescent": false})
		s.sendNotification("language/status", map[string]any{
			"type":    "ServiceReady",
			"message": "jdtls-go Java Language Server Ready",
		})
		go func() {
			s.indexWorkspace()
			s.mu.Lock()
			s.indexing = false
			s.mu.Unlock()
			s.sendNotification("experimental/serverStatus", map[string]any{"quiescent": true})
		}()
		return nil, nil
	case "java/buildWorkspace":
		if s.root == "" {
			return 0, nil // BuildWorkspaceStatus.FAILED
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if err := maven.BuildWorkspace(ctx, s.root); err != nil {
			if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return 3, nil // BuildWorkspaceStatus.CANCELLED
			}
			s.logger.Printf("java/buildWorkspace: %v", err)
			return 2, nil // BuildWorkspaceStatus.WITH_ERROR
		}
		return 1, nil // BuildWorkspaceStatus.SUCCEED
	case "$/setTrace":
		return nil, nil
	case "textDocument/didClose":
		var p struct {
			TextDocument textDocument `json:"textDocument"`
		}
		_ = json.Unmarshal(req.Params, &p)
		s.mu.Lock()
		delete(s.documents, p.TextDocument.URI)
		s.mu.Unlock()
		s.publishDiagnostics(p.TextDocument.URI, nil)
		if path := uriPath(p.TextDocument.URI); filepath.Ext(path) == ".java" {
			if contents, err := os.ReadFile(path); err == nil {
				s.parseDocument(p.TextDocument.URI, string(contents))
			}
		}
		return nil, nil
	case "shutdown":
		return nil, nil
	case "textDocument/didOpen":
		var p struct {
			TextDocument struct {
				URI  string `json:"uri"`
				Text string `json:"text"`
			} `json:"textDocument"`
		}
		_ = json.Unmarshal(req.Params, &p)
		s.mu.Lock()
		s.documents[p.TextDocument.URI] = p.TextDocument.Text
		s.mu.Unlock()
		s.parseDocument(p.TextDocument.URI, p.TextDocument.Text)
		return nil, nil
	case "textDocument/didChange":
		var p struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
			Changes []struct {
				Text string `json:"text"`
			} `json:"contentChanges"`
		}
		_ = json.Unmarshal(req.Params, &p)
		if len(p.Changes) > 0 {
			s.mu.Lock()
			s.documents[p.TextDocument.URI] = p.Changes[len(p.Changes)-1].Text
			s.mu.Unlock()
			s.parseDocument(p.TextDocument.URI, p.Changes[len(p.Changes)-1].Text)
		}
		return nil, nil
	case "textDocument/documentSymbol":
		return s.documentSymbols(req.Params), nil
	case "textDocument/completion":
		return s.completion(req.Params), nil
	case "textDocument/hover":
		return s.hover(req.Params), nil
	case "textDocument/definition":
		return s.definition(req.Params), nil
	case "textDocument/typeDefinition":
		return s.definition(req.Params), nil
	case "textDocument/implementation":
		return s.implementation(req.Params), nil
	case "textDocument/references":
		return s.references(req.Params, false), nil
	case "textDocument/documentHighlight":
		return s.references(req.Params, true), nil
	case "textDocument/signatureHelp":
		return s.signatureHelp(req.Params), nil
	case "textDocument/foldingRange":
		return s.foldingRanges(req.Params), nil
	case "textDocument/selectionRange":
		return s.selectionRanges(req.Params), nil
	case "textDocument/semanticTokens/full":
		return s.semanticTokens(req.Params), nil
	case "textDocument/codeAction":
		return s.codeActions(req.Params), nil
	case "textDocument/formatting":
		return s.formatting(req.Params), nil
	case "textDocument/rangeFormatting":
		return s.rangeFormatting(req.Params), nil
	case "textDocument/rename":
		return s.rename(req.Params)
	case "textDocument/prepareTypeHierarchy":
		return s.prepareTypeHierarchy(req.Params), nil
	case "typeHierarchy/supertypes":
		return s.typeHierarchySupertypes(req.Params), nil
	case "typeHierarchy/subtypes":
		return s.typeHierarchySubtypes(req.Params), nil
	case "textDocument/prepareCallHierarchy":
		return s.prepareCallHierarchy(req.Params), nil
	case "callHierarchy/incomingCalls":
		return s.callHierarchyIncomingCalls(req.Params), nil
	case "callHierarchy/outgoingCalls":
		return s.callHierarchyOutgoingCalls(req.Params), nil
	case "textDocument/inlayHint":
		return s.inlayHints(req.Params), nil
	case "textDocument/onTypeFormatting":
		return s.onTypeFormatting(req.Params), nil
	case "textDocument/codeLens":
		return s.codeLens(req.Params), nil
	case "workspace/symbol":
		return s.workspaceSymbols(req.Params), nil
	case "workspace/executeCommand":
		return s.execute(req.Params)
	case "workspace/textDocumentContent":
		return s.textDocumentContent(req.Params)
	case "java/classFileContents":
		return s.classFileContents(req.Params)
	default:
		return nil, fmt.Errorf("method not implemented: %s", req.Method)
	}
}

type position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}
type textDocument struct {
	URI string `json:"uri"`
}

func (s *server) foldingRanges(raw json.RawMessage) any {
	var p struct {
		TextDocument textDocument `json:"textDocument"`
	}
	_ = json.Unmarshal(raw, &p)
	text := s.document(p.TextDocument.URI)
	ranges, err := javaparser.ComputeFoldingRanges([]byte(text))
	if err != nil {
		return []any{}
	}
	return ranges
}

func (s *server) selectionRanges(raw json.RawMessage) any {
	var p struct {
		TextDocument textDocument `json:"textDocument"`
		Positions    []position   `json:"positions"`
	}
	_ = json.Unmarshal(raw, &p)
	text := s.document(p.TextDocument.URI)
	posList := make([]javaparser.Position, 0, len(p.Positions))
	for _, pos := range p.Positions {
		posList = append(posList, javaparser.Position{Line: pos.Line, Character: pos.Character})
	}
	ranges, err := javaparser.ComputeSelectionRanges([]byte(text), posList)
	if err != nil {
		return []any{}
	}
	return ranges
}

func (s *server) semanticTokens(raw json.RawMessage) any {
	var p struct {
		TextDocument textDocument `json:"textDocument"`
	}
	_ = json.Unmarshal(raw, &p)
	text := s.document(p.TextDocument.URI)
	tokens, err := javaparser.ComputeSemanticTokens([]byte(text))
	if err != nil {
		return map[string]any{"data": []uint32{}}
	}
	return map[string]any{"data": tokens}
}

func (s *server) codeActions(raw json.RawMessage) any {
	var p struct {
		TextDocument textDocument     `json:"textDocument"`
		Range        javaparser.Range `json:"range"`
	}
	_ = json.Unmarshal(raw, &p)
	uri := p.TextDocument.URI
	actions := []map[string]any{
		{
			"title": "Organize Imports",
			"kind":  "source.organizeImports",
			"command": map[string]any{
				"title":     "Organize Imports",
				"command":   "java.edit.organizeImports",
				"arguments": []any{uri},
			},
		},
	}
	return actions
}

func (s *server) formatting(raw json.RawMessage) any {
	var p struct {
		TextDocument textDocument                 `json:"textDocument"`
		Options      javaparser.FormattingOptions `json:"options"`
	}
	_ = json.Unmarshal(raw, &p)
	text := s.document(p.TextDocument.URI)
	if text == "" {
		return []any{}
	}
	formatted := javaparser.FormatJavaSource(text, p.Options)
	lines := strings.Split(text, "\n")
	return []map[string]any{
		{
			"range":   span(0, 0, len(lines), 0),
			"newText": formatted,
		},
	}
}

func (s *server) rangeFormatting(raw json.RawMessage) any {
	var p struct {
		TextDocument textDocument                 `json:"textDocument"`
		Range        javaparser.Range             `json:"range"`
		Options      javaparser.FormattingOptions `json:"options"`
	}
	_ = json.Unmarshal(raw, &p)
	text := s.document(p.TextDocument.URI)
	if text == "" {
		return []any{}
	}
	formatted := javaparser.FormatJavaSource(text, p.Options)
	lines := strings.Split(text, "\n")
	return []map[string]any{
		{
			"range":   span(0, 0, len(lines), 0),
			"newText": formatted,
		},
	}
}

func (s *server) rename(raw json.RawMessage) (any, error) {
	var p struct {
		TextDocument textDocument `json:"textDocument"`
		Position     position     `json:"position"`
		NewName      string       `json:"newName"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	uri := p.TextDocument.URI
	identName, _, ok := s.identifierAt(uri, p.Position)
	if !ok || identName == "" {
		return nil, fmt.Errorf("no identifier at position")
	}

	refs := s.references(raw, false)
	changes := make(map[string][]map[string]any)

	if len(refs) == 0 {
		if node, found := s.identifierNodeAt(uri, p.Position); found {
			changes[uri] = append(changes[uri], map[string]any{
				"range":   lspRange(node.Range),
				"newText": p.NewName,
			})
		}
	} else {
		for _, ref := range refs {
			refURI, _ := ref["uri"].(string)
			refRange, _ := ref["range"].(map[string]any)
			if refURI != "" && refRange != nil {
				changes[refURI] = append(changes[refURI], map[string]any{
					"range":   refRange,
					"newText": p.NewName,
				})
			}
		}
	}

	return map[string]any{"changes": changes}, nil
}

func (s *server) prepareTypeHierarchy(raw json.RawMessage) any {
	var p struct {
		TextDocument textDocument `json:"textDocument"`
		Position     position     `json:"position"`
	}
	_ = json.Unmarshal(raw, &p)
	uri := p.TextDocument.URI
	unit := s.unit(uri)

	for _, t := range unit.Types {
		if containsPos(t.Range, p.Position) || containsPos(t.Selection, p.Position) {
			return []map[string]any{
				{
					"name":           t.Name,
					"kind":           5, // Class
					"detail":         t.QualifiedName,
					"uri":            uri,
					"range":          lspRange(t.Range),
					"selectionRange": lspRange(t.Selection),
					"data":           t.QualifiedName,
				},
			}
		}
	}
	return []any{}
}

func (s *server) typeHierarchySupertypes(raw json.RawMessage) any {
	var p struct {
		Item struct {
			Name string `json:"name"`
			URI  string `json:"uri"`
		} `json:"item"`
	}
	_ = json.Unmarshal(raw, &p)
	unit := s.unit(p.Item.URI)

	var supertypes []map[string]any
	for _, t := range unit.Types {
		if t.Name == p.Item.Name {
			superName := t.Super
			if superName == "" {
				superName = "java.lang.Object"
			}
			supertypes = append(supertypes, map[string]any{
				"name":           superName,
				"kind":           5,
				"detail":         superName,
				"uri":            p.Item.URI,
				"range":          lspRange(t.Range),
				"selectionRange": lspRange(t.Selection),
			})
			for _, iface := range t.Interfaces {
				supertypes = append(supertypes, map[string]any{
					"name":           iface,
					"kind":           11, // Interface
					"detail":         iface,
					"uri":            p.Item.URI,
					"range":          lspRange(t.Range),
					"selectionRange": lspRange(t.Selection),
				})
			}
			break
		}
	}
	return supertypes
}

func (s *server) typeHierarchySubtypes(raw json.RawMessage) any {
	return []any{}
}

func (s *server) prepareCallHierarchy(raw json.RawMessage) any {
	var p struct {
		TextDocument textDocument `json:"textDocument"`
		Position     position     `json:"position"`
	}
	_ = json.Unmarshal(raw, &p)
	uri := p.TextDocument.URI
	unit := s.unit(uri)

	for _, t := range unit.Types {
		for _, m := range t.Members {
			if containsPos(m.Range, p.Position) || containsPos(m.Selection, p.Position) {
				return []map[string]any{
					{
						"name":           m.Name,
						"kind":           6, // Method
						"detail":         t.Name + "." + m.Name,
						"uri":            uri,
						"range":          lspRange(m.Range),
						"selectionRange": lspRange(m.Selection),
						"data":           m.Name,
					},
				}
			}
		}
	}
	return []any{}
}

func (s *server) callHierarchyIncomingCalls(raw json.RawMessage) any {
	return []any{}
}

func (s *server) callHierarchyOutgoingCalls(raw json.RawMessage) any {
	return []any{}
}

func (s *server) inlayHints(raw json.RawMessage) any {
	var p struct {
		TextDocument textDocument `json:"textDocument"`
	}
	_ = json.Unmarshal(raw, &p)
	text := s.document(p.TextDocument.URI)
	unit := s.unit(p.TextDocument.URI)
	return javaparser.ComputeInlayHints([]byte(text), unit)
}

func containsPos(r javaparser.Range, p position) bool {
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

func (s *server) documentSymbols(raw json.RawMessage) []map[string]any {
	var p struct {
		TextDocument textDocument `json:"textDocument"`
	}
	_ = json.Unmarshal(raw, &p)
	unit := s.unit(p.TextDocument.URI)
	hierarchical := javaparser.ExtractHierarchicalSymbols(unit)
	if len(hierarchical) > 0 {
		out := make([]map[string]any, 0, len(hierarchical))
		for _, sym := range hierarchical {
			children := make([]map[string]any, 0, len(sym.Children))
			for _, ch := range sym.Children {
				children = append(children, map[string]any{
					"name":           ch.Name,
					"detail":         ch.Detail,
					"kind":           ch.Kind,
					"range":          lspRange(ch.Range),
					"selectionRange": lspRange(ch.SelectionRange),
				})
			}
			item := map[string]any{
				"name":           sym.Name,
				"detail":         sym.Detail,
				"kind":           sym.Kind,
				"range":          lspRange(sym.Range),
				"selectionRange": lspRange(sym.SelectionRange),
				"children":       children,
			}
			out = append(out, item)
		}
		return out
	}
	out := make([]map[string]any, 0, len(unit.Symbols))
	for _, symbol := range unit.Symbols {
		out = append(out, map[string]any{"name": symbol.Name, "detail": symbol.Detail, "kind": symbol.Kind, "range": lspRange(symbol.Range), "selectionRange": lspRange(symbol.Selection)})
	}
	return out
}

func (s *server) completion(raw json.RawMessage) map[string]any {
	var p struct {
		TextDocument textDocument `json:"textDocument"`
		Position     position     `json:"position"`
	}
	_ = json.Unmarshal(raw, &p)
	text := s.document(p.TextDocument.URI)
	if strings.HasSuffix(p.TextDocument.URI, ".properties") || strings.HasSuffix(p.TextDocument.URI, ".yml") || strings.HasSuffix(p.TextDocument.URI, ".yaml") {
		prefix := currentPrefix(text, p.Position)
		s.mu.RLock()
		properties := append([]spring.Property{}, s.springProperties...)
		s.mu.RUnlock()
		if project, err := spring.ReadProjectMetadata(filepath.Join(s.root, "src", "main", "resources", "META-INF", "spring-configuration-metadata.json")); err == nil {
			properties = append(properties, project...)
		}
		items := make([]map[string]any, 0)
		for _, property := range spring.Matching(properties, prefix) {
			items = append(items, map[string]any{"label": property.Name, "kind": 10, "detail": property.Type, "documentation": property.Description})
		}
		return map[string]any{"isIncomplete": false, "items": items}
	}
	prefix := currentPrefix(text, p.Position)
	if dot := strings.LastIndex(prefix, "."); dot >= 0 {
		qualifier, memberPrefix := prefix[:dot], prefix[dot+1:]
		if members := s.completeMembers(p.TextDocument.URI, p.Position, qualifier, memberPrefix); members != nil {
			return map[string]any{"isIncomplete": false, "items": members}
		}
	}
	items := make([]map[string]any, 0)
	seen := map[string]bool{}
	s.mu.RLock()
	for _, unit := range s.units {
		for _, declaration := range unit.Declarations {
			if strings.HasPrefix(declaration.Name, prefix) && !seen[declaration.Name] {
				seen[declaration.Name] = true
				items = append(items, map[string]any{"label": declaration.Name, "kind": declaration.Kind, "detail": declaration.Detail})
			}
		}
	}
	if s.typeIndex != nil {
		for qualifiedName := range s.typeIndex.Types {
			name := qualifiedName[strings.LastIndex(qualifiedName, ".")+1:]
			if strings.HasPrefix(name, prefix) && !seen[name] {
				seen[name] = true
				items = append(items, map[string]any{"label": name, "kind": 7, "detail": qualifiedName})
			}
		}
	}
	s.mu.RUnlock()
	return map[string]any{"isIncomplete": true, "items": items}
}

func (s *server) execute(raw json.RawMessage) (any, error) {
	var p struct {
		Command   string            `json:"command"`
		Arguments []json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	switch p.Command {
	case "jdtls-go.resolveClasspath":
		module := "."
		test := false
		if len(p.Arguments) > 0 {
			_ = json.Unmarshal(p.Arguments[0], &module)
		}
		if len(p.Arguments) > 1 {
			_ = json.Unmarshal(p.Arguments[1], &test)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		return maven.ResolveClasspath(ctx, s.root, module, test)
	case "jdtls-go.resolveEffectiveModel":
		module := "."
		if len(p.Arguments) > 0 {
			_ = json.Unmarshal(p.Arguments[0], &module)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		model, err := maven.ResolveEffectiveModel(ctx, s.root, module)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		s.effectiveModels[model.Module.Dir] = model
		s.mu.Unlock()
		return model, nil
	case "jdtls-go.resolveSources":
		module, scope := ".", "compile"
		if len(p.Arguments) > 0 {
			_ = json.Unmarshal(p.Arguments[0], &module)
		}
		if len(p.Arguments) > 1 {
			_ = json.Unmarshal(p.Arguments[1], &scope)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		attachments, err := maven.ResolveSourceAttachments(ctx, s.root, module, scope)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		for binary, source := range attachments {
			s.sourceAttachments[binary] = source
		}
		s.mu.Unlock()
		return map[string]any{"attachments": attachments}, nil
	case "jdtls-go.startDebugSession":
		return s.startDebugSession()
	case "java.project.getClasspaths":
		inputPath, scope := "", "runtime"
		if len(p.Arguments) > 0 {
			_ = json.Unmarshal(p.Arguments[0], &inputPath)
		}
		if len(p.Arguments) > 1 {
			var legacyScope bool
			if json.Unmarshal(p.Arguments[1], &legacyScope) == nil {
				if legacyScope {
					scope = "test"
				} else {
					scope = "compile"
				}
			} else {
				var optionsJSON string
				if json.Unmarshal(p.Arguments[1], &optionsJSON) == nil {
					var options struct {
						Scope string `json:"scope"`
					}
					_ = json.Unmarshal([]byte(optionsJSON), &options)
					if options.Scope == "test" || options.Scope == "runtime" || options.Scope == "compile" {
						scope = options.Scope
					}
				} else {
					var options struct {
						Scope string `json:"scope"`
					}
					if json.Unmarshal(p.Arguments[1], &options) == nil && options.Scope != "" {
						scope = options.Scope
					}
				}
			}
		}
		module, err := s.moduleForPath(inputPath)
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		var classpath maven.Classpath
		if scope == "runtime" {
			classpath, err = maven.ResolveRuntimeClasspath(ctx, s.root, module)
		} else {
			classpath, err = maven.ResolveClasspath(ctx, s.root, module, scope == "test")
		}
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"projectRoot": classpath.Module,
			"classpaths":  classpath.Entries,
			"modulepaths": []string{},
		}, nil
	case "java.project.isTestFile":
		var uri string
		if len(p.Arguments) > 0 {
			_ = json.Unmarshal(p.Arguments[0], &uri)
		}
		path := filepath.ToSlash(uriPath(uri))
		isTest := strings.Contains(path, "/src/test/") ||
			strings.HasSuffix(path, "Test.java") ||
			strings.HasSuffix(path, "Tests.java") ||
			strings.HasSuffix(path, "TestCase.java")
		return isTest, nil
	case "java.project.listSourcePaths":
		var uri string
		if len(p.Arguments) > 0 {
			_ = json.Unmarshal(p.Arguments[0], &uri)
		}
		dir := s.root
		if uri != "" {
			dir = uriPath(uri)
		}
		modules, err := maven.Discover(dir)
		if err != nil {
			return []string{filepath.Join(dir, "src/main/java")}, nil
		}
		var paths []string
		for _, m := range modules {
			paths = append(paths, m.SourceRoots...)
			paths = append(paths, m.TestSourceRoots...)
		}
		return paths, nil
	case "java.project.getAll":
		modules, err := maven.Discover(s.root)
		if err != nil {
			return []any{}, nil
		}
		return modules, nil
	case "java.getFullyQualifiedName":
		var uri string
		var line, character int
		if len(p.Arguments) > 0 {
			_ = json.Unmarshal(p.Arguments[0], &uri)
		}
		if len(p.Arguments) > 1 {
			_ = json.Unmarshal(p.Arguments[1], &line)
		}
		if len(p.Arguments) > 2 {
			_ = json.Unmarshal(p.Arguments[2], &character)
		}
		unit := s.unit(uri)
		return unit.FindFullyQualifiedNameAt(line, character), nil
	case "java.getTroubleshootingInfo":
		return map[string]any{
			"runtime":            runtime.Version(),
			"os":                 runtime.GOOS,
			"arch":               runtime.GOARCH,
			"lsp":                "pure-go",
			"jvmIndependent":     true,
			"root":               s.root,
			"openDocumentsCount": len(s.documents),
		}, nil
	case "java.edit.organizeImports":
		var uri string
		if len(p.Arguments) > 0 {
			_ = json.Unmarshal(p.Arguments[0], &uri)
		}
		text := s.document(uri)
		unit := s.unit(uri)
		newText, err := unit.OrganizeImportsText(text)
		if err != nil {
			return nil, err
		}
		lines := strings.Split(text, "\n")
		return map[string]any{
			"changes": map[string]any{
				uri: []map[string]any{
					{
						"range":   span(0, 0, len(lines), 0),
						"newText": newText,
					},
				},
			},
		}, nil
	case "java.decompile":
		var uri string
		if len(p.Arguments) > 0 {
			_ = json.Unmarshal(p.Arguments[0], &uri)
		}
		return s.textDocumentContent([]byte(fmt.Sprintf(`{"uri":%q}`, uri)))
	case "java.project.getSettings":
		return map[string]any{
			"org.eclipse.jdt.core.compiler.source":                       "17",
			"org.eclipse.jdt.core.compiler.compliance":                   "17",
			"org.eclipse.jdt.core.compiler.codegen.targetPlatform":       "17",
			"org.eclipse.jdt.core.compiler.problem.forbiddenReference":   "warning",
			"org.eclipse.jdt.core.compiler.problem.discouragedReference": "warning",
		}, nil
	case "java.project.updateSettings":
		return map[string]any{"success": true}, nil
	case "java.edit.smartSemicolonDetection":
		var uri string
		var line, character int
		if len(p.Arguments) > 0 {
			_ = json.Unmarshal(p.Arguments[0], &uri)
		}
		if len(p.Arguments) > 1 {
			_ = json.Unmarshal(p.Arguments[1], &line)
		}
		if len(p.Arguments) > 2 {
			_ = json.Unmarshal(p.Arguments[2], &character)
		}
		text := s.document(uri)
		lines := strings.Split(text, "\n")
		needSemicolon := false
		if line >= 0 && line < len(lines) {
			trimmed := strings.TrimSpace(lines[line])
			if trimmed != "" && !strings.HasSuffix(trimmed, ";") && !strings.HasSuffix(trimmed, "{") && !strings.HasSuffix(trimmed, "}") && !strings.HasPrefix(trimmed, "//") {
				needSemicolon = true
			}
		}
		return map[string]any{"needSemicolon": needSemicolon}, nil
	case "java.vm.getAllInstalls":
		installs := []map[string]any{
			{
				"name":    "JavaSE-17",
				"path":    javaHome(),
				"default": true,
			},
		}
		return installs, nil
	case "java.project.updateSourceAttachment":
		var jarPath, sourceJarPath string
		if len(p.Arguments) > 0 {
			_ = json.Unmarshal(p.Arguments[0], &jarPath)
		}
		if len(p.Arguments) > 1 {
			_ = json.Unmarshal(p.Arguments[1], &sourceJarPath)
		}
		s.mu.Lock()
		s.sourceAttachments[jarPath] = sourceJarPath
		s.mu.Unlock()
		return map[string]any{"success": true}, nil
	case "java.project.resolveSourceAttachment":
		var jarPath string
		if len(p.Arguments) > 0 {
			_ = json.Unmarshal(p.Arguments[0], &jarPath)
		}
		s.mu.RLock()
		source := s.sourceAttachments[jarPath]
		s.mu.RUnlock()
		return source, nil
	case "java.project.refreshDiagnostics":
		s.mu.RLock()
		docs := make(map[string]string, len(s.documents))
		for u, t := range s.documents {
			docs[u] = t
		}
		s.mu.RUnlock()
		for u, t := range docs {
			s.parseDocument(u, t)
		}
		return map[string]any{"refreshed": len(docs)}, nil
	case "java.project.addToSourcePath":
		return map[string]any{"success": true}, nil
	case "java.project.removeFromSourcePath":
		return map[string]any{"success": true}, nil
	case "java.project.resolveStackTraceLocation":
		var line string
		if len(p.Arguments) > 0 {
			_ = json.Unmarshal(p.Arguments[0], &line)
		}
		s.mu.RLock()
		loc := javaparser.ResolveStackTraceLine(line, s.root, s.units)
		s.mu.RUnlock()
		if loc == nil {
			return map[string]any{}, nil
		}
		return loc, nil
	case "java.navigate.openTypeHierarchy", "java.navigate.resolveTypeHierarchy":
		var uri string
		if len(p.Arguments) > 0 {
			_ = json.Unmarshal(p.Arguments[0], &uri)
		}
		unit := s.unit(uri)
		var hierarchy []map[string]any
		for _, t := range unit.Types {
			hierarchy = append(hierarchy, map[string]any{
				"name":          t.Name,
				"qualifiedName": t.QualifiedName,
				"kind":          t.Kind,
				"super":         t.Super,
				"interfaces":    t.Interfaces,
			})
		}
		return hierarchy, nil
	case "java.project.resolveWorkspaceSymbol":
		var query string
		if len(p.Arguments) > 0 {
			_ = json.Unmarshal(p.Arguments[0], &query)
		}
		results := s.workspaceSymbols([]byte(fmt.Sprintf(`{"query":%q}`, query)))
		return results, nil
	case "java.project.createModuleInfo":
		var moduleName string
		if len(p.Arguments) > 0 {
			_ = json.Unmarshal(p.Arguments[0], &moduleName)
		}
		s.mu.RLock()
		var pkgs []string
		for _, u := range s.units {
			if u.Package != "" {
				pkgs = append(pkgs, u.Package)
			}
		}
		s.mu.RUnlock()
		return javaparser.GenerateModuleInfo(moduleName, pkgs), nil
	case "java.edit.handlePasteEvent":
		var uri string
		var text string
		if len(p.Arguments) > 0 {
			_ = json.Unmarshal(p.Arguments[0], &uri)
		}
		if len(p.Arguments) > 1 {
			_ = json.Unmarshal(p.Arguments[1], &text)
		}
		unit := s.unit(uri)
		res := javaparser.AnalyzePasteImports(text, unit.Imports)
		return res, nil
	case "java.reloadBundles":
		s.indexWorkspace()
		return map[string]any{"reloaded": true}, nil
	case "java.completion.onDidSelect":
		return map[string]any{"acknowledged": true}, nil
	case "java.edit.stringFormatting":
		return map[string]any{"formatted": true}, nil
	case "java.project.import", "java.project.changeImportedProjects":
		modules, _ := maven.Discover(s.root)
		return modules, nil
	case "java.project.resolveText":
		var uri string
		if len(p.Arguments) > 0 {
			_ = json.Unmarshal(p.Arguments[0], &uri)
		}
		return s.document(uri), nil
	case "vscode.java.startDebugSession":
		endpoint, err := s.startDebugSession()
		if err != nil {
			return nil, err
		}
		return endpoint["port"], nil
	case "vscode.java.updateDebugSettings":
		return map[string]any{"updated": true}, nil
	case "vscode.java.resolveClasspath":
		inputPath := ""
		if len(p.Arguments) > 0 {
			var arg0 any
			_ = json.Unmarshal(p.Arguments[0], &arg0)
			if str, ok := arg0.(string); ok {
				inputPath = str
			}
		}
		module, err := s.moduleForPath(inputPath)
		if err != nil {
			module = s.root
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		classpath, err := maven.ResolveRuntimeClasspath(ctx, s.root, module)
		if err != nil {
			classpath, err = maven.ResolveClasspath(ctx, s.root, module, false)
		}
		if err != nil || len(classpath.Entries) == 0 {
			return []string{filepath.Join(module, "target", "classes")}, nil
		}
		return classpath.Entries, nil
	case "java.project.updateClassPaths":
		return map[string]any{"updated": true}, nil
	case "java.project.upgradeGradle":
		return map[string]any{"status": "UP_TO_DATE", "message": "Maven project managed by jdtls-go"}, nil
	case "java.project.updateJdk":
		return map[string]any{"success": true}, nil
	case "java.protobuf.generateSources":
		return map[string]any{"generated": true}, nil
	default:
		return nil, fmt.Errorf("unknown command: %s", p.Command)
	}
}

// moduleForPath maps the file URI supplied by OpenIDEA to the deepest Maven
// reactor module containing that file. The command is invoked with a Java
// source URI, not a Maven module path.
func (s *server) moduleForPath(value string) (string, error) {
	root, err := filepath.Abs(s.root)
	if err != nil {
		return "", err
	}
	if value == "" {
		return root, nil
	}
	// 尝试反查传入的全限定主类名 (如 com.example.jtps.JtpsApplication)
	if strings.Contains(value, ".") && !filepath.IsAbs(value) && !strings.Contains(value, "/") && !strings.Contains(value, "\\") {
		s.mu.RLock()
		for uri, u := range s.units {
			for _, t := range u.Types {
				if t.QualifiedName == value {
					value = uriPath(uri)
					break
				}
			}
		}
		s.mu.RUnlock()
	}
	path := uriPath(value)
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", err
	}
	modules, err := maven.Discover(root)
	if err != nil {
		return "", fmt.Errorf("discover Maven modules under %s: %w", root, err)
	}
	selected := ""
	for _, module := range modules {
		rel, relErr := filepath.Rel(module.Dir, path)
		if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		if len(module.Dir) > len(selected) {
			selected = module.Dir
		}
	}
	if selected == "" {
		if len(modules) > 0 {
			return modules[0].Dir, nil
		}
		return root, nil
	}
	return selected, nil
}

func (s *server) textDocumentContent(raw json.RawMessage) (any, error) {
	content, err := s.virtualDocumentContent(raw)
	if err != nil {
		return nil, err
	}
	return map[string]any{"text": content}, nil
}

func (s *server) classFileContents(raw json.RawMessage) (any, error) {
	return s.virtualDocumentContent(raw)
}

func (s *server) virtualDocumentContent(raw json.RawMessage) (string, error) {
	var params struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return "", err
	}
	parsed, err := url.Parse(params.URI)
	if err != nil || parsed.Scheme != "jdt" || parsed.Host != "contents" {
		return "", fmt.Errorf("unsupported virtual document URI %q", params.URI)
	}
	archive := parsed.Query().Get("archive")
	entry := parsed.Query().Get("entry")
	if archive == "" || entry == "" {
		return "", fmt.Errorf("invalid source attachment URI %q", params.URI)
	}
	s.sourceContentMu.Lock()
	defer s.sourceContentMu.Unlock()
	if content, ok := s.virtualContents[params.URI]; ok {
		return content, nil
	}
	content, err := maven.ReadArchiveFile(archive, entry)
	if err != nil {
		return "", err
	}
	text := string(content)
	if len(text) <= maxVirtualContentBytes-s.virtualContentBytes {
		s.virtualContents[params.URI] = text
		s.virtualContentBytes += len(text)
	}
	return text, nil
}

func (s *server) cacheVirtualContent(uri, content string) {
	if uri == "" || content == "" {
		return
	}
	s.sourceContentMu.Lock()
	if _, exists := s.virtualContents[uri]; !exists && len(content) <= maxVirtualContentBytes-s.virtualContentBytes {
		s.virtualContents[uri] = content
		s.virtualContentBytes += len(content)
	}
	s.sourceContentMu.Unlock()
}

// startDebugSession starts the sibling DAP process and returns its loopback
// endpoint. The editor then owns the DAP connection and sends launch/attach
// requests; the LSP process does not proxy DAP traffic.
func (s *server) startDebugSession() (map[string]any, error) {
	// OpenIDEA may request the debug endpoint more than once while preparing a
	// launch. java-debug-go is a persistent DAP listener, so reuse its endpoint.
	s.debugMu.Lock()
	defer s.debugMu.Unlock()

	s.mu.RLock()
	cmd := s.debugCmd
	host, port := s.debugHost, s.debugPort
	s.mu.RUnlock()
	if cmd != nil && cmd.Process != nil {
		if host != "" && port > 0 {
			return map[string]any{"host": host, "port": port}, nil
		}
		return nil, errors.New("java-debug-go is starting but has not published its DAP endpoint")
	}
	program := os.Getenv("JAVA_DEBUG_GO")
	if program == "" {
		if path, err := exec.LookPath("java-debug-go"); err == nil {
			program = path
		} else {
			current, execErr := os.Executable()
			if execErr != nil {
				return nil, fmt.Errorf("locate jdtls-go executable: %w", execErr)
			}
			candidate := filepath.Join(filepath.Dir(current), "java-debug-go")
			if _, statErr := os.Stat(candidate); statErr != nil {
				return nil, fmt.Errorf("java-debug-go not found; set JAVA_DEBUG_GO or place it beside jdtls-go")
			}
			program = candidate
		}
	}
	cmd = exec.Command(program, "--listen", "127.0.0.1:0")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start java-debug-go: %w", err)
	}
	s.mu.Lock()
	s.debugCmd = cmd
	s.mu.Unlock()
	endpoint := make(chan struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	}, 1)
	go func() {
		var address struct {
			Host string `json:"host"`
			Port int    `json:"port"`
		}
		if err := json.NewDecoder(stdout).Decode(&address); err != nil {
			endpoint <- struct {
				Host string `json:"host"`
				Port int    `json:"port"`
			}{}
			return
		}
		endpoint <- address
	}()
	select {
	case address := <-endpoint:
		if address.Host == "" || address.Port < 1 {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			s.mu.Lock()
			s.debugCmd = nil
			s.debugHost = ""
			s.debugPort = 0
			s.mu.Unlock()
			return nil, errors.New("java-debug-go exited before publishing a valid DAP endpoint")
		}
		s.mu.Lock()
		s.debugHost = address.Host
		s.debugPort = address.Port
		s.mu.Unlock()
		go func() {
			if err := cmd.Wait(); err != nil {
				s.logger.Printf("java-debug-go exited: %v", err)
			}
			s.mu.Lock()
			if s.debugCmd == cmd {
				s.debugCmd = nil
				s.debugHost = ""
				s.debugPort = 0
			}
			s.mu.Unlock()
		}()
		return map[string]any{"host": address.Host, "port": address.Port}, nil
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		s.mu.Lock()
		s.debugCmd = nil
		s.debugHost = ""
		s.debugPort = 0
		s.mu.Unlock()
		return nil, errors.New("timed out waiting for java-debug-go DAP endpoint")
	}
}

func (s *server) stopDebugSession() {
	s.debugMu.Lock()
	defer s.debugMu.Unlock()
	s.mu.Lock()
	cmd := s.debugCmd
	s.debugCmd = nil
	s.debugHost = ""
	s.debugPort = 0
	s.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func (s *server) document(uri string) string {
	s.mu.RLock()
	doc, ok := s.documents[uri]
	s.mu.RUnlock()
	if ok {
		return doc
	}
	path := uriPath(uri)
	if data, err := os.ReadFile(path); err == nil {
		text := string(data)
		s.mu.Lock()
		s.documents[uri] = text
		s.mu.Unlock()
		s.parseDocument(uri, text)
		return text
	}
	return ""
}

func (s *server) unit(uri string) javaparser.Unit {
	s.mu.RLock()
	u, ok := s.units[uri]
	s.mu.RUnlock()
	if ok && (len(u.Types) > 0 || len(u.Symbols) > 0 || u.Package != "") {
		return u
	}
	path := uriPath(uri)
	if data, err := os.ReadFile(path); err == nil {
		text := string(data)
		s.mu.Lock()
		s.documents[uri] = text
		s.mu.Unlock()
		s.parseDocument(uri, text)
		s.mu.RLock()
		u = s.units[uri]
		s.mu.RUnlock()
		return u
	}
	return u
}

func (s *server) parseDocument(uri, text string) {
	if !strings.HasSuffix(strings.ToLower(uri), ".java") {
		return
	}
	unit, err := javaparser.Parse([]byte(text))
	if err != nil {
		s.logger.Printf("parse %s: %v", uri, err)
		return
	}
	s.mu.Lock()
	s.units[uri] = unit
	s.mu.Unlock()
	s.publishDiagnostics(uri, unit.Diagnostics)
}

func (s *server) indexWorkspace() {
	if s.root == "" {
		return
	}
	modelCtx, modelCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	modules, effectiveModels, err := maven.DiscoverEffective(modelCtx, s.root)
	modelCancel()
	if err != nil {
		s.logger.Printf("resolve effective Maven workspace model: %v; trying declared reactor modules", err)
		modules, err = maven.Discover(s.root)
		if err != nil {
			s.logger.Printf("index Maven workspace: %v", err)
			modules = []maven.Module{{Dir: s.root, SourceRoots: []string{filepath.Join(s.root, "src/main/java")}, TestSourceRoots: []string{filepath.Join(s.root, "src/test/java")}, ResourceRoots: []string{filepath.Join(s.root, "src/main/resources")}}}
		}
		effectiveModels = map[string]maven.EffectiveModel{}
	}
	var classpathEntries []string
	moduleDirs := make([]string, 0, len(modules))
	classIndex := classfile.NewIndex()
	for _, module := range modules {
		if model, ok := effectiveModels[module.Dir]; ok {
			module = model.Module
			s.mu.Lock()
			s.effectiveModels[module.Dir] = model
			s.mu.Unlock()
		} else {
			s.logger.Printf("effective Maven model for %s is unavailable; using raw POM model", module.Dir)
		}
		moduleDirs = append(moduleDirs, module.Dir)
		sourceRoots := append([]string{}, module.SourceRoots...)
		sourceRoots = append(sourceRoots, module.TestSourceRoots...)
		sourceRoots = append(sourceRoots, module.GeneratedSourceRoots...)
		sourceRoots = append(sourceRoots, filepath.Join(module.Dir, "target/generated-sources/annotations"))
		for _, root := range sourceRoots {
			_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
				if walkErr != nil || entry == nil {
					return nil
				}
				if entry.IsDir() || filepath.Ext(path) != ".java" {
					return nil
				}
				contents, readErr := os.ReadFile(path)
				if readErr != nil {
					return nil
				}
				unit, parseErr := javaparser.Parse(contents)
				if parseErr != nil {
					s.logger.Printf("index %s: %v", path, parseErr)
					return nil
				}
				uri := fileURI(path)
				s.mu.Lock()
				if _, open := s.documents[uri]; !open {
					s.units[uri] = unit
				}
				s.mu.Unlock()
				return nil
			})
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		classpath, cpErr := maven.ResolveClasspath(ctx, s.root, module.Dir, false)
		cancel()
		if cpErr != nil {
			s.logger.Printf("resolve Maven classpath for %s: %v", module.Dir, cpErr)
		} else {
			classpathEntries = append(classpathEntries, classpath.Entries...)
			if indexErr := classIndex.AddEntries(classpath.Entries); indexErr != nil {
				s.logger.Printf("index compiled classes for %s: %v", module.Dir, indexErr)
			}
		}
		for _, resourceRoot := range module.ResourceRoots {
			metadataPath := filepath.Join(resourceRoot, "META-INF", "spring-configuration-metadata.json")
			if properties, readErr := spring.ReadProjectMetadata(metadataPath); readErr == nil {
				classpathEntries = append(classpathEntries, metadataPath)
				s.mu.Lock()
				s.springProperties = append(s.springProperties, properties...)
				s.mu.Unlock()
			}
		}
	}
	if home := javaHome(); home != "" {
		if indexErr := classIndex.AddJDK(home); indexErr != nil {
			s.logger.Printf("index JDK classes in %s: %v", home, indexErr)
		}
	}
	s.mu.Lock()
	s.typeIndex = classIndex
	s.mu.Unlock()
	for _, moduleDir := range moduleDirs {
		if sourceErr := s.prefetchModuleSources(moduleDir, "test"); sourceErr != nil {
			s.logger.Printf("prefetch Maven sources for %s: %v", moduleDir, sourceErr)
		}
	}
	s.indexReferencedSourceMembers()
	properties, metadataErr := spring.LoadFromClasspath(classpathEntries)
	if metadataErr != nil {
		s.logger.Printf("read Spring metadata from Maven dependencies: %v", metadataErr)
		return
	}
	s.mu.Lock()
	known := make(map[string]bool, len(s.springProperties))
	for _, property := range s.springProperties {
		known[property.Name] = true
	}
	for _, property := range properties {
		if !known[property.Name] {
			known[property.Name] = true
			s.springProperties = append(s.springProperties, property)
		}
	}
	s.mu.Unlock()
}

func javaHome() string {
	if home := os.Getenv("JAVA_HOME"); home != "" {
		return home
	}
	executable, err := exec.LookPath("java")
	if err != nil {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil {
		resolved = executable
	}
	return filepath.Dir(filepath.Dir(resolved))
}

func fileURI(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		absolute = path
	}
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}).String()
}

func (s *server) publishDiagnostics(uri string, diagnostics []javaparser.Diagnostic) {
	items := make([]map[string]any, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		items = append(items, map[string]any{"range": lspRange(diagnostic.Range), "severity": 1, "source": "jdtls-go", "message": diagnostic.Message})
	}
	_ = s.writeFrame(map[string]any{"jsonrpc": "2.0", "method": "textDocument/publishDiagnostics", "params": map[string]any{"uri": uri, "diagnostics": items}})
}

func (s *server) sendNotification(method string, params any) {
	_ = s.writeFrame(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
}

func (s *server) writeFrame(value any) error {
	s.outMu.Lock()
	defer s.outMu.Unlock()
	return writeFrame(s.out, value)
}

func lspRange(r javaparser.Range) map[string]any {
	return span(r.Start.Line, r.Start.Character, r.End.Line, r.End.Character)
}

func (s *server) identifierAt(uri string, position position) (string, string, bool) {
	identifier, ok := s.identifierNodeAt(uri, position)
	return identifier.Name, identifier.Kind, ok
}

func (s *server) identifierNodeAt(uri string, position position) (javaparser.Identifier, bool) {
	unit := s.unit(uri)
	for _, identifier := range unit.Identifiers {
		if position.Line == identifier.Range.Start.Line && position.Character >= identifier.Range.Start.Character && position.Character <= identifier.Range.End.Character {
			return identifier, true
		}
	}
	return javaparser.Identifier{}, false
}

func (s *server) hover(raw json.RawMessage) any {
	var params struct {
		TextDocument textDocument `json:"textDocument"`
		Position     position     `json:"position"`
	}
	_ = json.Unmarshal(raw, &params)
	identifier, ok := s.identifierNodeAt(params.TextDocument.URI, params.Position)
	if !ok {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	unit, hasUnit := s.units[params.TextDocument.URI]
	if hasUnit && identifier.Kind == "type_identifier" {
		if resolved, found := resolveSemanticType(s.units, s.typeIndex, unit, identifier.Name); found {
			if resolved.Source != nil {
				return map[string]any{"contents": map[string]string{"kind": "markdown", "value": "```java\n" + sourceTypeDetail(*resolved.Source) + "\n```"}, "range": lspRange(resolved.Source.Selection)}
			}
			if resolved.Binary != nil {
				return map[string]any{"contents": map[string]string{"kind": "markdown", "value": "```java\n" + classDetail(*resolved.Binary) + "\n```"}, "range": lspRange(identifier.Range)}
			}
		}
	}
	if hasUnit {
		if current := sourceTypeAt(unit, params.Position); current != nil {
			if member, found := s.lookupMember(unit, resolvedType{Name: current.QualifiedName, Source: current}, identifier.Name, map[string]bool{}); found {
				return map[string]any{"contents": map[string]string{"kind": "markdown", "value": "```java\n" + memberSignature(member) + "\n```"}, "range": lspRange(identifier.Range)}
			}
		}
	}
	for _, unit := range s.units {
		for _, declaration := range unit.Declarations {
			if declaration.Name == identifier.Name && declaration.Container == identifier.Container && (identifier.Kind != "type_identifier" || isTypeSymbol(declaration.Kind)) {
				return map[string]any{"contents": map[string]string{"kind": "markdown", "value": "```java\n" + declaration.Detail + "\n```"}, "range": lspRange(declaration.Selection)}
			}
		}
	}
	if hasUnit {
		text := s.documents[params.TextDocument.URI]
		if docHover := javaparser.ComputeDocHover(text, unit, params.Position.Line, params.Position.Character); docHover != nil {
			return map[string]any{"contents": docHover.Contents, "range": lspRange(docHover.Range)}
		}
	}
	return nil
}

func (s *server) methodInvocationTarget(uri, name string, pos position) (resolvedType, javaparser.MemberInfo, int, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	unit, ok := s.units[uri]
	if !ok {
		return resolvedType{}, javaparser.MemberInfo{}, 0, false
	}
	for _, invocation := range unit.Invocations {
		if invocation.Name != name || pos.Line != invocation.Range.Start.Line ||
			pos.Character < invocation.Range.Start.Character || pos.Character > invocation.Range.End.Character {
			continue
		}
		target := resolvedType{}
		var found bool
		if invocation.Receiver == "" {
			if current := sourceTypeAt(unit, pos); current != nil {
				target, found = resolveSemanticType(s.units, s.typeIndex, unit, current.QualifiedName)
			}
		} else {
			target, found = resolveSemanticType(s.units, s.typeIndex, unit, invocation.Receiver)
			if !found {
				target, found = s.resolveReceiverType(unit, sourceTypeAt(unit, pos), pos, invocation.Receiver)
			}
		}
		if !found {
			continue
		}
		member, found := s.lookupMember(unit, target, name, map[string]bool{})
		if found && member.Kind == "method" {
			if target.Binary != nil {
				s.logger.Printf("resolve method call %s.%s from %s", target.Binary.Name, name, uri)
			}
			return target, member, invocation.ArgumentCount, true
		}
	}
	s.logger.Printf("method call target not found for %s at %s:%d:%d", name, uri, pos.Line, pos.Character)
	return resolvedType{}, javaparser.MemberInfo{}, 0, false
}

func sourceMemberKey(typeInfo classfile.Type, member string, argumentCount int) string {
	return typeInfo.Name + "|" + member + "|" + strconv.Itoa(argumentCount)
}

func (s *server) sourceMemberCachePath() string {
	return filepath.Join(s.root, ".openidea", "cache", "jdtls-go-source-members.json")
}

func (s *server) sourceMemberCacheVersion() string {
	data, err := os.ReadFile(filepath.Join(s.root, "pom.xml"))
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func (s *server) loadSourceMemberCache() {
	data, err := os.ReadFile(s.sourceMemberCachePath())
	if err != nil {
		return
	}
	var cache sourceMemberCache
	if json.Unmarshal(data, &cache) != nil || cache.Version == "" || cache.Version != s.sourceMemberCacheVersion() {
		return
	}
	valid := make(map[string]sourceMemberLocation, len(cache.Members))
	for key, location := range cache.Members {
		parsed, parseErr := url.Parse(location.URI)
		if parseErr != nil || parsed.Scheme != "jdt" || parsed.Query().Get("archive") == "" || parsed.Query().Get("entry") == "" {
			continue
		}
		if _, statErr := os.Stat(parsed.Query().Get("archive")); statErr != nil {
			continue
		}
		valid[key] = location
	}
	s.mu.Lock()
	for key, location := range valid {
		s.sourceMembers[key] = location
	}
	s.mu.Unlock()
	if len(valid) > 0 {
		s.logger.Printf("loaded %d cached Java source declarations", len(valid))
	}
}

func (s *server) saveSourceMemberCache() {
	version := s.sourceMemberCacheVersion()
	if version == "" {
		return
	}
	s.mu.RLock()
	members := make(map[string]sourceMemberLocation, len(s.sourceMembers))
	for key, location := range s.sourceMembers {
		members[key] = location
	}
	s.mu.RUnlock()
	if len(members) == 0 {
		return
	}
	data, err := json.Marshal(sourceMemberCache{Version: version, Members: members})
	if err != nil {
		return
	}
	path := s.sourceMemberCachePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		s.logger.Printf("create Java source cache directory: %v", err)
		return
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		s.logger.Printf("write Java source cache: %v", err)
		return
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		s.logger.Printf("replace Java source cache: %v", err)
	}
}

func sourceReceiverType(unit javaparser.Unit, receiver, method string) string {
	if receiver != "" {
		if strings.Contains(receiver, ".") {
			return receiver
		}
		for _, imported := range unit.Imports {
			if strings.HasSuffix(imported, ".*") {
				continue
			}
			if imported[strings.LastIndex(imported, ".")+1:] == receiver {
				return imported
			}
		}
		return ""
	}
	for _, imported := range unit.Imports {
		if strings.HasSuffix(imported, "."+method) {
			return strings.TrimSuffix(imported, "."+method)
		}
	}
	return ""
}

func (s *server) cachedInvocationLocation(uri, name string, pos position) (sourceMemberLocation, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	unit, ok := s.units[uri]
	if !ok {
		return sourceMemberLocation{}, false
	}
	for _, invocation := range unit.Invocations {
		if invocation.Name != name || invocation.Range.Start.Line != pos.Line ||
			pos.Character < invocation.Range.Start.Character || pos.Character > invocation.Range.End.Character {
			continue
		}
		qualifiedName := sourceReceiverType(unit, invocation.Receiver, invocation.Name)
		if qualifiedName == "" {
			continue
		}
		key := qualifiedName + "|" + invocation.Name + "|" + strconv.Itoa(invocation.ArgumentCount)
		location, found := s.sourceMembers[key]
		if found {
			return location, true
		}
	}
	return sourceMemberLocation{}, false
}

func (s *server) indexReferencedSourceMembers() {
	s.mu.RLock()
	units := make(map[string]javaparser.Unit, len(s.units))
	for uri, unit := range s.units {
		units[uri] = unit
	}
	s.mu.RUnlock()
	seen := make(map[string]bool)
	parsedSources := make(map[string]sourceAttachmentUnit)
	for uri, unit := range units {
		for _, invocation := range unit.Invocations {
			pos := position{Line: invocation.Range.Start.Line, Character: invocation.Range.Start.Character}
			target, member, argumentCount, ok := s.methodInvocationTarget(uri, invocation.Name, pos)
			if !ok || target.Binary == nil {
				continue
			}
			key := sourceMemberKey(*target.Binary, member.Name, argumentCount)
			if seen[key] {
				continue
			}
			seen[key] = true
			s.mu.RLock()
			_, cached := s.sourceMembers[key]
			s.mu.RUnlock()
			if cached {
				continue
			}
			typeKey := target.Binary.Origin + "|" + target.Binary.Name
			parsed, parsedOK := parsedSources[typeKey]
			if !parsedOK {
				s.mu.RLock()
				parsed, parsedOK = sourceAttachmentUnitForType(*target.Binary, s.sourceAttachments)
				s.mu.RUnlock()
				if parsedOK {
					parsedSources[typeKey] = parsed
				}
			}
			location, sourceRange, found := sourceMemberRange(parsed, member.Name, argumentCount)
			if found {
				s.mu.Lock()
				s.sourceMembers[key] = sourceMemberLocation{URI: location, Range: sourceRange}
				s.mu.Unlock()
				s.cacheVirtualContent(location, parsed.Content)
			}
		}
	}
	s.logger.Printf("indexed source declarations for %d referenced Java methods", len(seen))
	s.saveSourceMemberCache()
}

func (s *server) cacheAdjacentBinarySources(binary classfile.Type) {
	archive := binary.Origin
	if separator := strings.Index(archive, "!/"); separator >= 0 {
		archive = archive[:separator]
	}
	if !strings.HasSuffix(strings.ToLower(archive), ".jar") {
		return
	}
	sourceArchive := strings.TrimSuffix(archive, filepath.Ext(archive)) + "-sources.jar"
	if _, err := os.Stat(sourceArchive); err == nil {
		s.mu.Lock()
		s.sourceAttachments[filepath.Clean(archive)] = sourceArchive
		s.mu.Unlock()
	}
}

func (s *server) prefetchModuleSources(module, scope string) error {
	s.sourceResolveMu.Lock()
	defer s.sourceResolveMu.Unlock()
	s.mu.RLock()
	resolved := s.sourcesResolved[module]
	s.mu.RUnlock()
	if resolved {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	attachments, err := maven.ResolveSourceAttachments(ctx, s.root, module, scope)
	if err != nil {
		return err
	}
	s.mu.Lock()
	for binary, source := range attachments {
		s.sourceAttachments[binary] = source
	}
	s.sourcesResolved[module] = true
	s.mu.Unlock()
	s.logger.Printf("indexed %d Maven source attachments for %s", len(attachments), module)
	return nil
}

func (s *server) definition(raw json.RawMessage) any {
	var params struct {
		TextDocument textDocument `json:"textDocument"`
		Position     position     `json:"position"`
	}
	_ = json.Unmarshal(raw, &params)
	identifier, ok := s.identifierNodeAt(params.TextDocument.URI, params.Position)
	if !ok {
		s.logger.Printf("definition: no identifier at %s:%d:%d", params.TextDocument.URI, params.Position.Line, params.Position.Character)
		return []any{}
	}
	if location, found := s.cachedInvocationLocation(params.TextDocument.URI, identifier.Name, params.Position); found {
		return []any{map[string]any{"uri": location.URI, "range": lspRange(location.Range)}}
	}
	s.logger.Printf("definition: %s at %s:%d:%d", identifier.Name, params.TextDocument.URI, params.Position.Line, params.Position.Character)
	if target, member, argumentCount, found := s.methodInvocationTarget(params.TextDocument.URI, identifier.Name, params.Position); found {
		if target.Source != nil {
			s.mu.RLock()
			for sourceURI, sourceUnit := range s.units {
				for _, typeInfo := range sourceUnit.Types {
					if typeInfo.QualifiedName != target.Source.QualifiedName {
						continue
					}
					for _, declared := range typeInfo.Members {
						if declared.Name == member.Name && declared.Kind == "method" {
							s.mu.RUnlock()
							return []any{map[string]any{"uri": sourceURI, "range": lspRange(declared.Selection)}}
						}
					}
				}
			}
			s.mu.RUnlock()
		}
		if target.Binary != nil {
			s.cacheAdjacentBinarySources(*target.Binary)
			key := sourceMemberKey(*target.Binary, member.Name, argumentCount)
			s.mu.RLock()
			cached, hasSource := s.sourceMembers[key]
			if !hasSource {
				cached.URI, cached.Range, hasSource = sourceAttachmentMemberLocation(*target.Binary, member.Name, argumentCount, s.sourceAttachments)
			}
			s.mu.RUnlock()
			if hasSource {
				s.mu.Lock()
				s.sourceMembers[key] = cached
				s.mu.Unlock()
			}
			s.logger.Printf("definition: binary method %s.%s mapped source=%t uri=%s", target.Binary.Name, member.Name, hasSource, cached.URI)
			if hasSource {
				return []any{map[string]any{"uri": cached.URI, "range": lspRange(cached.Range)}}
			}
			s.mu.RLock()
			indexing := s.indexing
			s.mu.RUnlock()
			if indexing {
				return []any{}
			}
			return []any{map[string]any{"uri": classURI(target.Binary.Origin), "range": span(0, 0, 0, 0)}}
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	unit, hasUnit := s.units[params.TextDocument.URI]
	if hasUnit && identifier.Kind == "type_identifier" {
		if resolved, found := resolveSemanticType(s.units, s.typeIndex, unit, identifier.Name); found {
			if resolved.Source != nil {
				for uri, sourceUnit := range s.units {
					for _, typeInfo := range sourceUnit.Types {
						if typeInfo.QualifiedName == resolved.Name {
							return []any{map[string]any{"uri": uri, "range": lspRange(typeInfo.Selection)}}
						}
					}
				}
			}
			if resolved.Binary != nil {
				if uri, sourceRange, found := sourceAttachmentLocation(*resolved.Binary, s.sourceAttachments); found {
					return []any{map[string]any{"uri": uri, "range": lspRange(sourceRange)}}
				}
				return []any{map[string]any{"uri": classURI(resolved.Binary.Origin), "range": span(0, 0, 0, 0)}}
			}
		}
	}
	if hasUnit {
		if current := sourceTypeAt(unit, params.Position); current != nil {
			if member, found := s.lookupMember(unit, resolvedType{Name: current.QualifiedName, Source: current}, identifier.Name, map[string]bool{}); found {
				for uri, sourceUnit := range s.units {
					for _, typeInfo := range sourceUnit.Types {
						if typeInfo.QualifiedName != current.QualifiedName {
							continue
						}
						for _, declared := range typeInfo.Members {
							if declared.Name == member.Name {
								return []any{map[string]any{"uri": uri, "range": lspRange(declared.Selection)}}
							}
						}
					}
				}
			}
		}
	}
	locations := make([]map[string]any, 0)
	for uri, unit := range s.units {
		for _, declaration := range unit.Declarations {
			if declaration.Name == identifier.Name && declaration.Container == identifier.Container && (identifier.Kind != "type_identifier" || isTypeSymbol(declaration.Kind)) {
				locations = append(locations, map[string]any{"uri": uri, "range": lspRange(declaration.Selection)})
			}
		}
	}
	if len(locations) == 0 {
		for uri, sourceUnit := range s.units {
			for _, typeInfo := range sourceUnit.Types {
				for _, declared := range typeInfo.Members {
					if declared.Name == identifier.Name {
						locations = append(locations, map[string]any{"uri": uri, "range": lspRange(declared.Selection)})
					}
				}
			}
		}
	}
	return locations
}

func sourceTypeDetail(value javaparser.TypeInfo) string {
	kind := strings.TrimSuffix(value.Kind, "_declaration")
	kind = strings.TrimSuffix(kind, "_type")
	detail := kind + " " + value.QualifiedName
	if value.Super != "" {
		detail += " extends " + value.Super
	}
	if len(value.Interfaces) > 0 {
		if kind == "interface" {
			detail += " extends "
		} else {
			detail += " implements "
		}
		detail += strings.Join(value.Interfaces, ", ")
	}
	return detail
}

func (s *server) references(raw json.RawMessage, highlights bool) []map[string]any {
	var params struct {
		TextDocument textDocument `json:"textDocument"`
		Position     position     `json:"position"`
		Context      struct {
			IncludeDeclaration bool `json:"includeDeclaration"`
		} `json:"context"`
	}
	_ = json.Unmarshal(raw, &params)
	identifier, ok := s.identifierNodeAt(params.TextDocument.URI, params.Position)
	if !ok {
		return []map[string]any{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	currentUnit, ok := s.units[params.TextDocument.URI]
	if !ok {
		return []map[string]any{}
	}
	currentType := sourceTypeAt(currentUnit, params.Position)
	targetType := ""
	if identifier.Kind == "type_identifier" {
		if resolved, found := resolveSemanticType(s.units, s.typeIndex, currentUnit, identifier.Name); found {
			targetType = resolved.Name
		}
	}
	locations := make([]map[string]any, 0)
	for uri, unit := range s.units {
		if highlights && uri != params.TextDocument.URI {
			continue
		}
		for _, reference := range unit.Identifiers {
			if reference.Name != identifier.Name {
				continue
			}
			if targetType != "" {
				if reference.Kind != "type_identifier" {
					continue
				}
				if resolved, found := resolveSemanticType(s.units, s.typeIndex, unit, reference.Name); !found || resolved.Name != targetType {
					continue
				}
			} else {
				if currentType == nil || reference.Container != currentType.Name {
					continue
				}
				if candidateType := sourceTypeAt(unit, position{Line: reference.Range.Start.Line, Character: reference.Range.Start.Character}); candidateType == nil || candidateType.QualifiedName != currentType.QualifiedName {
					continue
				}
			}
			if highlights {
				hKind := 2 // Read
				if reference.Kind == "assignment_expression" || reference.Kind == "variable_declarator" {
					hKind = 3 // Write
				}
				locations = append(locations, map[string]any{"range": lspRange(reference.Range), "kind": hKind})
			} else {
				locations = append(locations, map[string]any{"uri": uri, "range": lspRange(reference.Range)})
			}
		}
		if !highlights && params.Context.IncludeDeclaration {
			for _, declaration := range unit.Declarations {
				if declaration.Name == identifier.Name && (targetType != "" && isTypeSymbol(declaration.Kind) || targetType == "" && currentType != nil && declaration.Container == currentType.Name) {
					locations = append(locations, map[string]any{"uri": uri, "range": lspRange(declaration.Selection)})
				}
			}
		}
	}
	return locations
}

func (s *server) workspaceSymbols(raw json.RawMessage) []map[string]any {
	var params struct {
		Query string `json:"query"`
	}
	_ = json.Unmarshal(raw, &params)
	query := strings.ToLower(params.Query)
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]map[string]any, 0)
	for uri, unit := range s.units {
		for _, symbol := range unit.Symbols {
			if query != "" && !strings.Contains(strings.ToLower(symbol.Name), query) {
				continue
			}
			items = append(items, map[string]any{"name": symbol.Name, "kind": symbol.Kind, "containerName": symbol.Container, "location": map[string]any{"uri": uri, "range": lspRange(symbol.Selection)}})
			if len(items) >= 500 {
				return items
			}
		}
	}
	return items
}

func (s *server) implementation(raw json.RawMessage) []map[string]any {
	var params struct {
		TextDocument textDocument `json:"textDocument"`
		Position     position     `json:"position"`
	}
	_ = json.Unmarshal(raw, &params)
	identifier, ok := s.identifierNodeAt(params.TextDocument.URI, params.Position)
	if !ok {
		return []map[string]any{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	unit := s.units[params.TextDocument.URI]
	items := make([]map[string]any, 0)

	if identifier.Kind == "type_identifier" {
		target, ok := resolveSemanticType(s.units, s.typeIndex, unit, identifier.Name)
		if !ok {
			return []map[string]any{}
		}
		for uri, sourceUnit := range s.units {
			for _, candidate := range sourceUnit.Types {
				parents := append([]string{candidate.Super}, candidate.Interfaces...)
				for _, parent := range parents {
					resolved, found := resolveSemanticType(s.units, s.typeIndex, sourceUnit, parent)
					if found && resolved.Name == target.Name {
						items = append(items, map[string]any{"uri": uri, "range": lspRange(candidate.Selection)})
						break
					}
				}
			}
		}
		return items
	}

	// 用户点击方法/字段跳转实现：在继承类或实现类中查找同名成员
	current := sourceTypeAt(unit, params.Position)
	for uri, sourceUnit := range s.units {
		for _, candidate := range sourceUnit.Types {
			isSubtype := false
			if current != nil {
				for _, parent := range append([]string{candidate.Super}, candidate.Interfaces...) {
					if parent == current.Name || parent == current.QualifiedName {
						isSubtype = true
						break
					}
				}
			}
			for _, m := range candidate.Members {
				if m.Name == identifier.Name && (isSubtype || current == nil || candidate.QualifiedName != current.QualifiedName) {
					items = append(items, map[string]any{"uri": uri, "range": lspRange(m.Selection)})
				}
			}
		}
	}

	if len(items) == 0 {
		for uri, sourceUnit := range s.units {
			for _, candidate := range sourceUnit.Types {
				for _, m := range candidate.Members {
					if m.Name == identifier.Name {
						items = append(items, map[string]any{"uri": uri, "range": lspRange(m.Selection)})
					}
				}
			}
		}
	}

	return items
}

func (s *server) signatureHelp(raw json.RawMessage) any {
	var params struct {
		TextDocument textDocument `json:"textDocument"`
		Position     position     `json:"position"`
	}
	_ = json.Unmarshal(raw, &params)
	text := s.document(params.TextDocument.URI)
	lines := strings.Split(text, "\n")
	if params.Position.Line < 0 || params.Position.Line >= len(lines) {
		return nil
	}
	line := []rune(lines[params.Position.Line])
	end := min(params.Position.Character, len(line))
	if end < 0 {
		return nil
	}
	before := string(line[:end])
	open := strings.LastIndex(before, "(")
	if open < 0 {
		return nil
	}
	callee := strings.TrimSpace(before[:open])
	start := len(callee)
	for start > 0 {
		ch := rune(callee[start-1])
		if !unicode.IsLetter(ch) && !unicode.IsDigit(ch) && ch != '_' && ch != '$' && ch != '.' {
			break
		}
		start--
	}
	callee = callee[start:]
	if callee == "" {
		return nil
	}
	activeParameter := strings.Count(before[open+1:], ",")
	s.mu.RLock()
	defer s.mu.RUnlock()
	unit, ok := s.units[params.TextDocument.URI]
	if !ok {
		return nil
	}
	current := sourceTypeAt(unit, params.Position)
	lastDot := strings.LastIndex(callee, ".")
	methodName, qualifier := callee, ""
	if lastDot >= 0 {
		methodName, qualifier = callee[lastDot+1:], callee[:lastDot]
	}
	var owner resolvedType
	var found bool
	if qualifier == "" {
		if current == nil {
			return nil
		}
		owner = resolvedType{Name: current.QualifiedName, Source: current}
		found = true
	} else {
		owner, found = s.resolveReceiverType(unit, current, params.Position, qualifier)
	}
	if !found {
		return nil
	}
	var signatures []map[string]any
	if owner.Source != nil {
		for _, member := range owner.Source.Members {
			if member.Name == methodName && member.Kind == "method" {
				parameters := make([]map[string]any, 0, len(member.Parameters))
				for _, parameter := range member.Parameters {
					parameters = append(parameters, map[string]any{"label": parameter.Type + " " + parameter.Name})
				}
				signatures = append(signatures, map[string]any{"label": member.Signature, "parameters": parameters})
			}
		}
	}
	if owner.Binary != nil {
		for _, member := range owner.Binary.Methods {
			if member.Name != methodName || !strings.HasPrefix(member.Descriptor, "(") {
				continue
			}
			parameterTypes := descriptorParameters(member.Descriptor)
			parameters := make([]map[string]any, 0, len(parameterTypes))
			labels := make([]string, 0, len(parameterTypes))
			for _, parameterType := range parameterTypes {
				parameters = append(parameters, map[string]any{"label": parameterType})
				labels = append(labels, parameterType)
			}
			returnType := binaryMemberType(member)
			label := member.Name + "(" + strings.Join(labels, ", ") + ")"
			if returnType != "" && returnType != "void" {
				label = returnType + " " + label
			}
			signatures = append(signatures, map[string]any{"label": label, "parameters": parameters})
		}
	}
	if len(signatures) == 0 {
		return nil
	}
	return map[string]any{"signatures": signatures, "activeSignature": 0, "activeParameter": activeParameter}
}

func descriptorParameters(descriptor string) []string {
	if !strings.HasPrefix(descriptor, "(") {
		return nil
	}
	descriptor = descriptor[1:]
	var parameters []string
	for len(descriptor) > 0 && descriptor[0] != ')' {
		start := 0
		for start < len(descriptor) && descriptor[start] == '[' {
			start++
		}
		if start >= len(descriptor) {
			break
		}
		end := start + 1
		if descriptor[start] == 'L' {
			if index := strings.IndexByte(descriptor[start:], ';'); index >= 0 {
				end = start + index + 1
			}
		}
		if end > len(descriptor) {
			break
		}
		parameters = append(parameters, javaDescriptorType(descriptor[:end]))
		descriptor = descriptor[end:]
	}
	return parameters
}

func sourceAttachmentLocation(typeInfo classfile.Type, attachments map[string]string) (string, javaparser.Range, bool) {
	archiveSeparator := strings.Index(typeInfo.Origin, "!/")
	if archiveSeparator < 0 || typeInfo.SourceFile == "" {
		return "", javaparser.Range{}, false
	}
	binaryArchive := filepath.Clean(typeInfo.Origin[:archiveSeparator])
	sourceArchive := attachments[binaryArchive]
	if sourceArchive == "" {
		return "", javaparser.Range{}, false
	}
	packagePath := filepath.ToSlash(filepath.Dir(strings.ReplaceAll(typeInfo.Name, ".", "/")))
	entry := filepath.ToSlash(filepath.Join(packagePath, typeInfo.SourceFile))
	content, err := maven.ReadArchiveFile(sourceArchive, entry)
	if err != nil {
		return "", javaparser.Range{}, false
	}
	unit, err := javaparser.Parse(content)
	if err != nil {
		return "", javaparser.Range{}, false
	}
	name := strings.TrimSuffix(typeInfo.SourceFile, filepath.Ext(typeInfo.SourceFile))
	var selection javaparser.Range
	for _, symbol := range unit.Symbols {
		if symbol.Name == name && isTypeSymbol(symbol.Kind) {
			selection = symbol.Selection
			break
		}
	}
	if selection.End == (javaparser.Position{}) {
		selection = spanRange(0, 0, 0, 0)
	}
	parsed, _ := url.Parse("jdt://contents/" + url.PathEscape(filepath.Base(sourceArchive)) + "/" + url.PathEscape(entry))
	query := parsed.Query()
	query.Set("archive", sourceArchive)
	query.Set("entry", entry)
	parsed.RawQuery = query.Encode()
	return parsed.String(), selection, true
}

func sourceAttachmentMemberLocation(typeInfo classfile.Type, member string, argumentCount int, attachments map[string]string) (string, javaparser.Range, bool) {
	parsed, ok := sourceAttachmentUnitForType(typeInfo, attachments)
	if !ok {
		return "", javaparser.Range{}, false
	}
	return sourceMemberRange(parsed, member, argumentCount)
}

func sourceAttachmentUnitForType(typeInfo classfile.Type, attachments map[string]string) (sourceAttachmentUnit, bool) {
	archiveSeparator := strings.Index(typeInfo.Origin, "!/")
	if archiveSeparator < 0 || typeInfo.SourceFile == "" {
		return sourceAttachmentUnit{}, false
	}
	binaryArchive := filepath.Clean(typeInfo.Origin[:archiveSeparator])
	sourceArchive := attachments[binaryArchive]
	if sourceArchive == "" {
		return sourceAttachmentUnit{}, false
	}
	packagePath := filepath.ToSlash(filepath.Dir(strings.ReplaceAll(typeInfo.Name, ".", "/")))
	entry := filepath.ToSlash(filepath.Join(packagePath, typeInfo.SourceFile))
	content, err := maven.ReadArchiveFile(sourceArchive, entry)
	if err != nil {
		return sourceAttachmentUnit{}, false
	}
	unit, err := javaparser.Parse(content)
	if err != nil {
		return sourceAttachmentUnit{}, false
	}
	name := strings.TrimSuffix(typeInfo.SourceFile, filepath.Ext(typeInfo.SourceFile))
	var classRange javaparser.Range
	for _, sourceType := range unit.Types {
		if sourceType.Name != name {
			continue
		}
		classRange = sourceType.Selection
		break
	}
	if classRange.End == (javaparser.Position{}) {
		classRange = spanRange(0, 0, 0, 0)
	}
	parsed, _ := url.Parse("jdt://contents/" + url.PathEscape(filepath.Base(sourceArchive)) + "/" + url.PathEscape(entry))
	query := parsed.Query()
	query.Set("archive", sourceArchive)
	query.Set("entry", entry)
	parsed.RawQuery = query.Encode()
	return sourceAttachmentUnit{URI: parsed.String(), TypeName: name, ClassRange: classRange, Unit: unit, Content: string(content)}, true
}

func sourceMemberRange(parsed sourceAttachmentUnit, member string, argumentCount int) (string, javaparser.Range, bool) {
	if parsed.URI == "" {
		return "", javaparser.Range{}, false
	}
	var first *javaparser.MemberInfo
	for _, sourceType := range parsed.Unit.Types {
		if sourceType.Name != parsed.TypeName {
			continue
		}
		for i := range sourceType.Members {
			candidate := &sourceType.Members[i]
			if candidate.Name != member || candidate.Kind != "method" {
				continue
			}
			if first == nil {
				first = candidate
			}
			if len(candidate.Parameters) == argumentCount {
				return parsed.URI, candidate.Selection, true
			}
		}
	}
	if first != nil {
		return parsed.URI, first.Selection, true
	}
	return parsed.URI, parsed.ClassRange, true
}

func spanRange(startLine, startCharacter, endLine, endCharacter int) javaparser.Range {
	return javaparser.Range{Start: javaparser.Position{Line: startLine, Character: startCharacter}, End: javaparser.Position{Line: endLine, Character: endCharacter}}
}

func resolveType(index *classfile.Index, unit javaparser.Unit, name string) (classfile.Type, bool) {
	if index == nil {
		return classfile.Type{}, false
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
		}
		if imported == name || strings.HasSuffix(imported, "."+name) {
			candidates = append(candidates, imported)
		}
	}
	candidates = append(candidates, "java.lang."+name)
	for _, candidate := range candidates {
		if value, ok := index.Types[candidate]; ok {
			return value, true
		}
	}
	var match classfile.Type
	for _, value := range index.Types {
		if value.Name == name || strings.HasSuffix(value.Name, "."+name) {
			if match.Name != "" {
				return classfile.Type{}, false
			}
			match = value
		}
	}
	return match, match.Name != ""
}

func classDetail(value classfile.Type) string {
	modifiers := []string{}
	for flag, name := range map[uint16]string{0x0001: "public", 0x0010: "final", 0x0400: "abstract", 0x0200: "interface", 0x2000: "@interface", 0x4000: "enum"} {
		if value.Access&flag != 0 {
			modifiers = append(modifiers, name)
		}
	}
	kind := "class"
	if value.Access&0x2000 != 0 {
		kind = "@interface"
	} else if value.Access&0x0200 != 0 {
		kind = "interface"
	} else if value.Access&0x4000 != 0 {
		kind = "enum"
	}
	_ = modifiers
	detail := kind + " " + value.Name
	if value.Super != "" && value.Super != "java.lang.Object" && kind == "class" {
		detail += " extends " + value.Super
	}
	if len(value.Interfaces) > 0 {
		if kind == "interface" {
			detail += " extends "
		} else {
			detail += " implements "
		}
		detail += strings.Join(value.Interfaces, ", ")
	}
	return detail
}

func classURI(origin string) string {
	if separator := strings.Index(origin, "!/"); separator >= 0 {
		archive := origin[:separator]
		entry := origin[separator+2:]
		return "jar:" + fileURI(archive) + "!/" + entry
	}
	return fileURI(origin)
}

func isTypeSymbol(kind int) bool {
	return kind == 5 || kind == 10 || kind == 11
}
func uriPath(uri string) string {
	if path, ok := fileuri.FromURI(uri); ok {
		return path
	}
	return uri
}
func currentPrefix(text string, pos position) string {
	lines := strings.Split(text, "\n")
	if pos.Line < 0 || pos.Line >= len(lines) {
		return ""
	}
	if pos.Character < 0 {
		return ""
	}
	runes := []rune(lines[pos.Line])
	end, units := 0, 0
	for end < len(runes) && units < pos.Character {
		width := 1
		if runes[end] > 0xffff {
			width = 2
		}
		if units+width > pos.Character {
			break
		}
		units += width
		end++
	}
	start := end
	for start > 0 {
		r := runes[start-1]
		if r != '_' && r != '$' && r != '-' && r != '.' && !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			break
		}
		start--
	}
	return string(runes[start:end])
}
func lineColumn(text string, offset int) (int, int) {
	line, last := 0, -1
	for i := 0; i < offset; i++ {
		if text[i] == '\n' {
			line++
			last = i
		}
	}
	return line, offset - last - 1
}
func span(sl, sc, el, ec int) map[string]any {
	return map[string]any{"start": map[string]int{"line": sl, "character": sc}, "end": map[string]int{"line": el, "character": ec}}
}

func readFrame(r *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		if strings.HasPrefix(strings.ToLower(line), "content-length:") {
			value := strings.TrimSpace(line[strings.IndexByte(line, ':')+1:])
			length, _ = strconv.Atoi(value)
		}
	}
	if length < 0 || length > 16*1024*1024 {
		return nil, fmt.Errorf("invalid Content-Length %d", length)
	}
	body := make([]byte, length)
	_, err := io.ReadFull(r, body)
	return body, err
}

func writeFrame(w *bufio.Writer, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}
	if _, err := w.Write(body); err != nil {
		return err
	}
	return w.Flush()
}

func (s *server) onTypeFormatting(raw json.RawMessage) []javaparser.TextEdit {
	var p struct {
		TextDocument textDocument                 `json:"textDocument"`
		Position     position                     `json:"position"`
		Ch           string                       `json:"ch"`
		Options      javaparser.FormattingOptions `json:"options"`
	}
	_ = json.Unmarshal(raw, &p)
	text := s.document(p.TextDocument.URI)
	return javaparser.ComputeOnTypeFormatting(text, p.Position.Line, p.Ch, p.Options)
}

func (s *server) codeLens(raw json.RawMessage) []javaparser.CodeLens {
	var p struct {
		TextDocument textDocument `json:"textDocument"`
	}
	_ = json.Unmarshal(raw, &p)
	unit := s.unit(p.TextDocument.URI)
	return javaparser.ComputeCodeLenses(p.TextDocument.URI, unit)
}
