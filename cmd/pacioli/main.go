// Command pacioli prints the version it was built from.
package main

import (
	"fmt"
	"io"
	"os"
)

// version is set at link time by the Makefile.
var version = "dev"

func main() {
	run(os.Stdout)
}

// run writes the version to w, so the one thing this binary does is testable.
func run(w io.Writer) {
	fmt.Fprintln(w, version)
}
