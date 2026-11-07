package tests

// ProtocolKind 表示协议类别的枚举类型
type ProtocolKind string

const (
	ProtocolKindLSPStandard    ProtocolKind = "LSP_STANDARD"
	ProtocolKindJDTExtension   ProtocolKind = "JDT_EXTENSION"
	ProtocolKindDAPStandard    ProtocolKind = "DAP_STANDARD"
	ProtocolKindJavaDebugExt   ProtocolKind = "JAVA_DEBUG_EXTENSION"
)

// ParityStatus 表示重构与行为对齐状态的枚举类型
type ParityStatus string

const (
	// ParityStatusFull 表示完全重构且核心行为对齐
	ParityStatusFull ParityStatus = "FULLY_ALIGNED"
	// ParityStatusPartial 表示部分重构（实现了子集能力，但缺少高级语义、复杂推导或扩展参数）
	ParityStatusPartial ParityStatus = "PARTIALLY_ALIGNED"
	// ParityStatusExplicitlyUnsupported 表示协议有明确定义，服务端明确拦截并返回不支持错误
	ParityStatusExplicitlyUnsupported ParityStatus = "EXPLICITLY_UNSUPPORTED"
	// ParityStatusMissing 表示尚未重构/缺失实现
	ParityStatusMissing ParityStatus = "MISSING_FROM_REWRITE"
)

// LSPMethod 表示 LSP 标准方法名的枚举类型
type LSPMethod string

const (
	LSPMethodInitialize              LSPMethod = "initialize"
	LSPMethodInitialized             LSPMethod = "initialized"
	LSPMethodShutdown                LSPMethod = "shutdown"
	LSPMethodExit                    LSPMethod = "exit"
	LSPMethodSetTrace                LSPMethod = "$/setTrace"
	LSPMethodTextDocumentDidOpen     LSPMethod = "textDocument/didOpen"
	LSPMethodTextDocumentDidChange   LSPMethod = "textDocument/didChange"
	LSPMethodTextDocumentDidClose    LSPMethod = "textDocument/didClose"
	LSPMethodTextDocumentCompletion  LSPMethod = "textDocument/completion"
	LSPMethodTextDocumentHover       LSPMethod = "textDocument/hover"
	LSPMethodTextDocumentDefinition  LSPMethod = "textDocument/definition"
	LSPMethodTextDocumentTypeDefinition LSPMethod = "textDocument/typeDefinition"
	LSPMethodTextDocumentImplementation LSPMethod = "textDocument/implementation"
	LSPMethodTextDocumentReferences  LSPMethod = "textDocument/references"
	LSPMethodTextDocumentHighlight   LSPMethod = "textDocument/documentHighlight"
	LSPMethodTextDocumentDocumentSymbol LSPMethod = "textDocument/documentSymbol"
	LSPMethodTextDocumentSignatureHelp  LSPMethod = "textDocument/signatureHelp"
	LSPMethodWorkspaceSymbol         LSPMethod = "workspace/symbol"
	LSPMethodWorkspaceExecuteCommand LSPMethod = "workspace/executeCommand"
	LSPMethodWorkspaceTextDocumentContent LSPMethod = "workspace/textDocumentContent"
	// 上游 eclipse.jdt.ls 支持但 jdtls-go 尚未重构的标准 LSP 方法
	LSPMethodTextDocumentFormatting      LSPMethod = "textDocument/formatting"
	LSPMethodTextDocumentRangeFormatting LSPMethod = "textDocument/rangeFormatting"
	LSPMethodTextDocumentOnTypeFormatting LSPMethod = "textDocument/onTypeFormatting"
	LSPMethodTextDocumentRename          LSPMethod = "textDocument/rename"
	LSPMethodTextDocumentCodeAction      LSPMethod = "textDocument/codeAction"
	LSPMethodTextDocumentCodeLens        LSPMethod = "textDocument/codeLens"
	LSPMethodTextDocumentFoldingRange    LSPMethod = "textDocument/foldingRange"
	LSPMethodTextDocumentSelectionRange  LSPMethod = "textDocument/selectionRange"
	LSPMethodTextDocumentSemanticTokens  LSPMethod = "textDocument/semanticTokens/full"
	LSPMethodTextDocumentInlayHint       LSPMethod = "textDocument/inlayHint"
	LSPMethodTextDocumentTypeHierarchy        LSPMethod = "textDocument/typeHierarchy"
	LSPMethodTextDocumentPrepareTypeHierarchy LSPMethod = "textDocument/prepareTypeHierarchy"
	LSPMethodTypeHierarchySupertypes          LSPMethod = "typeHierarchy/supertypes"
	LSPMethodTypeHierarchySubtypes            LSPMethod = "typeHierarchy/subtypes"
	LSPMethodTextDocumentCallHierarchy        LSPMethod = "textDocument/callHierarchy"
	LSPMethodTextDocumentPrepareCallHierarchy LSPMethod = "textDocument/prepareCallHierarchy"
	LSPMethodCallHierarchyIncomingCalls       LSPMethod = "callHierarchy/incomingCalls"
	LSPMethodCallHierarchyOutgoingCalls       LSPMethod = "callHierarchy/outgoingCalls"
	LSPNotificationLanguageStatus             LSPMethod = "language/status"
)

// JDTCommand 表示 eclipse.jdt.ls 专有命令的枚举类型
type JDTCommand string

const (
	JDTCommandEditOrganizeImports           JDTCommand = "java.edit.organizeImports"
	JDTCommandEditStringFormatting          JDTCommand = "java.edit.stringFormatting"
	JDTCommandEditHandlePasteEvent          JDTCommand = "java.edit.handlePasteEvent"
	JDTCommandProjectUpdateSourceAttachment JDTCommand = "java.project.updateSourceAttachment"
	JDTCommandProjectResolveSourceAttachment JDTCommand = "java.project.resolveSourceAttachment"
	JDTCommandProjectAddToSourcePath        JDTCommand = "java.project.addToSourcePath"
	JDTCommandProjectRemoveFromSourcePath   JDTCommand = "java.project.removeFromSourcePath"
	JDTCommandProjectListSourcePaths        JDTCommand = "java.project.listSourcePaths"
	JDTCommandProjectGetSettings            JDTCommand = "java.project.getSettings"
	JDTCommandProjectGetClasspaths          JDTCommand = "java.project.getClasspaths"
	JDTCommandProjectUpdateClassPaths       JDTCommand = "java.project.updateClassPaths"
	JDTCommandProjectUpdateSettings         JDTCommand = "java.project.updateSettings"
	JDTCommandProjectIsTestFile             JDTCommand = "java.project.isTestFile"
	JDTCommandProjectGetAll                 JDTCommand = "java.project.getAll"
	JDTCommandProjectRefreshDiagnostics     JDTCommand = "java.project.refreshDiagnostics"
	JDTCommandProjectImport                 JDTCommand = "java.project.import"
	JDTCommandProjectChangeImportedProjects  JDTCommand = "java.project.changeImportedProjects"
	JDTCommandProjectResolveStackTraceLocation JDTCommand = "java.project.resolveStackTraceLocation"
	JDTCommandNavigateOpenTypeHierarchy     JDTCommand = "java.navigate.openTypeHierarchy"
	JDTCommandNavigateResolveTypeHierarchy  JDTCommand = "java.navigate.resolveTypeHierarchy"
	JDTCommandProjectUpgradeGradle          JDTCommand = "java.project.upgradeGradle"
	JDTCommandProjectResolveWorkspaceSymbol JDTCommand = "java.project.resolveWorkspaceSymbol"
	JDTCommandProjectUpdateJdk              JDTCommand = "java.project.updateJdk"
	JDTCommandProtobufGenerateSources       JDTCommand = "java.protobuf.generateSources"
	JDTCommandProjectCreateModuleInfo       JDTCommand = "java.project.createModuleInfo"
	JDTCommandReloadBundles                 JDTCommand = "java.reloadBundles"
	JDTCommandCompletionOnDidSelect         JDTCommand = "java.completion.onDidSelect"
	JDTCommandDecompile                     JDTCommand = "java.decompile"
	JDTCommandEditSmartSemicolonDetection   JDTCommand = "java.edit.smartSemicolonDetection"
	JDTCommandVmGetAllInstalls              JDTCommand = "java.vm.getAllInstalls"
	JDTCommandGetTroubleshootingInfo        JDTCommand = "java.getTroubleshootingInfo"
	JDTCommandProjectResolveText            JDTCommand = "java.project.resolveText"
	JDTCommandGetFullyQualifiedName         JDTCommand = "java.getFullyQualifiedName"
	// jdtls-go 重写的专用命令
	JDTCommandGoResolveClasspath            JDTCommand = "jdtls-go.resolveClasspath"
	JDTCommandGoResolveEffectiveModel       JDTCommand = "jdtls-go.resolveEffectiveModel"
	JDTCommandGoResolveSources              JDTCommand = "jdtls-go.resolveSources"
	JDTCommandGoStartDebugSession           JDTCommand = "jdtls-go.startDebugSession"
	// VS Code Java 扩展生态命令别名
	JDTCommandVSCodeStartDebugSession       JDTCommand = "vscode.java.startDebugSession"
	JDTCommandVSCodeResolveClasspath        JDTCommand = "vscode.java.resolveClasspath"
)

// DAPCommand 表示 DAP 协议请求命令的枚举类型
type DAPCommand string

const (
	DAPCommandInitialize             DAPCommand = "initialize"
	DAPCommandLaunch                 DAPCommand = "launch"
	DAPCommandAttach                 DAPCommand = "attach"
	DAPCommandDisconnect             DAPCommand = "disconnect"
	DAPCommandTerminate              DAPCommand = "terminate"
	DAPCommandConfigurationDone      DAPCommand = "configurationDone"
	DAPCommandNext                   DAPCommand = "next"
	DAPCommandContinue               DAPCommand = "continue"
	DAPCommandStepIn                 DAPCommand = "stepIn"
	DAPCommandStepOut                DAPCommand = "stepOut"
	DAPCommandStepInTargets          DAPCommand = "stepInTargets"
	DAPCommandPause                  DAPCommand = "pause"
	DAPCommandStackTrace             DAPCommand = "stackTrace"
	DAPCommandRestartFrame           DAPCommand = "restartFrame"
	DAPCommandScopes                 DAPCommand = "scopes"
	DAPCommandVariables              DAPCommand = "variables"
	DAPCommandSetVariable            DAPCommand = "setVariable"
	DAPCommandSource                 DAPCommand = "source"
	DAPCommandLoadedSources          DAPCommand = "loadedSources"
	DAPCommandThreads                DAPCommand = "threads"
	DAPCommandSetBreakpoints         DAPCommand = "setBreakpoints"
	DAPCommandSetExceptionBreakpoints DAPCommand = "setExceptionBreakpoints"
	DAPCommandSetFunctionBreakpoints DAPCommand = "setFunctionBreakpoints"
	DAPCommandEvaluate               DAPCommand = "evaluate"
	DAPCommandCompletions            DAPCommand = "completions"
	DAPCommandRunInTerminal          DAPCommand = "runInTerminal"
	DAPCommandRedefineClasses        DAPCommand = "redefineClasses"
	DAPCommandExceptionInfo          DAPCommand = "exceptionInfo"
	DAPCommandDataBreakpointInfo     DAPCommand = "dataBreakpointInfo"
	DAPCommandSetDataBreakpoints     DAPCommand = "setDataBreakpoints"
	DAPCommandContinueAll            DAPCommand = "continueAll"
	DAPCommandContinueOthers         DAPCommand = "continueOthers"
	DAPCommandPauseAll               DAPCommand = "pauseAll"
	DAPCommandPauseOthers            DAPCommand = "pauseOthers"
	DAPCommandInlineValues           DAPCommand = "inlineValues"
	DAPCommandRefreshVariables       DAPCommand = "refreshVariables"
	DAPCommandProcessId              DAPCommand = "processId"
	DAPCommandBreakpointLocations    DAPCommand = "breakpointLocations"
	DAPCommandRefreshFrames          DAPCommand = "refreshFrames"
)

// DocumentHighlightKind 表示文档高亮语义类型的枚举
type DocumentHighlightKind int

const (
	DocumentHighlightKindText  DocumentHighlightKind = 1
	DocumentHighlightKindRead  DocumentHighlightKind = 2
	DocumentHighlightKindWrite DocumentHighlightKind = 3
)

// CodeLensKind 表示代码镜头指令类型的枚举
type CodeLensKind string

const (
	CodeLensKindRun   CodeLensKind = "java.dap.run"
	CodeLensKindDebug CodeLensKind = "java.dap.debug"
	CodeLensKindRef   CodeLensKind = "editor.action.findReferences"
)
