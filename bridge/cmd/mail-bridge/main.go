// Command mail-bridge is the stateless bridge between mail programs (IMAP,
// SMTP submission) and the HTTP edge in Convex. Configuration comes from the
// environment only; see README.md.
package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/bridge"
	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/config"
	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/edge"
)

func main() {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		// The message names variables, never values.
		fmt.Fprintln(os.Stderr, "mail-bridge: configuration:", err)
		os.Exit(2)
	}
	if edge.PlainHTTPAnyHost {
		fmt.Fprintln(os.Stderr, "mail-bridge: built with -tags localedge: plain http to the edge is allowed for any host. For local probes only, never for operation.")
	}
	b, err := bridge.Start(cfg, bridge.Options{LogWriter: os.Stdout})
	if err != nil {
		fmt.Fprintln(os.Stderr, "mail-bridge: cannot listen:", err)
		os.Exit(1)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
	b.Shutdown()
}
