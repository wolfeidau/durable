// Command restore copies a session database to dst under a new epoch, for the browser spike.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/wolfeidau/durable"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: restore <src> <dst>")
		os.Exit(2)
	}
	if err := durable.Restore(context.Background(), os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
