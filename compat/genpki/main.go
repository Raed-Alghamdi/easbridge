// Command genpki writes a throwaway CA and server certificate (DNS: easbridge,
// localhost; IP: 127.0.0.1) for the compatibility harness:
//
//	go run ./compat/genpki DIR   ->  DIR/ca.pem  DIR/server.pem  DIR/server-key.pem
//
// Development tool only; it is not part of the production binary.
package main

import (
	"fmt"
	"os"

	"easbridge/internal/testpki"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: genpki DIR")
		os.Exit(2)
	}
	p, err := testpki.New()
	if err != nil {
		fmt.Fprintln(os.Stderr, "genpki:", err)
		os.Exit(1)
	}
	if _, _, _, err := p.WriteFiles(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "genpki:", err)
		os.Exit(1)
	}
}
