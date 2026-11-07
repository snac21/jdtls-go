package tests

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cage/jdtls-go/internal/dap"
	"github.com/cage/jdtls-go/internal/jdwp"
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

func (c *dapClient) requestRaw(command DAPCommand, arguments any) ([]byte, error) {
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
		return res, nil
	case <-time.After(5 * time.Second):
		return nil, fmt.Errorf("timeout waiting for raw DAP response to %s (seq=%d)", command, seq)
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
	// 7. 测试 DAP 错误契约规范 (解决 launch failed: request failed)
	// -------------------------------------------------------------
	t.Run("DAP_Error_Format_And_TopLevel_Message", func(t *testing.T) {
		// 故意发送无 mainClass 的 launch 请求，触发校验错误
		badLaunchArgs := map[string]any{
			"name":    "TestError",
			"request": "launch",
		}
		rawResp, err := client.requestRaw(DAPCommandLaunch, badLaunchArgs)
		if err != nil {
			t.Fatalf("发送 launch 请求失败: %v", err)
		}

		var resp struct {
			Success bool   `json:"success"`
			Command string `json:"command"`
			Message string `json:"message"`
			Body    struct {
				Error struct {
					Format string `json:"format"`
				} `json:"error"`
			} `json:"body"`
		}
		if err := json.Unmarshal(rawResp, &resp); err != nil {
			t.Fatalf("解析 DAP 响应 JSON 失败: %v, raw: %s", err, string(rawResp))
		}

		if resp.Success {
			t.Errorf("缺少 mainClass 时 launch 不应成功")
		}
		if resp.Message == "" || resp.Message == "request failed" {
			t.Errorf("DAP 顶级 message 字段未正确填充，导致客户端 fallback 为 request failed: %q", resp.Message)
		}
		if resp.Body.Error.Format == "" {
			t.Errorf("DAP body.error.format 字段为空，未能对齐 DAP 结构化错误契约")
		}
		t.Logf("成功校验 DAP 结构化错误响应: command=%s, success=%v, message=%s, body.error.format=%s",
			resp.Command, resp.Success, resp.Message, resp.Body.Error.Format)
	})

	// -------------------------------------------------------------
	// 8. DAP disconnect 优雅断开
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

// TestJDWPVMStartDecoding 专项测试 JDWP 握手初期并发到达 EventVMStart (kind 90) 的解码稳定性
func TestJDWPVMStartDecoding(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	// 启动模拟 JVM JDWP Server
	go func() {
		// 1. 完成 JDWP 握手
		handshake := make([]byte, 14)
		if _, err := io.ReadFull(serverConn, handshake); err != nil {
			return
		}
		if _, err := serverConn.Write(handshake); err != nil {
			return
		}

		// 2. 模拟 JVM 立即主动推送 EventVMStart (kind 90) 事件包 (Set 64, Command 100)
		// Composite Event 包结构:
		// Header (11 bytes): Length(4) + ID(4) + Flags(1:0) + CommandSet(1:64) + Command(1:100)
		// Body: SuspendPolicy(1:2) + EventCount(4:1) + EventKind(1:90) + RequestID(4:0) + ThreadID(8:1001)
		// Body 长度 = 1 + 4 + 1 + 4 + 8 = 18 字节, 包总长 = 11 + 18 = 29 字节
		vmStartPkt := make([]byte, 29)
		binary.BigEndian.PutUint32(vmStartPkt[0:4], 29)
		binary.BigEndian.PutUint32(vmStartPkt[4:8], 9999) // event packet id
		vmStartPkt[8] = 0                                // command packet
		vmStartPkt[9] = 64                               // Event command set
		vmStartPkt[10] = 100                             // Composite command
		// Body
		vmStartPkt[11] = 2 // SuspendAll
		binary.BigEndian.PutUint32(vmStartPkt[12:16], 1)
		vmStartPkt[16] = 90 // EventVMStart
		binary.BigEndian.PutUint32(vmStartPkt[17:21], 0)
		binary.BigEndian.PutUint64(vmStartPkt[21:29], 1001) // ThreadID

		_, _ = serverConn.Write(vmStartPkt)

		// 3. 读取客户端发来的 VirtualMachine.IDSizes 请求包 (Set 1, Command 7)
		reqHeader := make([]byte, 11)
		if _, err := io.ReadFull(serverConn, reqHeader); err != nil {
			return
		}
		reqID := binary.BigEndian.Uint32(reqHeader[4:8])

		// 回复 IDSizes 响应包 (5 个 4 字节的 ID 大小声明，共 20 字节)
		// fieldID(4) + methodID(4) + objectID(4) + classID(4) + frameID(4) = 8, 8, 8, 8, 8
		respPkt := make([]byte, 11+20)
		binary.BigEndian.PutUint32(respPkt[0:4], 31)
		binary.BigEndian.PutUint32(respPkt[4:8], reqID)
		respPkt[8] = 0x80 // reply packet
		binary.BigEndian.PutUint16(respPkt[9:11], 0) // error code 0
		binary.BigEndian.PutUint32(respPkt[11:15], 8) // fieldID
		binary.BigEndian.PutUint32(respPkt[15:19], 8) // methodID
		binary.BigEndian.PutUint32(respPkt[19:23], 8) // objectID
		binary.BigEndian.PutUint32(respPkt[23:27], 8) // classID
		binary.BigEndian.PutUint32(respPkt[27:31], 8) // frameID

		_, _ = serverConn.Write(respPkt)
	}()

	// 客户端调用 jdwp.New
	vm, err := jdwp.New(clientConn)
	if err != nil {
		t.Fatalf("jdwp.New 失败，出现握手/事件解码回归: %v", err)
	}
	defer vm.Close()

	// 验证成功收到并解码 EventVMStart
	select {
	case event := <-vm.Events():
		if event.Kind != 90 {
			t.Errorf("期望收到 EventVMStart (kind 90)，实际为: %d", event.Kind)
		}
		if event.ThreadID != 1001 {
			t.Errorf("期望 ThreadID 为 1001，实际为: %d", event.ThreadID)
		}
		t.Logf("成功解码 EventVMStart: Kind=%d, ThreadID=%d", event.Kind, event.ThreadID)
	case <-time.After(3 * time.Second):
		t.Fatalf("超时未收到 EventVMStart 事件")
	}
}
