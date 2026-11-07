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
diagnostics, source type/member indexing, basic member completion, hover,
definition, references, implementation/type-definition lookup, document and
workspace symbols, signature help, Maven effective-model discovery, source
attachment lookup, and Spring configuration metadata reading. Semantic
resolution remains partial: overload selection, generic substitution, lexical
scope rules, access control, full compiler diagnostics, and refactoring behavior
are not implemented. Java syntax parsing uses
the Tree-sitter Java grammar through Go bindings; the C grammar/runtime are
compiled into the Go binaries, with no Java parser or server process. Maven support invokes Maven 3.9 (project
`mvnw`, `MAVEN_HOME`, then `PATH`); `MAVEN_SETTINGS` selects a settings file.

The DAP implementation now has a multiplexed JDWP reader for asynchronous VM
events, source and function breakpoints, field data breakpoints, class-prepare
handling, step over/into/out, thread and source stack frames, local-variable
scopes, object/array expansion, exception breakpoints, and a limited expression
evaluator for locals, primitive operators, fields, and arrays. It starts a target
JVM with JDWP or attaches to an existing JDWP port. Java method invocation during
evaluation, complete watchpoint semantics, run-in-terminal, hot code replace,
and remaining upstream DAP features are still incomplete; this is not yet
feature parity with Microsoft Java Debug.

The LSP custom command `jdtls-go.startDebugSession` starts the sibling
`java-debug-go` executable and returns its loopback `{host, port}` endpoint. Set
`JAVA_DEBUG_GO` if the executable is not beside `jdtls-go` and is not on `PATH`.
The editor must connect to that endpoint using DAP and send `launch` or
`attach` requests.

OpenIDEA compatibility includes the JDT LS `java/buildWorkspace` request and
the `java.project.getClasspaths` / `vscode.java.startDebugSession` commands.
The latter returns the numeric DAP port that OpenIDEA expects. Do not put the
`java-debug-go` executable in OpenIDEA's JDT LS `bundles` list: `bundles` is for
OSGi plugin JARs, while this Go debugger is started as a separate DAP process.
Keep both Go executables beside each other or set `JAVA_DEBUG_GO` in the
language-server process environment.

## Build

Use Go 1.27. From this directory, `make build` creates all four binaries in
`bin/`. Build only the macOS arm64 or Windows amd64 pair with `make build-arm`
or `make build-win`:

```sh
make build
make build-arm
make build-win
```

The targets are macOS arm64 and Windows amd64. Tree-sitter uses Cgo, so the
Windows cross-build also needs an x86_64 MinGW-w64 C compiler. The Makefile
defaults to `x86_64-w64-mingw32-gcc`; override it with `WINDOWS_CC` if your
compiler has a different path. The aliases `make build-darwin-arm64` and
`make build-windows-amd64` remain available.

To override the Go toolchain, set `GO`:

```sh
make build GO=/path/to/go
```

The Makefile prefers `/Users/Cage/bin/bin/go` when present and checks that the
compiler is Go 1.27. The platform-specific binaries have suffixes so both
targets can be kept in `bin/` at the same time. Start the macOS language server
with `bin/jdtls-go-darwin-arm64` over stdio. Start its standalone debug adapter
with `bin/java-debug-go-darwin-arm64 --listen 127.0.0.1:0`; it prints one JSON
line with the selected endpoint. Keep the matching LSP and DAP executables
together or set `JAVA_DEBUG_GO`.

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
example, with `mvn -f jtps/pom.xml compile`). Maven settings and project
profiles can be selected with `MAVEN_SETTINGS`, `MAVEN_PROFILES`,
`MAVEN_OFFLINE=true`, and `MAVEN_USER_PROPERTIES` (space-separated `key=value`
entries). The LSP command `jdtls-go.resolveEffectiveModel` returns the
interpolated module model; `jdtls-go.resolveSources` resolves dependency sources
and enables navigation into attached source JARs. Its `attachments` result maps
binary JAR paths to source JAR paths; pass that map as `sourceAttachments` in a
DAP `launch` request to enable dependency source lookup in stack frames.

See [docs/protocol-parity.md](docs/protocol-parity.md) for the current local
upstream LSP/DAP coverage matrix. The listed partial and unsupported features
are material gaps; a successful build is not feature parity.
