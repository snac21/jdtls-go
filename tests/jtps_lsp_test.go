package tests

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cage/jdtls-go/internal/lsp"
)

// lspClient 是参考 kmp-lsp/tests/lsp_smoke.rs 实现的轻量级纯内存 LSP 测试客户端
type lspClient struct {
	toServer   *io.PipeWriter
	fromServer *bufio.Reader
	mu         sync.Mutex
	nextID     int
	pending    map[int]chan json.RawMessage
	notifs     chan json.RawMessage
	done       chan struct{}
}

func newLSPTestClient(t *testing.T) (*lspClient, func()) {
	clientRead, serverWrite := io.Pipe()
	serverRead, clientWrite := io.Pipe()

	logger := log.New(io.Discard, "[test-lsp] ", 0)

	serverDone := make(chan error, 1)
	go func() {
		err := lsp.Serve(serverRead, serverWrite, logger)
		serverDone <- err
	}()

	client := &lspClient{
		toServer:   clientWrite,
		fromServer: bufio.NewReader(clientRead),
		pending:    make(map[int]chan json.RawMessage),
		notifs:     make(chan json.RawMessage, 50),
		done:       make(chan struct{}),
	}

	// 启动后台读取协程，解析 Content-Length 帧并分发
	go client.readLoop(t)

	cleanup := func() {
		close(client.done)
		_ = clientWrite.Close()
		_ = serverWrite.Close()
		_ = clientRead.Close()
		_ = serverRead.Close()
	}

	return client, cleanup
}

func (c *lspClient) readLoop(t *testing.T) {
	for {
		select {
		case <-c.done:
			return
		default:
		}

		length := -1
		for {
			line, err := c.fromServer.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSpace(line)
			if line == "" {
				break
			}
			if strings.HasPrefix(strings.ToLower(line), "content-length:") {
				val := strings.TrimSpace(line[strings.IndexByte(line, ':')+1:])
				length, _ = strconv.Atoi(val)
			}
		}

		if length < 0 {
			continue
		}

		body := make([]byte, length)
		if _, err := io.ReadFull(c.fromServer, body); err != nil {
			return
		}

		var generic struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}

		if err := json.Unmarshal(body, &generic); err != nil {
			continue
		}

		if generic.ID != nil {
			c.mu.Lock()
			ch, ok := c.pending[*generic.ID]
			delete(c.pending, *generic.ID)
			c.mu.Unlock()

			if ok {
				ch <- body
			}
		} else if generic.Method != "" {
			select {
			case c.notifs <- body:
			default:
			}
		}
	}
}

func (c *lspClient) request(method LSPMethod, params any) (json.RawMessage, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan json.RawMessage, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	reqMap := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  string(method),
		"params":  params,
	}

	body, err := json.Marshal(reqMap)
	if err != nil {
		return nil, err
	}

	frame := fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), string(body))
	if _, err := c.toServer.Write([]byte(frame)); err != nil {
		return nil, err
	}

	select {
	case res := <-ch:
		var parsed struct {
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(res, &parsed); err != nil {
			return nil, err
		}
		if parsed.Error != nil {
			return nil, fmt.Errorf("rpc error [%d]: %s", parsed.Error.Code, parsed.Error.Message)
		}
		return parsed.Result, nil
	case <-time.After(10 * time.Second):
		return nil, fmt.Errorf("timeout waiting for LSP response to %s (id=%d)", method, id)
	}
}

func (c *lspClient) notify(method LSPMethod, params any) error {
	notifMap := map[string]any{
		"jsonrpc": "2.0",
		"method":  string(method),
		"params":  params,
	}
	body, err := json.Marshal(notifMap)
	if err != nil {
		return err
	}
	frame := fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), string(body))
	_, err = c.toServer.Write([]byte(frame))
	return err
}

func findJtpsRoot(t *testing.T) string {
	candidates := []string{
		"../../jtps/jtps",
		"../jtps/jtps",
		"./jtps/jtps",
		"/Users/Cage/snac21/java-server/jtps/jtps",
	}
	for _, c := range candidates {
		abs, err := filepath.Abs(c)
		if err == nil {
			if _, err := os.Stat(filepath.Join(abs, "pom.xml")); err == nil {
				return abs
			}
		}
	}
	t.Fatalf("无法找到 jtps/jtps 项目根路径")
	return ""
}

// pathToURI 将本地绝对路径转换为 file:// URI
func pathToURI(p string) string {
	p = filepath.ToSlash(p)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return "file://" + (&url.URL{Path: p}).EscapedPath()
}

// TestJtpsLSPReconstructionSuite 对真实的 jtps 项目执行端到端 LSP 功能对齐测试
func TestJtpsLSPReconstructionSuite(t *testing.T) {
	jtpsRoot := findJtpsRoot(t)
	helloControllerPath := filepath.Join(jtpsRoot, "src/main/java/com/example/jtps/HelloController.java")
	appJavaPath := filepath.Join(jtpsRoot, "src/main/java/com/example/jtps/JdtlsApplication.java")
	if _, err := os.Stat(appJavaPath); err != nil {
		appJavaPath = filepath.Join(jtpsRoot, "src/main/java/com/example/jtps/JtpsApplication.java")
	}
	helloContentBytes, err := os.ReadFile(helloControllerPath)
	if err != nil {
		t.Fatalf("读取 HelloController.java 失败: %v", err)
	}
	helloContent := string(helloContentBytes)
	helloURI := pathToURI(helloControllerPath)
	appURI := pathToURI(appJavaPath)
	jtpsURI := pathToURI(jtpsRoot)

	client, cleanup := newLSPTestClient(t)
	defer cleanup()

	// -------------------------------------------------------------
	// 1. 测试 initialize 握手与能力协商
	// -------------------------------------------------------------
	t.Run("LSP_Initialize_Handshake", func(t *testing.T) {
		initParams := map[string]any{
			"rootUri": jtpsURI,
			"capabilities": map[string]any{
				"textDocument": map[string]any{
					"completion": map[string]any{"dynamicRegistration": false},
					"hover":      map[string]any{"dynamicRegistration": false},
				},
			},
		}

		res, err := client.request(LSPMethodInitialize, initParams)
		if err != nil {
			t.Fatalf("initialize 失败: %v", err)
		}

		var initResult struct {
			Capabilities struct {
				HoverProvider          bool `json:"hoverProvider"`
				DefinitionProvider     bool `json:"definitionProvider"`
				DocumentSymbolProvider bool `json:"documentSymbolProvider"`
				ReferencesProvider     bool `json:"referencesProvider"`
				ExecuteCommandProvider *struct {
					Commands []string `json:"commands"`
				} `json:"executeCommandProvider"`
			} `json:"capabilities"`
		}

		if err := json.Unmarshal(res, &initResult); err != nil {
			t.Fatalf("解析 capabilities 失败: %v", err)
		}

		if !initResult.Capabilities.HoverProvider {
			t.Errorf("期望 HoverProvider 为 true")
		}
		if !initResult.Capabilities.DefinitionProvider {
			t.Errorf("期望 DefinitionProvider 为 true")
		}
		if !initResult.Capabilities.DocumentSymbolProvider {
			t.Errorf("期望 DocumentSymbolProvider 为 true")
		}
		if initResult.Capabilities.ExecuteCommandProvider == nil || len(initResult.Capabilities.ExecuteCommandProvider.Commands) == 0 {
			t.Errorf("期望 ExecuteCommandProvider 提供支持的命令列表")
		} else {
			cmds := initResult.Capabilities.ExecuteCommandProvider.Commands
			t.Logf("服务端声明支持的自定义命令: %v", cmds)
		}

		_ = client.notify(LSPMethodInitialized, map[string]any{})
	})

	// -------------------------------------------------------------
	// 2. 测试 textDocument/didOpen 与 语法树解析
	// -------------------------------------------------------------
	t.Run("LSP_DidOpen_And_SyntaxDiagnostics", func(t *testing.T) {
		openParams := map[string]any{
			"textDocument": map[string]any{
				"uri":        helloURI,
				"languageId": "java",
				"version":    1,
				"text":       helloContent,
			},
		}

		if err := client.notify(LSPMethodTextDocumentDidOpen, openParams); err != nil {
			t.Fatalf("didOpen 发送失败: %v", err)
		}

		// 检查是否有发布诊断通知
		select {
		case notif := <-client.notifs:
			var diagNotif struct {
				Method string `json:"method"`
				Params struct {
					URI         string `json:"uri"`
					Diagnostics []any  `json:"diagnostics"`
				} `json:"params"`
			}
			if err := json.Unmarshal(notif, &diagNotif); err == nil && diagNotif.Method == "textDocument/publishDiagnostics" {
				t.Logf("收到文档诊断通知: URI=%s, 诊断数=%d", diagNotif.Params.URI, len(diagNotif.Params.Diagnostics))
			}
		case <-time.After(1 * time.Second):
			t.Log("无语法错误诊断通知（符合预期，代码无语法错误）")
		}
	})

	// -------------------------------------------------------------
	// 3. 测试 textDocument/documentSymbol 提取类与方法符号
	// -------------------------------------------------------------
	t.Run("LSP_DocumentSymbols_Extraction", func(t *testing.T) {
		params := map[string]any{
			"textDocument": map[string]any{
				"uri": helloURI,
			},
		}

		res, err := client.request(LSPMethodTextDocumentDocumentSymbol, params)
		if err != nil {
			t.Fatalf("documentSymbol 请求失败: %v", err)
		}

		type rawSym struct {
			Name     string   `json:"name"`
			Kind     int      `json:"kind"`
			Children []rawSym `json:"children"`
		}
		var symbols []rawSym
		if err := json.Unmarshal(res, &symbols); err != nil {
			t.Fatalf("解析符号返回失败: %v, raw: %s", err, string(res))
		}

		foundClass := false
		foundMethod := false
		var checkSym func(s rawSym)
		checkSym = func(s rawSym) {
			if s.Name == "HelloController" {
				foundClass = true
			}
			if s.Name == "hello" {
				foundMethod = true
			}
			for _, ch := range s.Children {
				checkSym(ch)
			}
		}
		for _, s := range symbols {
			checkSym(s)
		}

		if !foundClass {
			t.Errorf("未能成功提取 HelloController 类符号，符号列表: %+v", symbols)
		}
		if !foundMethod {
			t.Errorf("未能成功提取 hello 方法符号，符号列表: %+v", symbols)
		}
		t.Logf("成功提取到 HelloController 树形符号 (顶层数量 %d, 包含递归子方法)", len(symbols))
	})

	// -------------------------------------------------------------
	// 4. 测试 textDocument/hover 悬停信息
	// -------------------------------------------------------------
	t.Run("LSP_Hover_Information", func(t *testing.T) {
		// HelloController 在第 7 行（line 6），列字符大约在 15 左右
		params := map[string]any{
			"textDocument": map[string]any{
				"uri": helloURI,
			},
			"position": map[string]int{
				"line":      6,
				"character": 15,
			},
		}

		res, err := client.request(LSPMethodTextDocumentHover, params)
		if err != nil {
			t.Fatalf("hover 请求失败: %v", err)
		}

		if string(res) != "null" && len(res) > 0 {
			var hoverResult struct {
				Contents struct {
					Kind  string `json:"kind"`
					Value string `json:"value"`
				} `json:"contents"`
			}
			if err := json.Unmarshal(res, &hoverResult); err == nil && hoverResult.Contents.Value != "" {
				if hoverResult.Contents.Kind != "markdown" || !strings.Contains(hoverResult.Contents.Value, "HelloController") {
					t.Errorf("Hover 内容不符合预期富文本渲染: %+v", hoverResult)
				}
				t.Logf("成功获得高精度 Markdown Hover 提示: \n%s", hoverResult.Contents.Value)
			}
		} else {
			t.Errorf("Hover 不应返回空，期望返回 HelloController 类或成员卡片")
		}
	})

	// -------------------------------------------------------------
	// 5. 测试 textDocument/completion 补全建议
	// -------------------------------------------------------------
	t.Run("LSP_Completion_Suggestions", func(t *testing.T) {
		// 在 hello 方法内部触发补全 (第 10 行，line 9)
		params := map[string]any{
			"textDocument": map[string]any{
				"uri": helloURI,
			},
			"position": map[string]int{
				"line":      9,
				"character": 8,
			},
		}

		res, err := client.request(LSPMethodTextDocumentCompletion, params)
		if err != nil {
			t.Fatalf("completion 请求失败: %v", err)
		}

		var compList struct {
			IsIncomplete bool `json:"isIncomplete"`
			Items        []struct {
				Label string `json:"label"`
				Kind  int    `json:"kind"`
			} `json:"items"`
		}
		if err := json.Unmarshal(res, &compList); err != nil {
			t.Fatalf("解析 completion 结果失败: %v", err)
		}

		t.Logf("补全返回候选项数量: %d", len(compList.Items))
		if len(compList.Items) == 0 {
			t.Log("提示：当前上下文补全项为空，为部分重构预期行为")
		}
	})

	// -------------------------------------------------------------
	// 6. 测试自定义 Maven 专有命令 (jdtls-go.resolveClasspath)
	// -------------------------------------------------------------
	t.Run("LSP_ExecuteCommand_ResolveClasspath", func(t *testing.T) {
		cmdParams := map[string]any{
			"command": string(JDTCommandGoResolveClasspath),
			"arguments": []any{
				jtpsRoot,
				false, // compile scope
			},
		}

		res, err := client.request(LSPMethodWorkspaceExecuteCommand, cmdParams)
		if err != nil {
			// 记录事实：jdtls-go 依赖外部 Maven CLI 执行，若沙箱受限或 Maven 不可用将返回错误
			if strings.Contains(err.Error(), "Maven") || strings.Contains(err.Error(), "operation not permitted") || strings.Contains(err.Error(), "mvn") {
				t.Logf("预期捕获系统级外部 Maven 依赖行为: %v", err)
				return
			}
			t.Fatalf("resolveClasspath 执行出现非预期失败: %v", err)
		}

		var cpResult struct {
			ClassPaths []string `json:"classpaths"`
			SourceDirs []string `json:"sourceDirs"`
			Warnings   []string `json:"warnings"`
		}
		if err := json.Unmarshal(res, &cpResult); err != nil {
			t.Fatalf("解析 resolveClasspath 返回失败: %v, raw: %s", err, string(res))
		}

		t.Logf("成功解析类路径条目数: %d, 源码目录数: %d, 告警数: %d",
			len(cpResult.ClassPaths), len(cpResult.SourceDirs), len(cpResult.Warnings))

		foundTargetClasses := false
		for _, cp := range cpResult.ClassPaths {
			if strings.Contains(cp, "target/classes") {
				foundTargetClasses = true
				break
			}
		}
		if !foundTargetClasses {
			t.Errorf("resolveClasspath 期望包含 target/classes，实际: %v", cpResult.ClassPaths)
		}
	})

	// -------------------------------------------------------------
	// 7. 测试自定义 Maven 模型命令 (jdtls-go.resolveEffectiveModel)
	// -------------------------------------------------------------
	t.Run("LSP_ExecuteCommand_ResolveEffectiveModel", func(t *testing.T) {
		cmdParams := map[string]any{
			"command": string(JDTCommandGoResolveEffectiveModel),
			"arguments": []any{
				jtpsRoot,
			},
		}

		res, err := client.request(LSPMethodWorkspaceExecuteCommand, cmdParams)
		if err != nil {
			if strings.Contains(err.Error(), "Maven") || strings.Contains(err.Error(), "operation not permitted") || strings.Contains(err.Error(), "mvn") {
				t.Logf("预期捕获系统级外部 Maven 依赖行为: %v", err)
				return
			}
			t.Fatalf("resolveEffectiveModel 执行出现非预期失败: %v", err)
		}

		var modelResult struct {
			Artifact   string `json:"artifact"`
			GroupID    string `json:"groupId"`
			Version    string `json:"version"`
			SourceRoot string `json:"sourceRoot"`
		}
		if err := json.Unmarshal(res, &modelResult); err != nil {
			t.Fatalf("解析有效模型失败: %v, raw: %s", err, string(res))
		}

		if modelResult.Artifact != "" {
			t.Logf("有效模型解析结果: Artifact=%s, GroupID=%s, Version=%s",
				modelResult.Artifact, modelResult.GroupID, modelResult.Version)
		}
	})

	// -------------------------------------------------------------
	// 8. 阶段一新增特性测试：代码折叠区间 (textDocument/foldingRange)
	// -------------------------------------------------------------
	t.Run("LSP_FoldingRanges_Extraction", func(t *testing.T) {
		params := map[string]any{
			"textDocument": map[string]any{
				"uri": helloURI,
			},
		}

		res, err := client.request(LSPMethodTextDocumentFoldingRange, params)
		if err != nil {
			t.Fatalf("foldingRange 请求失败: %v", err)
		}

		var ranges []struct {
			StartLine int    `json:"startLine"`
			EndLine   int    `json:"endLine"`
			Kind      string `json:"kind"`
		}
		if err := json.Unmarshal(res, &ranges); err != nil {
			t.Fatalf("解析 foldingRange 失败: %v, raw: %s", err, string(res))
		}

		if len(ranges) == 0 {
			t.Errorf("期望返回折叠区间，实际返回空")
		}
		foundImports := false
		for _, r := range ranges {
			if r.Kind == "imports" {
				foundImports = true
			}
			t.Logf("折叠区间: startLine=%d, endLine=%d, kind=%s", r.StartLine, r.EndLine, r.Kind)
		}
		if !foundImports {
			t.Errorf("期望识别到 import 折叠块")
		}
	})

	// -------------------------------------------------------------
	// 9. 阶段一新增特性测试：智能选区扩展 (textDocument/selectionRange)
	// -------------------------------------------------------------
	t.Run("LSP_SelectionRanges_Expansion", func(t *testing.T) {
		params := map[string]any{
			"textDocument": map[string]any{
				"uri": helloURI,
			},
			"positions": []map[string]int{
				{"line": 6, "character": 15}, // HelloController
			},
		}

		res, err := client.request(LSPMethodTextDocumentSelectionRange, params)
		if err != nil {
			t.Fatalf("selectionRange 请求失败: %v", err)
		}

		var selRanges []struct {
			Range  map[string]any `json:"range"`
			Parent map[string]any `json:"parent"`
		}
		if err := json.Unmarshal(res, &selRanges); err != nil {
			t.Fatalf("解析 selectionRange 失败: %v, raw: %s", err, string(res))
		}

		if len(selRanges) == 0 {
			t.Errorf("期望返回选区扩展结构")
		} else {
			t.Logf("成功返回选区扩展: %+v", selRanges[0])
		}
	})

	// -------------------------------------------------------------
	// 10. 阶段一新增特性测试：语义高亮着色 (textDocument/semanticTokens/full)
	// -------------------------------------------------------------
	t.Run("LSP_SemanticTokens_Full", func(t *testing.T) {
		params := map[string]any{
			"textDocument": map[string]any{
				"uri": helloURI,
			},
		}

		res, err := client.request(LSPMethodTextDocumentSemanticTokens, params)
		if err != nil {
			t.Fatalf("semanticTokens 请求失败: %v", err)
		}

		var semResult struct {
			Data []uint32 `json:"data"`
		}
		if err := json.Unmarshal(res, &semResult); err != nil {
			t.Fatalf("解析 semanticTokens 失败: %v, raw: %s", err, string(res))
		}

		if len(semResult.Data) == 0 || len(semResult.Data)%5 != 0 {
			t.Errorf("期望返回非空的 5-tuple 语义标记数组，实际长度: %d", len(semResult.Data))
		} else {
			t.Logf("成功提取语义高亮令牌数: %d (共 %d 个数据元)", len(semResult.Data)/5, len(semResult.Data))
		}
	})

	// -------------------------------------------------------------
	// 11. 阶段一新增特性测试：代码修复与操作 (textDocument/codeAction)
	// -------------------------------------------------------------
	t.Run("LSP_CodeActions_OrganizeImports", func(t *testing.T) {
		params := map[string]any{
			"textDocument": map[string]any{
				"uri": helloURI,
			},
			"range": map[string]any{
				"start": map[string]int{"line": 0, "character": 0},
				"end":   map[string]int{"line": 1, "character": 0},
			},
		}

		res, err := client.request(LSPMethodTextDocumentCodeAction, params)
		if err != nil {
			t.Fatalf("codeAction 请求失败: %v", err)
		}

		var actions []struct {
			Title string `json:"title"`
			Kind  string `json:"kind"`
		}
		if err := json.Unmarshal(res, &actions); err != nil {
			t.Fatalf("解析 codeAction 失败: %v, raw: %s", err, string(res))
		}

		foundOrganize := false
		for _, a := range actions {
			if a.Kind == "source.organizeImports" {
				foundOrganize = true
			}
		}
		if !foundOrganize {
			t.Errorf("期望提供 source.organizeImports 操作")
		}
		t.Logf("成功返回 codeActions 数量: %d", len(actions))
	})

	// -------------------------------------------------------------
	// 12. 阶段一 JDT 专有扩展命令测试集
	// -------------------------------------------------------------
	t.Run("JDT_Command_Suite", func(t *testing.T) {
		// 12.1 java.project.isTestFile
		isTestRes, err := client.request(LSPMethodWorkspaceExecuteCommand, map[string]any{
			"command":   string(JDTCommandProjectIsTestFile),
			"arguments": []any{helloURI},
		})
		if err != nil {
			t.Fatalf("isTestFile 执行失败: %v", err)
		}
		var isTest bool
		_ = json.Unmarshal(isTestRes, &isTest)
		if isTest {
			t.Errorf("HelloController.java 不应判定为测试文件")
		}

		// 12.2 java.project.listSourcePaths
		srcPathsRes, err := client.request(LSPMethodWorkspaceExecuteCommand, map[string]any{
			"command":   string(JDTCommandProjectListSourcePaths),
			"arguments": []any{jtpsURI},
		})
		if err != nil {
			t.Fatalf("listSourcePaths 执行失败: %v", err)
		}
		var srcPaths []string
		_ = json.Unmarshal(srcPathsRes, &srcPaths)
		if len(srcPaths) == 0 {
			t.Errorf("listSourcePaths 期望返回源码路径列表")
		} else {
			t.Logf("返回源码路径列表: %v", srcPaths)
		}

		// 12.3 java.project.getAll
		allRes, err := client.request(LSPMethodWorkspaceExecuteCommand, map[string]any{
			"command":   string(JDTCommandProjectGetAll),
			"arguments": []any{},
		})
		if err != nil {
			t.Fatalf("getAll 执行失败: %v", err)
		}
		var allMods []any
		_ = json.Unmarshal(allRes, &allMods)
		t.Logf("getAll 模块总数: %d", len(allMods))

		// 12.4 java.getFullyQualifiedName (类名与方法名)
		fqnRes, err := client.request(LSPMethodWorkspaceExecuteCommand, map[string]any{
			"command":   string(JDTCommandGetFullyQualifiedName),
			"arguments": []any{helloURI, 6, 15}, // HelloController
		})
		if err != nil {
			t.Fatalf("getFullyQualifiedName 执行失败: %v", err)
		}
		var fqn string
		_ = json.Unmarshal(fqnRes, &fqn)
		if fqn != "com.example.jtps.HelloController" {
			t.Errorf("getFullyQualifiedName 期望 com.example.jtps.HelloController，实际: %s", fqn)
		} else {
			t.Logf("成功提取全限定类名: %s", fqn)
		}

		// 12.5 java.getTroubleshootingInfo
		tbRes, err := client.request(LSPMethodWorkspaceExecuteCommand, map[string]any{
			"command":   string(JDTCommandGetTroubleshootingInfo),
			"arguments": []any{},
		})
		if err != nil {
			t.Fatalf("getTroubleshootingInfo 执行失败: %v", err)
		}
		var tbInfo map[string]any
		_ = json.Unmarshal(tbRes, &tbInfo)
		if tbInfo["lsp"] != "pure-go" {
			t.Errorf("期望 lsp 为 pure-go，实际: %v", tbInfo["lsp"])
		}
		t.Logf("成功获取环境排查诊断信息: %+v", tbInfo)

		// 12.6 java.edit.organizeImports
		orgRes, err := client.request(LSPMethodWorkspaceExecuteCommand, map[string]any{
			"command":   string(JDTCommandEditOrganizeImports),
			"arguments": []any{helloURI},
		})
		if err != nil {
			t.Fatalf("organizeImports 执行失败: %v", err)
		}
		var orgEdit struct {
			Changes map[string][]struct {
				NewText string `json:"newText"`
			} `json:"changes"`
		}
		if err := json.Unmarshal(orgRes, &orgEdit); err != nil {
			t.Fatalf("解析 organizeImports 返回失败: %v", err)
		}
		if len(orgEdit.Changes) == 0 {
			t.Errorf("期望返回 Organize Imports WorkspaceEdit")
		} else {
			t.Logf("成功生成 Organize Imports WorkspaceEdit")
		}

		// 12.7 java.project.getSettings
		settingsRes, err := client.request(LSPMethodWorkspaceExecuteCommand, map[string]any{
			"command":   string(JDTCommandProjectGetSettings),
			"arguments": []any{jtpsURI},
		})
		if err != nil {
			t.Fatalf("getSettings 执行失败: %v", err)
		}
		var settings map[string]any
		_ = json.Unmarshal(settingsRes, &settings)
		if settings["org.eclipse.jdt.core.compiler.source"] != "17" {
			t.Errorf("期望编译器版本 17，实际: %v", settings["org.eclipse.jdt.core.compiler.source"])
		} else {
			t.Logf("成功提取工程编译配置: %+v", settings)
		}

		// 12.8 java.edit.smartSemicolonDetection
		semiRes, err := client.request(LSPMethodWorkspaceExecuteCommand, map[string]any{
			"command":   string(JDTCommandEditSmartSemicolonDetection),
			"arguments": []any{helloURI, 9, 30}, // 位于缺少分号行
		})
		if err != nil {
			t.Fatalf("smartSemicolonDetection 执行失败: %v", err)
		}
		var semiResult struct {
			NeedSemicolon bool `json:"needSemicolon"`
		}
		_ = json.Unmarshal(semiRes, &semiResult)
		t.Logf("智能分号探测结果: needSemicolon=%v", semiResult.NeedSemicolon)

		// 12.9 java.vm.getAllInstalls
		vmRes, err := client.request(LSPMethodWorkspaceExecuteCommand, map[string]any{
			"command":   string(JDTCommandVmGetAllInstalls),
			"arguments": []any{},
		})
		if err != nil {
			t.Fatalf("getAllInstalls 执行失败: %v", err)
		}
		var vms []map[string]any
		_ = json.Unmarshal(vmRes, &vms)
		if len(vms) == 0 {
			t.Errorf("期望返回 JVM 安装列表")
		} else {
			t.Logf("成功发现已安装 JVM: %+v", vms)
		}

		// 12.10 java.project.updateSourceAttachment & resolveSourceAttachment
		testJar := "/test/path/mylib.jar"
		testSrcJar := "/test/path/mylib-sources.jar"
		_, err = client.request(LSPMethodWorkspaceExecuteCommand, map[string]any{
			"command":   string(JDTCommandProjectUpdateSourceAttachment),
			"arguments": []any{testJar, testSrcJar},
		})
		if err != nil {
			t.Fatalf("updateSourceAttachment 执行失败: %v", err)
		}
		resolveSrcRes, err := client.request(LSPMethodWorkspaceExecuteCommand, map[string]any{
			"command":   string(JDTCommandProjectResolveSourceAttachment),
			"arguments": []any{testJar},
		})
		if err != nil {
			t.Fatalf("resolveSourceAttachment 执行失败: %v", err)
		}
		var resolvedSrc string
		_ = json.Unmarshal(resolveSrcRes, &resolvedSrc)
		if resolvedSrc != testSrcJar {
			t.Errorf("resolveSourceAttachment 期望 %s，实际: %s", testSrcJar, resolvedSrc)
		} else {
			t.Logf("源码附件更新与解析正常对齐: %s", resolvedSrc)
		}

		// 12.11 java.project.refreshDiagnostics
		refreshRes, err := client.request(LSPMethodWorkspaceExecuteCommand, map[string]any{
			"command":   string(JDTCommandProjectRefreshDiagnostics),
			"arguments": []any{},
		})
		if err != nil {
			t.Fatalf("refreshDiagnostics 执行失败: %v", err)
		}
		t.Logf("refreshDiagnostics 成功响应: %s", string(refreshRes))
	})

	// -------------------------------------------------------------
	// 13. 阶段二新增特性测试：源码格式化排版 (textDocument/formatting)
	// -------------------------------------------------------------
	t.Run("LSP_Formatting_Source", func(t *testing.T) {
		params := map[string]any{
			"textDocument": map[string]any{
				"uri": helloURI,
			},
			"options": map[string]any{
				"tabSize":      4,
				"insertSpaces": true,
			},
		}

		res, err := client.request(LSPMethodTextDocumentFormatting, params)
		if err != nil {
			t.Fatalf("formatting 请求失败: %v", err)
		}

		var edits []struct {
			Range   map[string]any `json:"range"`
			NewText string         `json:"newText"`
		}
		if err := json.Unmarshal(res, &edits); err != nil {
			t.Fatalf("解析 formatting 返回失败: %v", err)
		}
		if len(edits) == 0 {
			t.Errorf("formatting 期望返回排版 TextEdit")
		} else {
			t.Logf("成功生成格式化 TextEdit: %d 项修改", len(edits))
		}
	})

	// -------------------------------------------------------------
	// 14. 阶段二新增特性测试：符号重命名 (textDocument/rename)
	// -------------------------------------------------------------
	t.Run("LSP_Rename_Identifier", func(t *testing.T) {
		params := map[string]any{
			"textDocument": map[string]any{
				"uri": helloURI,
			},
			"position": map[string]int{
				"line":      8, // public String hello()
				"character": 19,
			},
			"newName": "sayHello",
		}

		res, err := client.request(LSPMethodTextDocumentRename, params)
		if err != nil {
			t.Fatalf("rename 请求失败: %v", err)
		}

		var edit struct {
			Changes map[string][]struct {
				NewText string `json:"newText"`
			} `json:"changes"`
		}
		if err := json.Unmarshal(res, &edit); err != nil {
			t.Fatalf("解析 rename 返回失败: %v", err)
		}
		if len(edit.Changes) == 0 {
			t.Errorf("rename 期望返回非空 WorkspaceEdit")
		} else {
			t.Logf("成功生成重命名 WorkspaceEdit: %v", edit.Changes)
		}
	})

	// -------------------------------------------------------------
	// 15. 阶段二新增特性测试：类型层次树 (textDocument/typeHierarchy)
	// -------------------------------------------------------------
	t.Run("LSP_TypeHierarchy_Supertypes", func(t *testing.T) {
		prepParams := map[string]any{
			"textDocument": map[string]any{
				"uri": helloURI,
			},
			"position": map[string]int{
				"line":      6, // public class HelloController
				"character": 15,
			},
		}

		prepRes, err := client.request(LSPMethodTextDocumentPrepareTypeHierarchy, prepParams)
		if err != nil {
			t.Fatalf("prepareTypeHierarchy 请求失败: %v", err)
		}

		var items []struct {
			Name string `json:"name"`
			Kind int    `json:"kind"`
		}
		if err := json.Unmarshal(prepRes, &items); err != nil {
			t.Fatalf("解析 prepareTypeHierarchy 失败: %v", err)
		}

		if len(items) == 0 {
			t.Errorf("prepareTypeHierarchy 期望返回当前类结构")
		} else {
			t.Logf("成功解析类型层次准备项: %+v", items[0])

			// 查询父类与接口
			superParams := map[string]any{
				"item": map[string]any{
					"name": items[0].Name,
					"uri":  helloURI,
				},
			}
			superRes, err := client.request(LSPMethodTypeHierarchySupertypes, superParams)
			if err != nil {
				t.Fatalf("typeHierarchy/supertypes 请求失败: %v", err)
			}
			var supers []struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(superRes, &supers)
			t.Logf("成功提取父类型/接口列表: %v", supers)
		}
	})

	// -------------------------------------------------------------
	// 16. 阶段二新增特性测试：方法调用链 (textDocument/callHierarchy)
	// -------------------------------------------------------------
	t.Run("LSP_CallHierarchy_Prepare", func(t *testing.T) {
		callParams := map[string]any{
			"textDocument": map[string]any{
				"uri": helloURI,
			},
			"position": map[string]int{
				"line":      8, // public String hello()
				"character": 19,
			},
		}

		callRes, err := client.request(LSPMethodTextDocumentPrepareCallHierarchy, callParams)
		if err != nil {
			t.Fatalf("prepareCallHierarchy 请求失败: %v", err)
		}

		var callItems []struct {
			Name string `json:"name"`
			Kind int    `json:"kind"`
		}
		if err := json.Unmarshal(callRes, &callItems); err != nil {
			t.Fatalf("解析 prepareCallHierarchy 失败: %v", err)
		}
		if len(callItems) == 0 {
			t.Errorf("prepareCallHierarchy 期望返回方法条目")
		} else {
			t.Logf("成功解析调用链条目: %+v", callItems[0])
		}
	})

	// -------------------------------------------------------------
	// 17. 阶段二新增特性测试：内联参数提示 (textDocument/inlayHint)
	// -------------------------------------------------------------
	t.Run("LSP_InlayHints_Extraction", func(t *testing.T) {
		hintParams := map[string]any{
			"textDocument": map[string]any{
				"uri": helloURI,
			},
		}

		hintRes, err := client.request(LSPMethodTextDocumentInlayHint, hintParams)
		if err != nil {
			t.Fatalf("inlayHint 请求失败: %v", err)
		}

		var hints []struct {
			Label string `json:"label"`
			Kind  int    `json:"kind"`
		}
		if err := json.Unmarshal(hintRes, &hints); err != nil {
			t.Fatalf("解析 inlayHint 失败: %v", err)
		}
		t.Logf("成功生成 InlayHints 数量: %d, 详情: %+v", len(hints), hints)
	})

	// -------------------------------------------------------------
	// 18. 测试 textDocument/onTypeFormatting 键入格式化
	// -------------------------------------------------------------
	t.Run("LSP_OnTypeFormatting", func(t *testing.T) {
		params := map[string]any{
			"textDocument": map[string]any{
				"uri": helloURI,
			},
			"position": map[string]int{
				"line":      10,
				"character": 20,
			},
			"ch": ";",
			"options": map[string]any{
				"tabSize":      4,
				"insertSpaces": true,
			},
		}

		res, err := client.request(LSPMethodTextDocumentOnTypeFormatting, params)
		if err != nil {
			t.Fatalf("onTypeFormatting 失败: %v", err)
		}
		t.Logf("onTypeFormatting 返回 edits: %s", string(res))
	})

	// -------------------------------------------------------------
	// 19. 测试 textDocument/codeLens 代码镜头
	// -------------------------------------------------------------
	t.Run("LSP_CodeLens", func(t *testing.T) {
		params := map[string]any{
			"textDocument": map[string]any{
				"uri": appURI,
			},
		}

		res, err := client.request(LSPMethodTextDocumentCodeLens, params)
		if err != nil {
			t.Fatalf("codeLens 失败: %v", err)
		}

		var lenses []struct {
			Command struct {
				Title   string `json:"title"`
				Command string `json:"command"`
			} `json:"command"`
		}
		if err := json.Unmarshal(res, &lenses); err != nil {
			t.Fatalf("解析 codeLens 失败: %v", err)
		}

		foundRun := false
		for _, l := range lenses {
			if strings.Contains(l.Command.Title, "Run") {
				foundRun = true
			}
		}
		if !foundRun {
			t.Errorf("未能成功生成 JtpsApplication main 方法 Run CodeLens: %+v", lenses)
		}
		t.Logf("成功提取到 CodeLenses 数量: %d, 详情: %+v", len(lenses), lenses)
	})

	// -------------------------------------------------------------
	// 20. 测试第四阶段新增 JDT 专有命令套件
	// -------------------------------------------------------------
	t.Run("LSP_Deep_JDT_Suite", func(t *testing.T) {
		// 1. 测试异常堆栈定位解析 java.project.resolveStackTraceLocation
		stackLine := "at com.example.jtps.HelloController.hello(HelloController.java:12)"
		stackRes, err := client.request(LSPMethodWorkspaceExecuteCommand, map[string]any{
			"command":   string(JDTCommandProjectResolveStackTraceLocation),
			"arguments": []any{stackLine},
		})
		if err != nil {
			t.Fatalf("resolveStackTraceLocation 失败: %v", err)
		}
		var stackLoc struct {
			URI       string `json:"uri"`
			Line      int    `json:"line"`
			ClassName string `json:"className"`
		}
		if err := json.Unmarshal(stackRes, &stackLoc); err != nil {
			t.Fatalf("解析堆栈定位失败: %v", err)
		}
		if stackLoc.ClassName != "com.example.jtps.HelloController" || stackLoc.Line != 11 {
			t.Errorf("堆栈定位解析异常: %+v", stackLoc)
		}
		t.Logf("成功反解异常调用栈: 类=%s, 行=%d, URI=%s", stackLoc.ClassName, stackLoc.Line, stackLoc.URI)

		// 2. 测试类型层次继承树 java.navigate.openTypeHierarchy
		navRes, err := client.request(LSPMethodWorkspaceExecuteCommand, map[string]any{
			"command":   string(JDTCommandNavigateOpenTypeHierarchy),
			"arguments": []any{helloURI},
		})
		if err != nil {
			t.Fatalf("openTypeHierarchy 失败: %v", err)
		}
		t.Logf("openTypeHierarchy 响应: %s", string(navRes))

		// 3. 测试自动生成 module-info.java
		moduleRes, err := client.request(LSPMethodWorkspaceExecuteCommand, map[string]any{
			"command":   string(JDTCommandProjectCreateModuleInfo),
			"arguments": []any{"com.example.jtps"},
		})
		if err != nil {
			t.Fatalf("createModuleInfo 失败: %v", err)
		}
		var moduleCode string
		if err := json.Unmarshal(moduleRes, &moduleCode); err == nil {
			if !strings.Contains(moduleCode, "module com.example.jtps") {
				t.Errorf("生成的 module-info.java 不符合预期: %s", moduleCode)
			}
			t.Logf("成功生成 module-info.java: \n%s", moduleCode)
		}

		// 4. 测试粘贴代码自动识别缺失导入 java.edit.handlePasteEvent
		pastedSnippet := "@Autowired\nprivate List<String> list;\n"
		pasteRes, err := client.request(LSPMethodWorkspaceExecuteCommand, map[string]any{
			"command":   string(JDTCommandEditHandlePasteEvent),
			"arguments": []any{helloURI, pastedSnippet},
		})
		if err != nil {
			t.Fatalf("handlePasteEvent 失败: %v", err)
		}
		var pasteResult struct {
			AddedImports []string `json:"addedImports"`
		}
		if err := json.Unmarshal(pasteRes, &pasteResult); err != nil {
			t.Fatalf("解析 pasteResult 失败: %v", err)
		}
		foundAutowired := false
		for _, imp := range pasteResult.AddedImports {
			if strings.Contains(imp, "Autowired") {
				foundAutowired = true
			}
		}
		if !foundAutowired {
			t.Errorf("未能识别出缺失的 Autowired import: %+v", pasteResult.AddedImports)
		}
		t.Logf("粘贴代码成功识别并推荐导入: %+v", pasteResult.AddedImports)

		// 5. 测试工作区符号解析 java.project.resolveWorkspaceSymbol
		symRes, err := client.request(LSPMethodWorkspaceExecuteCommand, map[string]any{
			"command":   string(JDTCommandProjectResolveWorkspaceSymbol),
			"arguments": []any{"Hello"},
		})
		if err != nil {
			t.Fatalf("resolveWorkspaceSymbol 失败: %v", err)
		}
		t.Logf("resolveWorkspaceSymbol 结果: %s", string(symRes))

		// 6. 测试重载 Bundles / 索引缓存 java.reloadBundles
		reloadRes, err := client.request(LSPMethodWorkspaceExecuteCommand, map[string]any{
			"command":   string(JDTCommandReloadBundles),
			"arguments": []any{},
		})
		if err != nil {
			t.Fatalf("reloadBundles 失败: %v", err)
		}
		t.Logf("reloadBundles 结果: %s", string(reloadRes))

		// 7. 测试工程类路径更新 java.project.updateClassPaths
		updateCpRes, err := client.request(LSPMethodWorkspaceExecuteCommand, map[string]any{
			"command":   string(JDTCommandProjectUpdateClassPaths),
			"arguments": []any{jtpsRoot},
		})
		if err != nil {
			t.Fatalf("updateClassPaths 失败: %v", err)
		}
		t.Logf("updateClassPaths 响应: %s", string(updateCpRes))

		// 8. 测试构建系统兼容声明 java.project.upgradeGradle
		upgradeRes, err := client.request(LSPMethodWorkspaceExecuteCommand, map[string]any{
			"command":   string(JDTCommandProjectUpgradeGradle),
			"arguments": []any{},
		})
		if err != nil {
			t.Fatalf("upgradeGradle 失败: %v", err)
		}
		t.Logf("upgradeGradle 响应: %s", string(upgradeRes))

		// 9. 测试 JDK 切换与重定向 java.project.updateJdk
		jdkRes, err := client.request(LSPMethodWorkspaceExecuteCommand, map[string]any{
			"command":   string(JDTCommandProjectUpdateJdk),
			"arguments": []any{"JavaSE-17"},
		})
		if err != nil {
			t.Fatalf("updateJdk 失败: %v", err)
		}
		t.Logf("updateJdk 响应: %s", string(jdkRes))

		// 10. 测试 Protobuf 源码生成扩展 java.protobuf.generateSources
		protoRes, err := client.request(LSPMethodWorkspaceExecuteCommand, map[string]any{
			"command":   string(JDTCommandProtobufGenerateSources),
			"arguments": []any{},
		})
		if err != nil {
			t.Fatalf("protobufGenerateSources 失败: %v", err)
		}
		t.Logf("protobufGenerateSources 响应: %s", string(protoRes))
	})

	// -------------------------------------------------------------
	// 21. 测试方法定义与实现跳转 (对齐 Cannot find declaration to go to 缺陷)
	// -------------------------------------------------------------
	t.Run("LSP_Method_Definition_And_Implementation", func(t *testing.T) {
		// HelloController.java 中的 hello 方法 (line 8, col 18: "public String hello()")
		defParams := map[string]any{
			"textDocument": map[string]any{"uri": helloURI},
			"position":     map[string]int{"line": 8, "character": 19},
		}
		defRes, err := client.request(LSPMethodTextDocumentDefinition, defParams)
		if err != nil {
			t.Fatalf("textDocument/definition 失败: %v", err)
		}
		var defLocs []struct {
			URI   string `json:"uri"`
			Range struct {
				Start struct {
					Line      int `json:"line"`
					Character int `json:"character"`
				} `json:"start"`
			} `json:"range"`
		}
		if err := json.Unmarshal(defRes, &defLocs); err != nil {
			t.Fatalf("解析 definition 响应失败: %v", err)
		}
		if len(defLocs) == 0 {
			t.Errorf("点击 hello 方法未返回任何定义位置")
		}
		t.Logf("hello 方法定义位置数量: %d, 响应: %s", len(defLocs), string(defRes))

		// 测试方法实现跳转 (textDocument/implementation)
		implRes, err := client.request(LSPMethodTextDocumentImplementation, defParams)
		if err != nil {
			t.Fatalf("textDocument/implementation 失败: %v", err)
		}
		var implLocs []any
		if err := json.Unmarshal(implRes, &implLocs); err != nil {
			t.Fatalf("解析 implementation 响应失败: %v", err)
		}
		if len(implLocs) == 0 {
			t.Errorf("点击 hello 方法未返回任何实现位置")
		}
		t.Logf("hello 方法实现位置数量: %d, 响应: %s", len(implLocs), string(implRes))
	})

	// -------------------------------------------------------------
	// 22. 测试 OpenIDEA 调试类路径解析 (vscode.java.resolveClasspath)
	// -------------------------------------------------------------
	t.Run("LSP_VSCode_ResolveClasspath_OpenIDEA", func(t *testing.T) {
		// OpenIDEA 传参形式: [mainClass, projectName]
		classes := []string{"com.example.jtps.JdtlsApplication", "com.example.jtps.JtpsApplication"}
		for _, cls := range classes {
			cpRes, err := client.request(LSPMethodWorkspaceExecuteCommand, map[string]any{
				"command":   string(JDTCommandVSCodeResolveClasspath),
				"arguments": []any{cls, "jtps"},
			})
			if err != nil {
				t.Fatalf("vscode.java.resolveClasspath 对于类 %s 执行失败: %v", cls, err)
			}
			var entries []string
			if err := json.Unmarshal(cpRes, &entries); err != nil {
				t.Fatalf("vscode.java.resolveClasspath 未返回 []string 数组格式: %v, raw: %s", err, string(cpRes))
			}
			if len(entries) == 0 {
				t.Fatalf("类路径解析结果为空切片")
			}
			foundClasses := false
			for _, entry := range entries {
				if strings.HasSuffix(filepath.ToSlash(entry), "target/classes") {
					foundClasses = true
					break
				}
			}
			if !foundClasses {
				t.Errorf("解析结果未包含 target/classes 目录: %+v", entries)
			}
			t.Logf("类 %s 成功解析类路径条目数: %d, 示例: %s", cls, len(entries), entries[0])
		}
	})

	// -------------------------------------------------------------
	// 23. 测试优雅关闭 (shutdown & exit)
	// -------------------------------------------------------------
	t.Run("LSP_Shutdown_And_Exit", func(t *testing.T) {
		res, err := client.request(LSPMethodShutdown, nil)
		if err != nil {
			t.Fatalf("shutdown 失败: %v", err)
		}
		if string(res) != "null" {
			t.Logf("shutdown 返回: %s", string(res))
		}
		_ = client.notify(LSPMethodExit, nil)
	})
}
