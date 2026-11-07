# jdtls-go

`jdtls-go` 是用 Go 编写的 Java 语言服务器和调试适配器，供支持 LSP/DAP 的编辑器使用。

- `jdtls-go`：提供 Java 代码分析和 Maven classpath 解析。
- `java-debug-go`：启动或连接 Java 程序，支持断点、单步、调用栈和变量查看。

项目仍在开发中，功能尚未完整替代 Eclipse JDT LS 或 Microsoft Java Debug。

## 构建

需要 Go 1.27。编译结果写入 `bin/`：

```sh
make build      # macOS 和 Windows 两个平台
make build-arm   # macOS Apple Silicon
make build-win   # Windows x86_64，需要 MinGW-w64
```

Maven 项目需要 Maven 3.9 和 JDK。构建 Windows 版本时，默认交叉编译器是
`x86_64-w64-mingw32-gcc`，也可以通过 `WINDOWS_CC` 指定其他路径。

## 在 OpenIDEA 中配置

在 Java 项目的根目录创建 `.openidea/settings.toml`。将下面的 `/your/path/...`
替换为你机器上的实际路径。示例用于 macOS Apple Silicon：

```toml
[languageServers.jdtls]
command = "/your/path/jdtls-go/bin/jdtls-go-darwin-arm64"
languages = ["java"]
enabled = true

[languageServers.jdtls.env]
JAVA_DEBUG_GO = "/your/path/jdtls-go/bin/java-debug-go-darwin-arm64"
MAVEN_HOME = "/your/path/apache-maven-3.9"
MAVEN_SETTINGS = "/your/path/settings.xml"
MAVEN_USER_PROPERTIES = "Dmaven.repo.local=/your/path/maven-repository"

[languages.java]
jdk = "/your/path/jdk-21"

[languages.java.maven]
userSettings = "/your/path/settings.xml"
```

Windows 请将两个程序路径改为对应的 `.exe` 文件，例如
`jdtls-go-windows-amd64.exe` 和 `java-debug-go-windows-amd64.exe`。

`JAVA_DEBUG_GO` 指向独立的调试程序。**不要**把它填进 OpenIDEA 的
`bundles`：`bundles` 用于 Eclipse/OSGi 插件 JAR，本项目的调试器不是 JAR。

## 运行和调试

在 OpenIDEA 中打开 Maven 项目，选择 Java 主类运行或调试。设置断点后，调试器会通过 Maven classpath 启动程序；也可以在 controller 中单步进入 service。

Maven 私有仓库和镜像配置放在 `settings.xml` 中。`MAVEN_SETTINGS` 和
`userSettings` 应指向同一个文件；`MAVEN_USER_PROPERTIES` 可指定本地依赖目录。

## 当前范围

项目支持基础 Java 分析、Maven classpath 解析和常用源码调试功能。完整编译器语义、重构、表达式求值以及 Eclipse/Microsoft 调试器的全部功能仍未实现。
