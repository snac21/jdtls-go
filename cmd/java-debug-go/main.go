package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"

	"github.com/cage/jdtls-go/internal/dap"
)

func main() {
	address := flag.String("listen", "127.0.0.1:0", "DAP TCP listen address")
	flag.Parse()
	listener, err := net.Listen("tcp", *address)
	if err != nil {
		log.Fatal(err)
	}
	defer listener.Close()
	bound := listener.Addr().(*net.TCPAddr)
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"host": "127.0.0.1", "port": bound.Port})
	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			if err := dap.Serve(c, c, log.New(os.Stderr, "java-debug-go: ", log.LstdFlags)); err != nil {
				fmt.Fprintf(os.Stderr, "java-debug-go: DAP session ended: %v\n", err)
			}
		}(conn)
	}
}
