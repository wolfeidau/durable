// Command gen writes the config JSON Schema to the committed file.
package main

import (
	"fmt"
	"os"

	"github.com/wolfeidau/durable/internal/config"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: gen <output file>")
		os.Exit(2)
	}
	b, err := config.SchemaJSON()
	if err == nil {
		err = os.WriteFile(os.Args[1], b, 0o644) //nolint:gosec // a published, non-secret file
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
