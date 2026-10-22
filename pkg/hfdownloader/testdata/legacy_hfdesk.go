// This fixture models the pre-rebuild HFDesk flag.Parse -> browser/server path.
// It intentionally does not know the wrapper's protocol probe flag.
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
)

func main() {
	port := flag.Int("port", 8080, "HTTP port")
	openUI := flag.Bool("open", true, "open the web UI")
	noOpen := flag.Bool("no-open", false, "do not open the web UI")
	flag.Parse()
	if *openUI && !*noOpen {
		_ = os.WriteFile(os.Getenv("LEGACY_BROWSER_SENTINEL"), []byte("opened"), 0o600)
	}
	if value := os.Getenv("LEGACY_PORT"); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			*port = parsed
		}
	}
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		return
	}
	defer listener.Close()
	_ = os.WriteFile(os.Getenv("LEGACY_SERVER_SENTINEL"), []byte("started"), 0o600)
}
