// Command easbridge lets IMAP/SMTP clients use an Exchange mailbox that only
// allows Exchange ActiveSync.
//
//	easbridge [serve]      run the bridge (configuration from EASBRIDGE_* variables)
//	easbridge probe        check a real Exchange server (reads the password without echo)
//	easbridge healthcheck  exit 0 if a running bridge reports healthy (for Docker)
//	easbridge init         write the compose override and env file for a MailFlow install
//	easbridge ca           print the CA certificate of the automatic TLS mode (PEM)
//	easbridge version
package main

import (
	"fmt"
	"os"

	"easbridge/internal/version"
)

func main() {
	cmd := "serve"
	args := os.Args[1:]
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = runServe(args)
	case "probe":
		err = runProbe(args, os.Stdin, os.Stdout, os.Stderr)
	case "healthcheck":
		err = runHealthcheck(args)
	case "init":
		err = runInit(args, os.Stdin, os.Stdout, os.Stderr, stdinIsTerminal())
	case "ca":
		err = runCA(args, os.Stdout)
	case "version", "--version", "-version":
		fmt.Println("easbridge", version.Version)
	case "help", "-h", "--help":
		fmt.Fprint(os.Stdout, usage)
	default:
		fmt.Fprintf(os.Stderr, "easbridge: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "easbridge:", err)
		os.Exit(1)
	}
}

const usage = `usage: easbridge [command]

  serve        run the IMAP/SMTP bridge (default); configuration comes from EASBRIDGE_* variables
  probe        first check against a real Exchange server; flags: --url --user --version
               --data-dir --ping-seconds --ca-file --password-stdin --device-type
               --device-model --device-friendly-name --device-os --device-os-language
               --user-agent --trace (the device flags default to the EASBRIDGE_DEVICE_* variables;
               --trace prints every request and response as redacted XML)
  init         write docker-compose.easbridge.yml and easbridge.env next to MailFlow's compose file
               (docker run --rm -it --user "$(id -u):$(id -g)" -v "$PWD:/out" <image> init)
  ca           print the CA certificate (PEM) of the automatic TLS certificate, for MailFlow to trust:
               docker exec easbridge /easbridge ca
  healthcheck  query the health endpoint of a running bridge; exit status 0 means healthy;
               -v also prints the report: docker exec easbridge /easbridge healthcheck -v
  version      print the version
`
