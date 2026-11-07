package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"

	"github.com/cage/jdtls-go/internal/lsp"
)

func main() {
	fs := flag.NewFlagSet("jdtls-go", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // 避免向 stdout 打印错误破坏 LSP 协议

	var dataDir, configDir, listenAddr string
	var port int
	fs.StringVar(&dataDir, "data", "", "Eclipse workspace data directory (compatibility)")
	fs.StringVar(&configDir, "configuration", "", "Eclipse configuration directory (compatibility)")
	fs.StringVar(&listenAddr, "listen", "", "TCP listen address for LSP")
	fs.IntVar(&port, "port", 0, "Socket port for LSP")

	// 忽略未知参数，确保兼容各种包装器脚本
	_ = fs.Parse(os.Args[1:])

	logger := log.New(os.Stderr, "jdtls-go: ", log.LstdFlags)

	if port > 0 || listenAddr != "" {
		addr := listenAddr
		if addr == "" {
			addr = fmt.Sprintf("127.0.0.1:%d", port)
		}
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			logger.Fatalf("failed to listen on %s: %v", addr, err)
		}
		defer listener.Close()
		logger.Printf("listening on %s", listener.Addr().String())
		for {
			conn, err := listener.Accept()
			if err != nil {
				logger.Printf("accept error: %v", err)
				continue
			}
			go func(c net.Conn) {
				defer c.Close()
				if err := lsp.Serve(c, c, logger); err != nil {
					logger.Printf("session ended: %v", err)
				}
			}(conn)
		}
	} else {
		// 默认 Stdio 模式
		if err := lsp.Serve(os.Stdin, os.Stdout, logger); err != nil {
			logger.Printf("server stopped: %v", err)
			os.Exit(1)
		}
	}
}
