package tests

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cage/jdtls-go/internal/dap"
)

// dapClient 是用于 DAP 协议通信的轻量级测试客户端
type dapClient struct {
	toServer   *io.PipeWriter
	fromServer *bufio.Reader
	mu         sync.Mutex
	nextSeq    int
	pending    map[int]chan json.RawMessage
	events     chan json.RawMessage
	done       chan struct{}
}

func newDAPTestClient(t *testing.T) (*dapClient, func()) {
	clientRead, serverWrite := io.Pipe()
	serverRead, clientWrite := io.Pipe()

	logger := log.New(io.Discard, "[test-dap] ", 0)

	go func() {
		_ = dap.Serve(serverRead, serverWrite, logger)
	}()

	client := &dapClient{
		toServer:   clientWrite,
		fromServer: bufio.NewReader(clientRead),
		pending:    make(map[int]chan json.RawMessage),
		events:     make(chan json.RawMessage, 50),
		done:       make(chan struct{}),
	}

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

func (c *dapClient) readLoop(t *testing.T) {
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
			Type       string `json:"type"`
			RequestSeq int    `json:"request_seq"`
			Event      string `json:"event"`
		}

		if err := json.Unmarshal(body, &generic); err != nil {
			continue
		}

		if generic.Type == "response" {
			c.mu.Lock()
			ch, ok := c.pending[generic.RequestSeq]
			delete(c.pending, generic.RequestSeq)
			c.mu.Unlock()

			if ok {
				ch <- body
			}
		} else if generic.Type == "event" {
			select {
			case c.events <- body:
			default:
			}
		}
	}
}

func (c *dapClient) request(command DAPCommand, arguments any) (json.RawMessage, error) {
	c.mu.Lock()
	c.nextSeq++
	seq := c.nextSeq
	ch := make(chan json.RawMessage, 1)
	c.pending[seq] = ch
	c.mu.Unlock()

	reqMap := map[string]any{
		"seq":       seq,
		"type":      "request",
		"command":   string(command),
		"arguments": arguments,
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
			Success bool            `json:"success"`
			Message string          `json:"message"`
			Body    json.RawMessage `json:"body"`
		}
		if err := json.Unmarshal(res, &parsed); err != nil {
			return nil, err
		}
		if !parsed.Success {
			var errBody struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(parsed.Body, &errBody)
			errMsg := parsed.Message
			if errMsg == "" && errBody.Error != "" {
				errMsg = errBody.Error
			}
			return nil, fmt.Errorf("dap error: %s", errMsg)
		}
		return parsed.Body, nil
	case <-time.After(5 * time.Second):
		return nil, fmt.Errorf("timeout waiting for DAP response to %s (seq=%d)", command, seq)
	}
}

// TestJtpsDAPReconstructionSuite 对 DAP 服务端执行端到端契约与行为对齐测试
func TestJtpsDAPReconstructionSuite(t *testing.T) {
	jtpsRoot := findJtpsRoot(t)
	helloControllerPath := filepath.Join(jtpsRoot, "src/main/java/com/example/jtps/HelloController.java")

	client, cleanup := newDAPTestClient(t)
	defer cleanup()

	// -------------------------------------------------------------
	// 1. DAP Initialize 握手与能力协商
	// -------------------------------------------------------------
	t.Run("DAP_Initialize_Capabilities", func(t *testing.T) {
		args := map[string]any{
			"clientID":   "openidea-test",
			"adapterID":  "java-debug-go",
			"pathFormat": "path",
		}

		body, err := client.request(DAPCommandInitialize, args)
		if err != nil {
			t.Fatalf("initialize 失败: %v", err)
		}

		var caps struct {
			SupportsConfigurationDoneRequest bool `json:"supportsConfigurationDoneRequest"`
			SupportsFunctionBreakpoints      bool `json:"supportsFunctionBreakpoints"`
			SupportsConditionalBreakpoints   bool `json:"supportsConditionalBreakpoints"`
			SupportsHitConditionalBreakpoints bool `json:"supportsHitConditionalBreakpoints"`
			SupportsLogPoints                bool `json:"supportsLogPoints"`
			SupportsExceptionInfoRequest     bool `json:"supportsExceptionInfoRequest"`
			SupportsDataBreakpoints          bool `json:"supportsDataBreakpoints"`
			SupportsEvaluateForHovers        bool `json:"supportsEvaluateForHovers"`
		}

		if err := json.Unmarshal(body, &caps); err != nil {
			t.Fatalf("解析 DAP capabilities 失败: %v", err)
		}

		if !caps.SupportsConfigurationDoneRequest {
			t.Errorf("期望支持 ConfigurationDoneRequest")
		}
		if !caps.SupportsFunctionBreakpoints {
			t.Errorf("期望支持 FunctionBreakpoints")
		}
		if !caps.SupportsConditionalBreakpoints {
			t.Errorf("期望支持 ConditionalBreakpoints")
		}
		if !caps.SupportsHitConditionalBreakpoints {
			t.Errorf("期望支持 HitConditionalBreakpoints")
		}
		if !caps.SupportsLogPoints {
			t.Errorf("期望支持 LogPoints")
		}

		t.Logf("DAP 服务端声明的能力对齐验证通过: %+v", caps)
	})

	// -------------------------------------------------------------
	// 2. DAP setBreakpoints 源码行断点测试 (针对 HelloController.java)
	// -------------------------------------------------------------
	t.Run("DAP_SetBreakpoints_HelloController", func(t *testing.T) {
		args := map[string]any{
			"source": map[string]any{
				"path": helloControllerPath,
			},
			"breakpoints": []map[string]any{
				{
					"line": 10, // System.out.printf("ss");
				},
				{
					"line":         14, // return "hello from jtps";
					"condition":    "1 == 1",
					"hitCondition": "3",
					"logMessage":   "hello reached",
				},
			},
		}

		_, err := client.request(DAPCommandSetBreakpoints, args)
		// 校验行为事实：jdtls-go 的 DAP 服务端要求必须先存在活动的 JVM 连接 (s.vm != nil)
		// 未连接时严格返回 "no JVM is attached"
		if err == nil {
			t.Errorf("期望未附加 JVM 时拒绝 setBreakpoints 请求，但请求成功了")
		} else if !strings.Contains(err.Error(), "no JVM is attached") {
			t.Errorf("期望返回 'no JVM is attached'，实际错误: %v", err)
		} else {
			t.Logf("成功验证 DAP 状态保护机制 (预期拦截: %v)", err)
		}
	})

	// -------------------------------------------------------------
	// 3. DAP setFunctionBreakpoints 函数断点测试
	// -------------------------------------------------------------
	t.Run("DAP_SetFunctionBreakpoints", func(t *testing.T) {
		args := map[string]any{
			"breakpoints": []map[string]any{
				{
					"name": "com.example.jtps.HelloController.hello",
				},
			},
		}

		_, err := client.request(DAPCommandSetFunctionBreakpoints, args)
		if err == nil {
			t.Errorf("期望未附加 JVM 时拒绝 setFunctionBreakpoints 请求，但请求成功了")
		} else if !strings.Contains(err.Error(), "no JVM is attached") {
			t.Errorf("期望返回 'no JVM is attached'，实际错误: %v", err)
		} else {
			t.Logf("成功验证函数断点状态保护机制 (预期拦截: %v)", err)
		}
	})

	// -------------------------------------------------------------
	// 4. DAP setExceptionBreakpoints 异常断点测试
	// -------------------------------------------------------------
	t.Run("DAP_SetExceptionBreakpoints", func(t *testing.T) {
		args := map[string]any{
			"filters": []string{"uncaught"},
		}

		_, err := client.request(DAPCommandSetExceptionBreakpoints, args)
		if err == nil {
			t.Errorf("期望未附加 JVM 时拒绝 setExceptionBreakpoints 请求，但请求成功了")
		} else if !strings.Contains(err.Error(), "no JVM is attached") {
			t.Errorf("期望返回 'no JVM is attached'，实际错误: %v", err)
		} else {
			t.Logf("成功验证异常断点状态保护机制 (预期拦截: %v)", err)
		}
	})

	// -------------------------------------------------------------
	// 5. 阶段三新增特性测试：DAP 高阶调度与热更命令集
	// -------------------------------------------------------------
	t.Run("DAP_Advanced_Debug_Suite", func(t *testing.T) {
		// 5.1 restartFrame 状态保护与参数契约
		_, err := client.request(DAPCommandRestartFrame, map[string]any{"frameId": 1})
		if err == nil {
			t.Errorf("期望未附加 JVM 时拦截 restartFrame，但成功了")
		} else {
			t.Logf("成功验证 restartFrame 保护逻辑: %v", err)
		}

		// 5.2 pauseOthers 与 continueOthers
		_, err = client.request(DAPCommandPauseOthers, map[string]any{"threadId": 1})
		if err == nil {
			t.Errorf("期望未附加 JVM 时拦截 pauseOthers")
		} else {
			t.Logf("成功验证 pauseOthers 保护逻辑: %v", err)
		}

		_, err = client.request(DAPCommandContinueOthers, map[string]any{"threadId": 1})
		if err == nil {
			t.Errorf("期望未附加 JVM 时拦截 continueOthers")
		} else {
			t.Logf("成功验证 continueOthers 保护逻辑: %v", err)
		}

		// 5.3 redefineClasses (HCR)
		_, err = client.request(DAPCommandRedefineClasses, map[string]any{
			"classes": []map[string]any{
				{"name": "com.example.jtps.HelloController", "bytes": ""},
			},
		})
		if err == nil {
			t.Errorf("期望未附加 JVM 时拦截 redefineClasses")
		} else {
			t.Logf("成功验证 redefineClasses 保护逻辑: %v", err)
		}

		// 5.4 runInTerminal
		termRes, err := client.request(DAPCommandRunInTerminal, map[string]any{
			"kind":  "integrated",
			"title": "JTps Test",
			"args":  []string{"java", "-version"},
		})
		if err != nil {
			t.Fatalf("runInTerminal 执行失败: %v", err)
		}
		t.Logf("runInTerminal 成功协商返回: %s", string(termRes))

		// 5.5 inlineValues
		ivRes, err := client.request(DAPCommandInlineValues, map[string]any{
			"textDocument": map[string]any{"uri": "file:///test/HelloController.java"},
			"range": map[string]any{
				"start": map[string]int{"line": 10, "character": 0},
				"end":   map[string]int{"line": 10, "character": 10},
			},
		})
		if err != nil {
			t.Fatalf("inlineValues 执行失败: %v", err)
		}
		t.Logf("inlineValues 成功应答: %s", string(ivRes))

		// 5.6 stepInTargets
		stepRes, err := client.request(DAPCommandStepInTargets, map[string]any{
			"frameId": 1,
		})
		if err != nil {
			t.Fatalf("stepInTargets 执行失败: %v", err)
		}
		t.Logf("stepInTargets 成功应答: %s", string(stepRes))
	})

	// -------------------------------------------------------------
	// 6. DAP configurationDone 通知
	// -------------------------------------------------------------
	t.Run("DAP_ConfigurationDone", func(t *testing.T) {
		_, err := client.request(DAPCommandConfigurationDone, map[string]any{})
		if err != nil {
			t.Fatalf("configurationDone 失败: %v", err)
		}
		t.Log("configurationDone 执行成功")
	})

	// -------------------------------------------------------------
	// 7. DAP disconnect 优雅断开
	// -------------------------------------------------------------
	t.Run("DAP_Disconnect", func(t *testing.T) {
		args := map[string]any{
			"terminateDebuggee": false,
		}
		_, err := client.request(DAPCommandDisconnect, args)
		if err != nil {
			t.Fatalf("disconnect 失败: %v", err)
		}
		t.Log("disconnect 执行成功")
	})
}
