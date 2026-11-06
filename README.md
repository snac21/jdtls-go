# jdtls-go

`jdtls-go` is one Go project with two programs:

- `cmd/jdtls-go`: Java language server using LSP over stdio.
- `cmd/java-debug-go`: Debug Adapter Protocol server using DAP over loopback TCP and a Go JDWP client.

Both programs are implemented in Go 1.27. The debugger does not load a Java helper,
the Microsoft `java-debug` JAR, Eclipse/OSGi bundles, or Gradle. The Java
application being debugged still runs on a JDK; that is the target runtime,
not an implementation dependency of these tools.

## Current implementation status

The project is an early implementation, not yet feature-equivalent to
Eclipse JDT LS or Microsoft's Java Debug Server. The current LSP slice includes
stdio framing, document synchronization, recoverable Java parsing and syntax
diagnostics, document symbols, workspace declaration completion, simple
hover/definition lookup, Maven reactor discovery, Maven dependency classpath
resolution, and Spring configuration metadata reading. Java syntax parsing uses
the Tree-sitter Java grammar through Go bindings; the C grammar/runtime are
compiled into the Go binaries, with no Java parser or server process. Maven support invokes Maven 3.9 (project
`mvnw`, `MAVEN_HOME`, then `PATH`); `MAVEN_SETTINGS` selects a settings file.

The DAP implementation now has a multiplexed JDWP reader for asynchronous VM
events, source-file and line-table breakpoint resolution, class-prepare handling,
step over/into/out, thread and source stack frames, local-variable scopes,
object/array expansion, exception breakpoints, and a limited expression evaluator
for locals, primitive operators, fields, and arrays. It starts a target JVM with
JDWP or attaches to an existing JDWP port. Conditional/log breakpoints,
inheritance-aware object inspection, Java method invocation during evaluation,
full Java expression semantics, and remaining upstream DAP features are still
incomplete; this is not yet feature parity with Microsoft Java Debug.

The LSP custom command `jdtls-go.startDebugSession` starts the sibling
`java-debug-go` executable and returns its loopback `{host, port}` endpoint. Set
`JAVA_DEBUG_GO` if the executable is not beside `jdtls-go` and is not on `PATH`.
The editor must connect to that endpoint using DAP and send `launch` or
`attach` requests.

## Build

Use a Go 1.27 toolchain. From this directory:

```sh
go build -o bin/jdtls-go ./cmd/jdtls-go
go build -o bin/java-debug-go ./cmd/java-debug-go
```

Start the language server with `bin/jdtls-go` over stdio. Start the standalone
debug adapter with `bin/java-debug-go --listen 127.0.0.1:0`; it prints one JSON
line with the selected endpoint. Keep both executables together or set
`JAVA_DEBUG_GO`.

## Maven classpath example

The language server exposes `jdtls-go.resolveClasspath` through
`workspace/executeCommand`. It resolves compile scope by default and test scope
when the second command argument is `true`. The result includes `target/classes`
and Maven dependency entries, suitable for constructing a debug launch classpath.

The DAP `launch` request can also resolve a Maven runtime classpath itself. Pass
`projectRoot`, optionally a `moduleDir` (absolute or relative to the reactor
root), and the Java `mainClass`; omit `classPaths` to ask Maven 3.9 for the
module's runtime dependencies. When `sourcePaths` is omitted, the adapter takes
the module's main and test source roots from its POM. This also works for a
Spring Boot application whose `mainClass` starts the application; the server
does not need a Spring-specific launcher.

Example DAP `launch` arguments for the JTps module:

```json
{
  "projectRoot": "/Users/Cage/snac21/java-server/jtps",
  "moduleDir": "jtps",
  "mainClass": "com.example.jtps.JtpsApplication"
}
```

If the reactor root is itself the application module, set `moduleDir` to `.`.
The target JVM still needs a JDK and the module must have been compiled (for
example, with `mvn -f jtps/pom.xml compile`).
