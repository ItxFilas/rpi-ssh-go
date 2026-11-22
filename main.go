package main

import (
	"fmt"
	"os"

	"rpi-ssh-go/pkg/connect"
	"rpi-ssh-go/pkg/mcp"
	"rpi-ssh-go/pkg/rtc"
)

func main() {
	os.Exit(run())
}

// run holds everything with deferred cleanup, so open sessions are closed
// before the process exits, also when serving ends with an error.
func run() int {
	client, err := connect.NewClient()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Initialization error: %v\n", err)
		return 1
	}

	pool := rtc.NewPool(client)
	defer pool.Close()
	server := mcp.NewServer()

	mcp.RegisterRPiTools(server, client, pool)

	if err := server.Serve(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "Server exited with error: %v\n", err)
		return 1
	}
	return 0
}
