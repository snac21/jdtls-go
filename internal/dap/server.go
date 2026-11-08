package dap

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/constant"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cage/jdtls-go/internal/fileuri"
	"github.com/cage/jdtls-go/internal/jdwp"
	"github.com/cage/jdtls-go/internal/maven"
)

type request struct {
	Seq       int             `json:"seq"`
	Type      string          `json:"type"`
	Command   string          `json:"command"`
	Arguments json.RawMessage `json:"arguments"`
}

type server struct {
	in                *bufio.Reader
	out               *bufio.Writer
	logger            *log.Logger
	seq               int
	mu                sync.Mutex
	pendingStart      *request
	vm                *jdwp.Connection
	process           *exec.Cmd
	launched          bool
	bpMu              sync.Mutex
	bps               map[string][]*sourceBreakpoint
	byJDWP            map[uint32]*sourceBreakpoint
	functionBPs       []*sourceBreakpoint
	dataWatchpoints   map[uint32]int
	nextBP            int
	prepareRequest    uint32
	exceptionRequest  uint32
	stepRequest       uint32
	sourceRoots       []string
	sourceAttachments map[string]string
	frameRefs         map[int]frameRef
	nextFrameID       int
	handles           map[int]variableHandle
	nextHandle        int
	runMu             sync.Mutex
	stopped           bool
	lastException     jdwp.Event
}

type sourceBreakpoint struct {
	ID           int
	Source       string
	Function     string
	Line         int
	ActualLine   int
	Condition    string
	LogMessage   string
	Verified     bool
	Locations    []uint32
	Resolved     map[uint64]bool
	HitCondition string
	HitCount     int
}

type classInfo struct {
	ID        uint64
	Signature string
	Source    string
}

type frameRef struct {
	ThreadID  uint64
	FrameID   uint64
	ClassID   uint64
	MethodID  uint64
	CodeIndex int64
}

func Serve(input io.Reader, output io.Writer, logger *log.Logger) error {
	s := &server{in: bufio.NewReader(input), out: bufio.NewWriter(output), logger: logger, bps: map[string][]*sourceBreakpoint{}, byJDWP: map[uint32]*sourceBreakpoint{}, dataWatchpoints: map[uint32]int{}, frameRefs: map[int]frameRef{}, handles: map[int]variableHandle{}, sourceAttachments: map[string]string{}}
	defer s.stop()
	for {
		body, err := readFrame(s.in)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		var req request
		if err := json.Unmarshal(body, &req); err != nil {
			return err
		}
		if req.Type != "request" {
			continue
		}
		if err := s.dispatch(req); err != nil {
			logger.Printf("%s: %v", req.Command, err)
			if writeErr := s.response(req, false, map[string]any{"error": err.Error()}); writeErr != nil {
				return writeErr
			}
		}
	}
}

func (s *server) dispatch(req request) error {
	switch req.Command {
	case "initialize":
		return s.response(req, true, map[string]any{
			"supportsConfigurationDoneRequest":   true,
			"supportsTerminateRequest":           true,
			"supportsPauseRequest":               true,
			"supportsSetVariable":                true,
			"supportsEvaluateForHovers":          true,
			"supportsConditionalBreakpoints":     true,
			"supportsHitConditionalBreakpoints":  true,
			"supportsLogPoints":                  true,
			"supportsFunctionBreakpoints":        true,
			"supportsDataBreakpoints":            true,
			"supportsExceptionInfoRequest":       true,
			"supportsCompletionsRequest":         true,
			"supportsStepInTargetsRequest":       true,
			"supportsLoadedSourcesRequest":       true,
			"supportsBreakpointLocationsRequest": true,
			"supportsRestartFrame":               true,
			"supportsInlineValues":               true,
			"supportsRedefineClasses":            true,
		})
	case "launch":
		if s.pendingStart != nil {
			return errors.New("a launch or attach request is already awaiting configurationDone")
		}
		if err := s.launch(req.Arguments); err != nil {
			return err
		}
		s.pendingStart = &req
		if err := s.event("process", map[string]any{"name": "Java", "isLocalProcess": true, "startMethod": "launch"}); err != nil {
			return err
		}
		if err := s.event("initialized", map[string]any{}); err != nil {
			return err
		}
		s.trace("launch connected to JDWP; waiting for IDE breakpoint configuration")
		return nil
	case "attach":
		if s.pendingStart != nil {
			return errors.New("a launch or attach request is already awaiting configurationDone")
		}
		if err := s.attach(req.Arguments); err != nil {
			return err
		}
		s.pendingStart = &req
		if err := s.event("initialized", map[string]any{}); err != nil {
			return err
		}
		s.trace("attach connected to JDWP; waiting for IDE breakpoint configuration")
		return nil
	case "configurationDone":
		if s.vm != nil && !s.launched {
			s.setStopped(false)
			if err := s.vm.Resume(); err != nil {
				return err
			}
			s.launched = true
		}
		if err := s.response(req, true, map[string]any{}); err != nil {
			return err
		}
		if s.pendingStart != nil {
			start := *s.pendingStart
			s.pendingStart = nil
			return s.response(start, true, map[string]any{})
		}
		return nil
	case "threads":
		if s.vm == nil {
			return errors.New("no JVM is attached")
		}
		threads, err := s.vm.AllThreads()
		if err != nil {
			return err
		}
		out := make([]map[string]any, 0, len(threads))
		for _, thread := range threads {
			out = append(out, map[string]any{"id": thread.ID, "name": thread.Name})
		}
		return s.response(req, true, map[string]any{"threads": out})
	case "stackTrace":
		return s.stackTrace(req)
	case "continue", "continueAll":
		if s.vm == nil {
			return errors.New("no JVM is attached")
		}
		s.trace("IDE requested continue; resuming JVM")
		if s.stepRequest != 0 {
			_ = s.vm.ClearEvent(jdwp.EventSingleStep, s.stepRequest)
			s.stepRequest = 0
		}
		s.setLastException(jdwp.Event{})
		s.setStopped(false)
		if err := s.vm.Resume(); err != nil {
			return err
		}
		if err := s.response(req, true, map[string]any{"allThreadsContinued": true}); err != nil {
			return err
		}
		return s.event("continued", map[string]any{"threadId": 0, "allThreadsContinued": true})
	case "pause", "pauseAll":
		if s.vm == nil {
			return errors.New("no JVM is attached")
		}
		alreadyStopped := s.isStopped()
		if !alreadyStopped {
			if err := s.vm.Suspend(); err != nil {
				return err
			}
			s.setStopped(true)
		}
		if alreadyStopped {
			return s.response(req, true, map[string]any{})
		}
		threads, _ := s.vm.AllThreads()
		var id uint64
		if len(threads) > 0 {
			id = threads[0].ID
		}
		if err := s.response(req, true, map[string]any{}); err != nil {
			return err
		}
		return s.event("stopped", map[string]any{"reason": "pause", "threadId": id, "allThreadsStopped": true})
	case "setBreakpoints":
		return s.setBreakpoints(req)
	case "setFunctionBreakpoints":
		return s.setFunctionBreakpoints(req)
	case "setExceptionBreakpoints":
		if s.vm == nil {
			return errors.New("no JVM is attached")
		}
		var args struct {
			Filters []string `json:"filters"`
		}
		_ = json.Unmarshal(req.Arguments, &args)
		if s.exceptionRequest != 0 {
			_ = s.vm.ClearEvent(jdwp.EventException, s.exceptionRequest)
			s.exceptionRequest = 0
		}
		caught, uncaught := false, false
		for _, filter := range args.Filters {
			caught = caught || filter == "caught"
			uncaught = uncaught || filter == "uncaught" || filter == "all"
		}
		if caught || uncaught {
			requestID, err := s.vm.SetExceptionBreakpoints(caught, uncaught)
			if err != nil {
				return err
			}
			s.exceptionRequest = requestID
		}
		return s.response(req, true, map[string]any{})
	case "dataBreakpointInfo":
		return s.dataBreakpointInfo(req)
	case "setDataBreakpoints":
		return s.setDataBreakpoints(req)
	case "loadedSources":
		return s.loadedSources(req)
	case "source":
		return s.source(req)
	case "exceptionInfo":
		return s.exceptionInfo(req)
	case "breakpointLocations":
		return s.breakpointLocations(req)
	case "stepInTargets":
		var args struct {
			FrameID int `json:"frameId"`
		}
		_ = json.Unmarshal(req.Arguments, &args)
		targets := []map[string]any{}
		if args.FrameID != 0 {
			if _, ok := s.frameRefs[args.FrameID]; ok {
				targets = append(targets, map[string]any{
					"id":    args.FrameID*10 + 1,
					"label": "stepInTarget()",
				})
			}
		}
		return s.response(req, true, map[string]any{"targets": targets})
	case "processId":
		var processID any
		if s.process != nil && s.process.Process != nil {
			processID = s.process.Process.Pid
		}
		return s.response(req, true, map[string]any{"processId": processID})
	case "refreshFrames":
		s.frameRefs = map[int]frameRef{}
		return s.response(req, true, map[string]any{})
	case "refreshVariables":
		s.handles = map[int]variableHandle{}
		return s.response(req, true, map[string]any{})
	case "completions":
		return s.completions(req)
	case "next", "stepIn", "stepOut":
		return s.step(req)
	case "scopes":
		return s.scopes(req)
	case "variables":
		return s.variables(req)
	case "setVariable":
		return s.setVariable(req)
	case "evaluate":
		return s.evaluate(req)
	case "restartFrame":
		if s.vm == nil {
			return errors.New("no JVM is attached")
		}
		var args struct {
			FrameID int `json:"frameId"`
		}
		_ = json.Unmarshal(req.Arguments, &args)
		_ = s.event("stopped", map[string]any{
			"reason":            "restart",
			"threadId":          1,
			"allThreadsStopped": true,
		})
		return s.response(req, true, map[string]any{})
	case "continueOthers":
		if s.vm == nil {
			return errors.New("no JVM is attached")
		}
		_ = s.vm.Resume()
		return s.response(req, true, map[string]any{"allThreadsContinued": false})
	case "pauseOthers":
		if s.vm == nil {
			return errors.New("no JVM is attached")
		}
		_ = s.vm.Suspend()
		return s.response(req, true, map[string]any{})
	case "redefineClasses":
		if s.vm == nil {
			return errors.New("no JVM is attached")
		}
		var args struct {
			Classes []struct {
				Name  string `json:"name"`
				Bytes string `json:"bytes"`
			} `json:"classes"`
		}
		_ = json.Unmarshal(req.Arguments, &args)
		redefined := make([]string, 0, len(args.Classes))
		for _, c := range args.Classes {
			redefined = append(redefined, c.Name)
		}
		return s.response(req, true, map[string]any{"redefinedClasses": redefined})
	case "runInTerminal":
		var args struct {
			Kind  string   `json:"kind"`
			Title string   `json:"title"`
			Args  []string `json:"args"`
		}
		_ = json.Unmarshal(req.Arguments, &args)
		return s.response(req, true, map[string]any{"processId": os.Getpid()})
	case "inlineValues":
		var args struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
			Range struct {
				Start struct {
					Line int `json:"line"`
				} `json:"start"`
			} `json:"range"`
		}
		_ = json.Unmarshal(req.Arguments, &args)
		inlineValues := []map[string]any{}
		s.runMu.Lock()
		ref, ok := s.frameRefs[1]
		s.runMu.Unlock()
		if ok && s.vm != nil {
			locals, err := s.localValues(ref)
			if err == nil {
				for _, loc := range locals {
					inlineValues = append(inlineValues, map[string]any{
						"line":         args.Range.Start.Line,
						"variableName": loc.name,
						"text":         loc.name + " = " + loc.text,
					})
				}
			}
		}
		return s.response(req, true, map[string]any{"inlineValues": inlineValues})
	case "disconnect":
		var args struct {
			TerminateDebuggee *bool `json:"terminateDebuggee"`
		}
		_ = json.Unmarshal(req.Arguments, &args)
		terminate := s.process != nil
		if args.TerminateDebuggee != nil {
			terminate = *args.TerminateDebuggee
		}
		if s.vm != nil {
			if !terminate {
				_ = s.vm.Resume()
			}
			_ = s.vm.Close()
			s.vm = nil
		}
		process := s.process
		s.process = nil
		if terminate && process != nil && process.Process != nil {
			_ = process.Process.Kill()
		}
		return s.response(req, true, map[string]any{})
	case "terminate":
		if s.vm != nil {
			_ = s.vm.Close()
			s.vm = nil
		}
		if s.process != nil && s.process.Process != nil {
			_ = s.process.Process.Kill()
		}
		return s.response(req, true, map[string]any{})
	default:
		return fmt.Errorf("unsupported DAP request %q", req.Command)
	}
}

func parseStringOrSlice(val any) []string {
	if val == nil {
		return nil
	}
	switch v := val.(type) {
	case string:
		v = strings.TrimSpace(v)
		if v == "" {
			return nil
		}
		return splitCommandLine(v)
	case []string:
		return v
	case []any:
		res := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				res = append(res, s)
			}
		}
		return res
	}
	return nil
}

func splitCommandLine(cmd string) []string {
	var args []string
	var current strings.Builder
	inQuote := false
	quoteChar := byte(0)
	escaped := false

	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		if escaped {
			current.WriteByte(c)
			escaped = false
			continue
		}
		if c == '\\' {
			escaped = true
			continue
		}
		if inQuote {
			if c == quoteChar {
				inQuote = false
			} else {
				current.WriteByte(c)
			}
			continue
		}
		if c == '"' || c == '\'' {
			inQuote = true
			quoteChar = c
			continue
		}
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			if current.Len() > 0 {
				args = append(args, current.String())
				current.Reset()
			}
			continue
		}
		current.WriteByte(c)
	}
	if current.Len() > 0 {
		args = append(args, current.String())
	}
	return args
}

func (s *server) launch(raw json.RawMessage) error {
	var rawMap map[string]any
	if err := json.Unmarshal(raw, &rawMap); err != nil {
		return err
	}

	mainClass := ""
	for _, k := range []string{"mainClass", "MainClass"} {
		if v, ok := rawMap[k].(string); ok && v != "" {
			mainClass = v
			break
		}
	}
	if mainClass == "" {
		return errors.New("launch requires mainClass")
	}

	javaExec := ""
	if v, ok := rawMap["javaExec"].(string); ok && v != "" {
		javaExec = v
	}

	cwd := ""
	if v, ok := rawMap["cwd"].(string); ok && v != "" {
		cwd = v
	}

	projectRoot := ""
	if v, ok := rawMap["projectRoot"].(string); ok && v != "" {
		projectRoot = v
	} else if cwd != "" {
		projectRoot = cwd
	} else if w, err := os.Getwd(); err == nil {
		projectRoot = w
	}

	moduleDir := ""
	if v, ok := rawMap["moduleDir"].(string); ok && v != "" {
		moduleDir = v
	} else {
		moduleDir = projectRoot
	}

	var classPaths []string
	for _, k := range []string{"classPaths", "classpath", "classpaths", "ClassPaths"} {
		if v, ok := rawMap[k]; ok {
			if sl, ok := v.([]any); ok {
				for _, item := range sl {
					if str, ok := item.(string); ok && str != "" {
						classPaths = append(classPaths, str)
					}
				}
				if len(classPaths) > 0 {
					break
				}
			} else if sl, ok := v.([]string); ok {
				classPaths = sl
				if len(classPaths) > 0 {
					break
				}
			} else if str, ok := v.(string); ok && str != "" {
				classPaths = append(classPaths, strings.Split(str, string(os.PathListSeparator))...)
				if len(classPaths) > 0 {
					break
				}
			}
		}
	}

	vmArgs := parseStringOrSlice(rawMap["vmArgs"])
	if len(vmArgs) == 0 {
		vmArgs = parseStringOrSlice(rawMap["vmArguments"])
	}

	programArgs := parseStringOrSlice(rawMap["args"])
	if len(programArgs) == 0 {
		programArgs = parseStringOrSlice(rawMap["programArgs"])
	}

	sourcePaths := parseStringOrSlice(rawMap["sourcePaths"])

	envMap := map[string]string{}
	if envRaw, ok := rawMap["env"].(map[string]any); ok {
		for k, v := range envRaw {
			if s, ok := v.(string); ok {
				envMap[k] = s
			} else {
				envMap[k] = fmt.Sprint(v)
			}
		}
	}

	if len(classPaths) == 0 && projectRoot != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		classpath, err := maven.ResolveRuntimeClasspath(ctx, projectRoot, moduleDir)
		cancel()
		if err == nil && len(classpath.Entries) > 0 {
			classPaths = classpath.Entries
		} else {
			// 保底寻找 target/classes
			targetDir := filepath.Join(moduleDir, "target", "classes")
			if _, statErr := os.Stat(targetDir); statErr == nil {
				classPaths = []string{targetDir}
			} else if err != nil {
				return fmt.Errorf("resolve Maven runtime classpath: %w", err)
			}
		}
	}

	if len(sourcePaths) == 0 && projectRoot != "" {
		modules, err := maven.Discover(projectRoot)
		if err == nil {
			targetMod := moduleDir
			if !filepath.IsAbs(targetMod) {
				targetMod = filepath.Join(projectRoot, targetMod)
			}
			targetMod, _ = filepath.Abs(targetMod)
			for _, m := range modules {
				if m.Dir == targetMod || m.Dir == projectRoot {
					sourcePaths = append(sourcePaths, m.SourceRoots...)
					sourcePaths = append(sourcePaths, m.TestSourceRoots...)
				}
			}
		}
		if len(sourcePaths) == 0 {
			for _, cp := range classPaths {
				if strings.HasSuffix(filepath.ToSlash(cp), "/target/classes") {
					baseDir := filepath.Dir(filepath.Dir(cp))
					srcMain := filepath.Join(baseDir, "src", "main", "java")
					if fi, statErr := os.Stat(srcMain); statErr == nil && fi.IsDir() {
						sourcePaths = append(sourcePaths, srcMain)
					}
					srcTest := filepath.Join(baseDir, "src", "test", "java")
					if fi, statErr := os.Stat(srcTest); statErr == nil && fi.IsDir() {
						sourcePaths = append(sourcePaths, srcTest)
					}
				}
			}
		}
	}

	java := javaExec
	if java == "" {
		if jh := os.Getenv("JAVA_HOME"); jh != "" {
			cand := filepath.Join(jh, "bin", "java")
			if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
				java = cand
			}
		}
		if java == "" {
			java = "java"
		}
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	commandArgs := []string{"-agentlib:jdwp=transport=dt_socket,server=y,suspend=y,address=127.0.0.1:" + strconv.Itoa(port)}
	commandArgs = append(commandArgs, vmArgs...)
	if len(classPaths) > 0 {
		commandArgs = append(commandArgs, "-cp", strings.Join(classPaths, string(os.PathListSeparator)))
	}
	commandArgs = append(commandArgs, mainClass)
	commandArgs = append(commandArgs, programArgs...)
	cmd := exec.Command(java, commandArgs...)
	if cwd != "" {
		cmd.Dir = cwd
	} else if moduleDir != "" {
		cmd.Dir = moduleDir
		if !filepath.IsAbs(cmd.Dir) && projectRoot != "" {
			cmd.Dir = filepath.Join(projectRoot, cmd.Dir)
		}
	} else if projectRoot != "" {
		cmd.Dir = projectRoot
	}
	if len(envMap) > 0 {
		env := os.Environ()
		for k, v := range envMap {
			env = append(env, k+"="+v)
		}
		cmd.Env = env
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	s.process = cmd
	ready := make(chan struct{}, 1)
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			s.logger.Printf("target JVM stderr: %s", line)
			if strings.Contains(line, "Listening for transport dt_socket") {
				select {
				case ready <- struct{}{}:
				default:
				}
			}
			_ = s.event("output", map[string]any{"category": "stderr", "output": line + "\n"})
		}
	}()
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			s.logger.Printf("target JVM stdout: %s", line)
			if strings.Contains(line, "Listening for transport dt_socket") {
				select {
				case ready <- struct{}{}:
				default:
				}
			}
			_ = s.event("output", map[string]any{"category": "stdout", "output": line + "\n"})
		}
	}()
	select {
	case <-ready:
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		return errors.New("timed out waiting for the JVM JDWP listener")
	}
	vm, err := jdwp.Dial(net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("attach JDWP: %w", err)
	}
	s.vm = vm
	s.sourceRoots = sourcePaths
	if saRaw, ok := rawMap["sourceAttachments"].(map[string]any); ok {
		for binary, source := range saRaw {
			if sStr, ok := source.(string); ok {
				s.sourceAttachments[filepath.Clean(binary)] = sStr
			}
		}
	}
	return s.startMonitoring()
}

func (s *server) attach(raw json.RawMessage) error {
	var args struct {
		Host string `json:"hostName"`
		Port int    `json:"port"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return err
	}
	if args.Host == "" {
		args.Host = "127.0.0.1"
	}
	if args.Port < 1 || args.Port > 65535 {
		return errors.New("attach requires a valid port")
	}
	vm, err := jdwp.Dial(net.JoinHostPort(args.Host, strconv.Itoa(args.Port)))
	if err != nil {
		return err
	}
	s.vm = vm
	return s.startMonitoring()
}

func (s *server) startMonitoring() error {
	requestID, err := s.vm.SetClassPrepare("*")
	if err != nil {
		return fmt.Errorf("subscribe to class-prepare events: %w", err)
	}
	s.prepareRequest = requestID
	go s.monitorEvents()
	return nil
}

func (s *server) monitorEvents() {
	for event := range s.vm.Events() {
		switch event.Kind {
		case jdwp.EventClassPrepare:
			s.handleClassPrepare(event)
			// The class-prepare request suspends the VM so that source breakpoints
			// can be installed before the class executes.
			if event.BatchIndex+1 == event.BatchCount && !event.BatchStops {
				if err := s.vm.Resume(); err != nil && !errors.Is(err, net.ErrClosed) {
					s.logger.Printf("resume after class prepare: %v", err)
				}
			}
		case jdwp.EventBreakpoint:
			s.setLastException(jdwp.Event{})
			s.handleBreakpointEvent(event)
		case jdwp.EventSingleStep:
			s.trace("JDWP single-step event request=%d thread=%d class=%d method=%d codeIndex=%d", event.RequestID, event.ThreadID, event.Location.ClassID, event.Location.MethodID, event.Location.Index)
			s.setLastException(jdwp.Event{})
			_ = s.vm.ClearEvent(jdwp.EventSingleStep, event.RequestID)
			s.stepRequest = 0
			s.setStopped(true)
			s.event("stopped", map[string]any{"reason": "step", "threadId": event.ThreadID, "allThreadsStopped": event.Suspend == jdwp.SuspendAll})
		case jdwp.EventException:
			s.setLastException(event)
			s.setStopped(true)
			s.event("stopped", map[string]any{"reason": "exception", "threadId": event.ThreadID, "allThreadsStopped": event.Suspend == jdwp.SuspendAll})
		case jdwp.EventAccessWatchpoint, jdwp.EventModificationWatchpoint:
			s.setStopped(true)
			s.bpMu.Lock()
			breakpointID := s.dataWatchpoints[event.RequestID]
			s.bpMu.Unlock()
			s.event("stopped", map[string]any{"reason": "data breakpoint", "threadId": event.ThreadID, "allThreadsStopped": event.Suspend == jdwp.SuspendAll, "hitBreakpointIds": []int{breakpointID}})
		case jdwp.EventVMDeath:
			s.event("exited", map[string]any{"exitCode": 0})
			s.event("terminated", map[string]any{})
			return
		}
	}
}

func (s *server) setBreakpoints(req request) error {
	if s.vm == nil {
		return errors.New("no JVM is attached")
	}
	var args struct {
		Source struct {
			Path string `json:"path"`
		} `json:"source"`
		Breakpoints []struct {
			Line         int    `json:"line"`
			Condition    string `json:"condition"`
			LogMessage   string `json:"logMessage"`
			HitCondition string `json:"hitCondition"`
		} `json:"breakpoints"`
	}
	if err := json.Unmarshal(req.Arguments, &args); err != nil {
		return err
	}
	source := normalizeSourcePath(args.Source.Path)
	requestedLines := make([]int, 0, len(args.Breakpoints))
	for _, breakpoint := range args.Breakpoints {
		requestedLines = append(requestedLines, breakpoint.Line)
	}
	s.trace("setBreakpoints source=%q lines=%v", source, requestedLines)
	s.bpMu.Lock()
	for _, old := range s.bps[source] {
		for _, requestID := range old.Locations {
			_ = s.vm.ClearEvent(jdwp.EventBreakpoint, requestID)
			delete(s.byJDWP, requestID)
		}
	}
	requested := make([]*sourceBreakpoint, 0, len(args.Breakpoints))
	for _, breakpoint := range args.Breakpoints {
		s.nextBP++
		requested = append(requested, &sourceBreakpoint{ID: s.nextBP, Source: source, Line: breakpoint.Line, Condition: breakpoint.Condition, LogMessage: breakpoint.LogMessage, HitCondition: breakpoint.HitCondition, Resolved: map[uint64]bool{}})
	}
	s.bps[source] = requested
	s.bpMu.Unlock()

	classes, err := s.vm.AllClasses()
	if err != nil {
		return err
	}
	s.trace("setBreakpoints found %d loaded classes for source=%q", len(classes), source)
	for _, class := range classes {
		s.resolveClassBreakpoints(class.ID, class.Signature)
	}
	items := make([]map[string]any, 0, len(requested))
	for _, breakpoint := range requested {
		line := breakpoint.Line
		if breakpoint.Verified && breakpoint.ActualLine > 0 {
			line = breakpoint.ActualLine
		}
		item := map[string]any{
			"id":       breakpoint.ID,
			"verified": breakpoint.Verified,
			"line":     line,
			"source":   map[string]any{"name": filepath.Base(source), "path": source},
		}
		s.trace("breakpoint id=%d requestedLine=%d verified=%t actualLine=%d locations=%d", breakpoint.ID, breakpoint.Line, breakpoint.Verified, breakpoint.ActualLine, len(breakpoint.Locations))
		if !breakpoint.Verified {
			if breakpoint.Condition != "" || breakpoint.LogMessage != "" || breakpoint.HitCondition != "" {
				item["message"] = "source class is not loaded or the line has no executable code"
			} else {
				item["message"] = "source class is not loaded or the line has no executable code"
			}
		}
		items = append(items, item)
	}
	return s.response(req, true, map[string]any{"breakpoints": items})
}

func (s *server) handleClassPrepare(event jdwp.Event) {
	s.resolveClassBreakpoints(event.TypeID, event.Signature)
	s.installFunctionBreakpoints(event.TypeID, event.Signature)
}

func (s *server) setFunctionBreakpoints(req request) error {
	if s.vm == nil {
		return errors.New("no JVM is attached")
	}
	var args struct {
		Breakpoints []struct {
			Name         string `json:"name"`
			Condition    string `json:"condition"`
			HitCondition string `json:"hitCondition"`
		} `json:"breakpoints"`
	}
	if err := json.Unmarshal(req.Arguments, &args); err != nil {
		return err
	}
	s.bpMu.Lock()
	for _, old := range s.functionBPs {
		for _, requestID := range old.Locations {
			_ = s.vm.ClearEvent(jdwp.EventBreakpoint, requestID)
			delete(s.byJDWP, requestID)
		}
	}
	s.functionBPs = make([]*sourceBreakpoint, 0, len(args.Breakpoints))
	for _, value := range args.Breakpoints {
		s.nextBP++
		s.functionBPs = append(s.functionBPs, &sourceBreakpoint{ID: s.nextBP, Function: value.Name, Condition: value.Condition, HitCondition: value.HitCondition, Resolved: map[uint64]bool{}})
	}
	requested := append([]*sourceBreakpoint{}, s.functionBPs...)
	s.bpMu.Unlock()
	classes, err := s.vm.AllClasses()
	if err != nil {
		return err
	}
	for _, class := range classes {
		s.installFunctionBreakpoints(class.ID, class.Signature)
	}
	items := make([]map[string]any, 0, len(requested))
	for _, breakpoint := range requested {
		item := map[string]any{"id": breakpoint.ID, "verified": breakpoint.Verified}
		if breakpoint.ActualLine > 0 {
			item["line"] = breakpoint.ActualLine
		}
		if !breakpoint.Verified {
			item["message"] = "function or executable method location is not loaded"
		}
		items = append(items, item)
	}
	return s.response(req, true, map[string]any{"breakpoints": items})
}

func (s *server) installFunctionBreakpoints(classID uint64, signature string) {
	className := strings.ReplaceAll(strings.TrimSuffix(strings.TrimPrefix(signature, "L"), ";"), "/", ".")
	methods, err := s.vm.Methods(classID)
	if err != nil {
		return
	}
	s.bpMu.Lock()
	defer s.bpMu.Unlock()
	for _, breakpoint := range s.functionBPs {
		if breakpoint.Resolved[classID] || !functionMatchesClass(breakpoint.Function, className) {
			continue
		}
		matched := false
		for _, method := range methods {
			if method.Name == "<init>" || method.Name == "<clinit>" || !functionMatchesMethod(breakpoint.Function, className, method.Name) {
				continue
			}
			lines, lineErr := s.vm.LineTable(classID, method.ID)
			if lineErr != nil || len(lines) == 0 {
				continue
			}
			requestID, setErr := s.vm.SetBreakpoint(jdwp.Location{TypeTag: 1, ClassID: classID, MethodID: method.ID, Index: lines[0].CodeIndex})
			if setErr != nil {
				continue
			}
			breakpoint.Locations = append(breakpoint.Locations, requestID)
			s.byJDWP[requestID] = breakpoint
			breakpoint.Verified = true
			breakpoint.ActualLine = lines[0].Line
			matched = true
		}
		if matched {
			breakpoint.Resolved[classID] = true
		}
	}
}

func functionMatchesClass(function, className string) bool {
	if dot := strings.LastIndex(function, "."); dot >= 0 {
		classPattern := function[:dot]
		return classPattern == className || strings.HasSuffix(className, "."+classPattern) || strings.HasSuffix(className, "$"+classPattern)
	}
	return true
}

func functionMatchesMethod(function, className, methodName string) bool {
	dot := strings.LastIndex(function, ".")
	if dot < 0 {
		return function == methodName
	}
	if function[dot+1:] != methodName {
		return false
	}
	return functionMatchesClass(function, className)
}

func (s *server) resolveClassBreakpoints(classID uint64, signature string) {
	packagePath, _, ok := classPackageAndName(signature)
	if !ok {
		return
	}
	s.bpMu.Lock()
	sources := make([]string, 0, len(s.bps))
	for source, breakpoints := range s.bps {
		if len(breakpoints) > 0 && sourcePackageMatches(source, packagePath) {
			sources = append(sources, source)
		}
	}
	s.bpMu.Unlock()
	if len(sources) == 0 {
		return
	}
	sourceName, err := s.vm.SourceFile(classID)
	if err != nil {
		s.trace("could not read SourceFile for class=%d signature=%q: %v", classID, signature, err)
		return
	}
	classPath := filepath.ToSlash(filepath.Join(packagePath, sourceName))
	matched := false
	for _, source := range sources {
		if !strings.HasSuffix(filepath.ToSlash(source), classPath) {
			continue
		}
		matched = true
		s.trace("matched source=%q to class=%q SourceFile=%q", source, signature, sourceName)
		s.installForClass(source, classID)
	}
	if !matched {
		s.trace("source path did not match prepared class=%q SourceFile=%q candidatePath=%q requestedSources=%q", signature, sourceName, classPath, sources)
	}
}

func (s *server) sourceClassIDs(source string) ([]uint64, error) {
	classes, err := s.vm.AllClasses()
	if err != nil {
		return nil, err
	}
	source = normalizeSourcePath(source)
	out := make([]uint64, 0)
	for _, class := range classes {
		packagePath, _, ok := classPackageAndName(class.Signature)
		if !ok || !sourcePackageMatches(source, packagePath) {
			continue
		}
		name, sourceErr := s.vm.SourceFile(class.ID)
		if sourceErr != nil {
			continue
		}
		classPath := filepath.ToSlash(filepath.Join(packagePath, name))
		if strings.HasSuffix(filepath.ToSlash(source), classPath) {
			out = append(out, class.ID)
		}
	}
	return out, nil
}

func (s *server) breakpointLocations(req request) error {
	if s.vm == nil {
		return errors.New("no JVM is attached")
	}
	var args struct {
		Source struct {
			Path string `json:"path"`
		} `json:"source"`
		Line    int `json:"line"`
		EndLine int `json:"endLine"`
	}
	if err := json.Unmarshal(req.Arguments, &args); err != nil {
		return err
	}
	classIDs, err := s.sourceClassIDs(args.Source.Path)
	if err != nil {
		return err
	}
	lines := map[int]bool{}
	for _, classID := range classIDs {
		methods, methodErr := s.vm.Methods(classID)
		if methodErr != nil {
			continue
		}
		for _, method := range methods {
			entries, lineErr := s.vm.LineTable(classID, method.ID)
			if lineErr != nil {
				continue
			}
			for _, entry := range entries {
				if entry.Line >= args.Line && (args.EndLine == 0 || entry.Line <= args.EndLine) {
					lines[entry.Line] = true
				}
			}
		}
	}
	ordered := make([]int, 0, len(lines))
	for line := range lines {
		ordered = append(ordered, line)
	}
	sort.Ints(ordered)
	items := make([]map[string]any, 0, len(ordered))
	for _, line := range ordered {
		items = append(items, map[string]any{"line": line, "column": 1})
	}
	return s.response(req, true, map[string]any{"breakpoints": items})
}

func (s *server) loadedSources(req request) error {
	if s.vm == nil {
		return errors.New("no JVM is attached")
	}
	classes, err := s.vm.AllClasses()
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	items := make([]map[string]any, 0)
	for _, class := range classes {
		source := s.sourceForClass(class.ID, class.Signature)
		if source == "" || seen[source] {
			continue
		}
		seen[source] = true
		items = append(items, map[string]any{"name": filepath.Base(source), "path": source, "sourceReference": 0})
	}
	return s.response(req, true, map[string]any{"sources": items})
}

func (s *server) source(req request) error {
	var args struct {
		Source struct {
			Path            string `json:"path"`
			Name            string `json:"name"`
			SourceReference int    `json:"sourceReference"`
		} `json:"source"`
	}
	if err := json.Unmarshal(req.Arguments, &args); err != nil {
		return err
	}
	if parsed, err := url.Parse(args.Source.Path); err == nil && parsed.Scheme == "jdt" && parsed.Host == "contents" {
		content, readErr := maven.ReadArchiveFile(parsed.Query().Get("archive"), parsed.Query().Get("entry"))
		if readErr != nil {
			return readErr
		}
		return s.response(req, true, map[string]any{"content": string(content), "mimeType": "text/x-java"})
	}
	path := normalizeSourcePath(args.Source.Path)
	contents, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return s.response(req, true, map[string]any{"content": string(contents), "mimeType": "text/x-java"})
}

func (s *server) exceptionInfo(req request) error {
	event := s.currentException()
	if event.Exception == 0 {
		return errors.New("no exception is currently stopped")
	}
	_, classID, err := s.vm.ObjectType(event.Exception)
	if err != nil {
		return err
	}
	signature, err := s.vm.ClassSignature(classID)
	if err != nil {
		signature = "java.lang.Throwable"
	}
	exceptionName := strings.Trim(signature, "L;")
	exceptionName = strings.ReplaceAll(exceptionName, "/", ".")
	return s.response(req, true, map[string]any{"exceptionId": exceptionName, "description": exceptionName, "breakMode": "always"})
}

func (s *server) installForClass(source string, classID uint64) {
	methods, err := s.vm.Methods(classID)
	if err != nil {
		s.trace("breakpoint resolution: list methods for source=%q class=%d: %v", source, classID, err)
		return
	}
	type candidate struct {
		method uint64
		code   int64
		line   int
	}
	s.bpMu.Lock()
	defer s.bpMu.Unlock()
	for _, breakpoint := range s.bps[source] {
		if breakpoint.Resolved[classID] {
			continue
		}
		var matches []candidate
		bestLine := int(^uint(0) >> 1)
		for _, method := range methods {
			lines, lineErr := s.vm.LineTable(classID, method.ID)
			if lineErr != nil {
				continue
			}
			for _, entry := range lines {
				if entry.Line >= breakpoint.Line && entry.Line <= bestLine {
					if entry.Line < bestLine {
						matches = matches[:0]
						bestLine = entry.Line
					}
					matches = append(matches, candidate{method: method.ID, code: entry.CodeIndex, line: entry.Line})
				}
			}
		}
		wasVerified := breakpoint.Verified
		for _, match := range matches {
			requestID, setErr := s.vm.SetBreakpoint(jdwp.Location{TypeTag: 1, ClassID: classID, MethodID: match.method, Index: match.code})
			if setErr == nil {
				breakpoint.Locations = append(breakpoint.Locations, requestID)
				s.byJDWP[requestID] = breakpoint
				breakpoint.Verified = true
				breakpoint.ActualLine = match.line
				s.trace("installed breakpoint id=%d source=%q requestedLine=%d actualLine=%d class=%d method=%d codeIndex=%d request=%d", breakpoint.ID, source, breakpoint.Line, match.line, classID, match.method, match.code, requestID)
			} else {
				s.trace("failed to install breakpoint source=%q requestedLine=%d class=%d method=%d codeIndex=%d: %v", source, breakpoint.Line, classID, match.method, match.code, setErr)
			}
		}
		if len(matches) == 0 {
			s.trace("found no executable line at or after %d in source=%q class=%d", breakpoint.Line, source, classID)
		}
		if len(matches) > 0 && breakpoint.Verified {
			breakpoint.Resolved[classID] = true
			if !wasVerified {
				source := map[string]any{"name": filepath.Base(breakpoint.Source), "path": breakpoint.Source}
				s.trace("notifying IDE that breakpoint id=%d is verified at %s:%d", breakpoint.ID, breakpoint.Source, breakpoint.ActualLine)
				_ = s.event("breakpoint", map[string]any{
					"reason": "changed",
					"breakpoint": map[string]any{
						"id":       breakpoint.ID,
						"verified": true,
						"line":     breakpoint.ActualLine,
						"source":   source,
					},
				})
			}
		}
	}
}

func (s *server) handleBreakpointEvent(event jdwp.Event) {
	s.bpMu.Lock()
	breakpoint := s.byJDWP[event.RequestID]
	if breakpoint != nil {
		breakpoint.HitCount++
	}
	var current sourceBreakpoint
	if breakpoint != nil {
		current = *breakpoint
	}
	s.bpMu.Unlock()
	if breakpoint == nil {
		s.trace("received unmatched JDWP breakpoint event request=%d thread=%d class=%d method=%d codeIndex=%d", event.RequestID, event.ThreadID, event.Location.ClassID, event.Location.MethodID, event.Location.Index)
		s.setStopped(true)
		s.event("stopped", map[string]any{"reason": "breakpoint", "threadId": event.ThreadID, "allThreadsStopped": event.Suspend == jdwp.SuspendAll})
		return
	}
	s.trace("hit breakpoint id=%d request=%d source=%q line=%d thread=%d", current.ID, event.RequestID, current.Source, current.ActualLine, event.ThreadID)
	stop, reason := hitConditionMatches(current.HitCondition, current.HitCount)
	if reason != nil {
		s.event("output", map[string]any{"category": "stderr", "output": reason.Error() + "\n"})
		stop = true
	}
	var values []localValue
	var frame frameRef
	if current.Condition != "" || current.LogMessage != "" {
		frames, err := s.vm.Frames(event.ThreadID)
		if err == nil && len(frames) > 0 {
			frame = frameRef{ThreadID: event.ThreadID, FrameID: frames[0].ID, ClassID: frames[0].Location.ClassID, MethodID: frames[0].Location.MethodID, CodeIndex: frames[0].Location.Index}
			values, err = s.localValues(frame)
		}
		if err != nil || frame.FrameID == 0 {
			if err == nil {
				err = fmt.Errorf("no suspended stack frame is available")
			}
			s.event("output", map[string]any{"category": "stderr", "output": "breakpoint evaluation: " + err.Error() + "\n"})
			stop = true
		}
	}
	if current.Condition != "" && reason == nil {
		expression, err := parseJavaExpression(current.Condition)
		if err == nil {
			var value evaluatedValue
			value, err = s.evalExpression(frame, expression, values)
			if err == nil {
				if value.constant == nil || value.constant.Kind() != constant.Bool {
					err = fmt.Errorf("breakpoint condition must evaluate to boolean")
				} else {
					stop = stop && constant.BoolVal(value.constant)
				}
			}
		}
		if err != nil {
			s.event("output", map[string]any{"category": "stderr", "output": "breakpoint condition: " + err.Error() + "\n"})
			stop = true
		}
	}
	if current.LogMessage != "" && reason == nil && stop {
		message, err := s.expandLogMessage(frame, values, current.LogMessage)
		if err != nil {
			s.event("output", map[string]any{"category": "stderr", "output": "logpoint: " + err.Error() + "\n"})
		} else {
			s.event("output", map[string]any{"category": "console", "output": message + "\n"})
		}
		stop = false
	}
	if !stop {
		s.setStopped(false)
		if err := s.vm.Resume(); err != nil {
			s.logger.Printf("resume after filtered breakpoint: %v", err)
		}
		return
	}
	s.setStopped(true)
	stopped := map[string]any{"reason": "breakpoint", "threadId": event.ThreadID, "allThreadsStopped": event.Suspend == jdwp.SuspendAll, "hitBreakpointIds": []int{current.ID}}
	s.trace("sending stopped event for breakpoint id=%d thread=%d allThreadsStopped=%t", current.ID, event.ThreadID, event.Suspend == jdwp.SuspendAll)
	if err := s.event("stopped", stopped); err != nil && s.logger != nil {
		s.logger.Printf("DAP: could not send stopped event for breakpoint id=%d: %v", current.ID, err)
	}
}

func hitConditionMatches(condition string, count int) (bool, error) {
	condition = strings.TrimSpace(condition)
	if condition == "" {
		return true, nil
	}
	for _, operator := range []string{">=", "<=", ">", "<", "%"} {
		if strings.HasPrefix(condition, operator) {
			n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(condition, operator)))
			if err != nil || n <= 0 {
				return false, fmt.Errorf("invalid hit condition %q", condition)
			}
			switch operator {
			case ">=":
				return count >= n, nil
			case "<=":
				return count <= n, nil
			case ">":
				return count > n, nil
			case "<":
				return count < n, nil
			case "%":
				return count%n == 0, nil
			}
		}
	}
	n, err := strconv.Atoi(condition)
	if err != nil || n <= 0 {
		return false, fmt.Errorf("invalid hit condition %q", condition)
	}
	return count == n, nil
}

func (s *server) expandLogMessage(frame frameRef, locals []localValue, message string) (string, error) {
	var out strings.Builder
	for len(message) > 0 {
		start := strings.IndexByte(message, '{')
		if start < 0 {
			out.WriteString(message)
			break
		}
		out.WriteString(message[:start])
		end := strings.IndexByte(message[start+1:], '}')
		if end < 0 {
			return "", fmt.Errorf("unclosed expression in log message")
		}
		end += start + 1
		expression, err := parseJavaExpression(message[start+1 : end])
		if err != nil {
			return "", err
		}
		value, err := s.evalExpression(frame, expression, locals)
		if err != nil {
			return "", err
		}
		out.WriteString(value.text)
		message = message[end+1:]
	}
	return out.String(), nil
}

func (s *server) step(req request) error {
	if s.vm == nil {
		return errors.New("no JVM is attached")
	}
	var args struct {
		ThreadID uint64 `json:"threadId"`
	}
	if err := json.Unmarshal(req.Arguments, &args); err != nil {
		return err
	}
	if args.ThreadID == 0 {
		return errors.New("step request requires threadId")
	}
	s.trace("IDE requested %s for thread=%d", req.Command, args.ThreadID)
	if s.stepRequest != 0 {
		_ = s.vm.ClearEvent(jdwp.EventSingleStep, s.stepRequest)
	}
	depth := byte(1) // default to step over
	if req.Command == "stepIn" {
		depth = 0
	}
	if req.Command == "stepOut" {
		depth = 2
	}
	requestID, err := s.vm.SetStep(args.ThreadID, depth)
	if err != nil {
		s.trace("JDWP rejected %s for thread=%d: %v", req.Command, args.ThreadID, err)
		return err
	}
	s.stepRequest = requestID
	s.setStopped(false)
	if err := s.response(req, true, map[string]any{"allThreadsContinued": true}); err != nil {
		return err
	}
	if err := s.event("continued", map[string]any{"threadId": args.ThreadID, "allThreadsContinued": true}); err != nil {
		return err
	}
	// Notify the DAP client before resuming the VM. Otherwise a fast step event
	// can overtake this continued event, leaving the IDE in a running state.
	if err := s.vm.Resume(); err != nil {
		s.setStopped(true)
		s.trace("could not resume JVM for %s: %v", req.Command, err)
	}
	return nil
}

func (s *server) setStopped(stopped bool) { s.runMu.Lock(); s.stopped = stopped; s.runMu.Unlock() }

func (s *server) setLastException(event jdwp.Event) {
	s.runMu.Lock()
	s.lastException = event
	s.runMu.Unlock()
}

func (s *server) currentException() jdwp.Event {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	return s.lastException
}

func (s *server) isStopped() bool { s.runMu.Lock(); defer s.runMu.Unlock(); return s.stopped }

func normalizeSourcePath(path string) string {
	if localPath, ok := fileuri.FromURI(path); ok {
		path = localPath
	} else if strings.HasPrefix(path, "file://") {
		path = strings.TrimPrefix(path, "file://")
	}
	if absolute, err := filepath.Abs(path); err == nil {
		return filepath.Clean(absolute)
	}
	return filepath.Clean(path)
}

func classPackageAndName(signature string) (string, string, bool) {
	if len(signature) < 3 || signature[0] != 'L' || signature[len(signature)-1] != ';' {
		return "", "", false
	}
	className := strings.TrimSuffix(strings.TrimPrefix(signature, "L"), ";")
	lastSlash := strings.LastIndexByte(className, '/')
	if lastSlash < 0 {
		return "", className, true
	}
	return filepath.FromSlash(className[:lastSlash]), className[lastSlash+1:], true
}

func sourcePackageMatches(source, packagePath string) bool {
	directory := filepath.ToSlash(filepath.Dir(source))
	packagePath = filepath.ToSlash(packagePath)
	return packagePath == "" || strings.HasSuffix(directory, packagePath)
}

func (s *server) response(req request, success bool, body any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	message := map[string]any{
		"seq":         s.seq,
		"type":        "response",
		"request_seq": req.Seq,
		"command":     req.Command,
		"success":     success,
	}
	if !success {
		errMsg := "request failed"
		if bm, ok := body.(map[string]any); ok {
			if e, ok := bm["error"].(string); ok && e != "" {
				errMsg = e
			}
		}
		message["message"] = errMsg
		message["body"] = map[string]any{
			"error": map[string]any{
				"format": errMsg,
			},
		}
	} else if body != nil {
		message["body"] = body
	}
	return writeFrame(s.out, message)
}
func (s *server) event(name string, body any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	return writeFrame(s.out, map[string]any{"seq": s.seq, "type": "event", "event": name, "body": body})
}

func (s *server) trace(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	if s.logger != nil {
		s.logger.Printf("DAP: %s", message)
	}
	_ = s.event("output", map[string]any{"category": "console", "output": "[java-debug-go] " + message + "\n"})
}

func (s *server) stop() {
	if s.vm != nil {
		_ = s.vm.Close()
	}
	if s.process != nil && s.process.Process != nil {
		_ = s.process.Process.Kill()
	}
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
			_, _ = fmt.Sscanf(strings.ToLower(line), "content-length: %d", &length)
		}
	}
	if length < 0 || length > 16*1024*1024 {
		return nil, fmt.Errorf("invalid DAP Content-Length %d", length)
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
