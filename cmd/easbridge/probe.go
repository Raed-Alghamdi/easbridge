package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"golang.org/x/term"

	"easbridge/internal/config"
	"easbridge/internal/easupstream"
	"easbridge/internal/store"
)

// probeOptions is what `easbridge probe` was asked to do.
type probeOptions struct {
	url, user, version, dataDir, caFile string
	pingSeconds                         int
	passwordStdin                       bool
	// trace: print the decoded request and response of every call, with all content
	// redacted (easupstream.ProbeConfig.Trace).
	trace bool
	// skipContacts leaves the contacts step and the directory step out.
	skipContacts bool
	// device is the device the probe presents. It starts from the same environment
	// variables, defaults and checks as the service (config.LoadDevice), so a probe
	// that works says something about the service; a flag may override one value.
	device config.Device
}

// parseProbe reads the flags of `easbridge probe`. getenv supplies the defaults
// (the same EASBRIDGE_* variables the service reads).
func parseProbe(args []string, getenv func(string) string, stderr io.Writer) (*probeOptions, error) {
	dev, err := config.LoadDevice(getenv)
	if err != nil {
		return nil, fmt.Errorf("device settings:\n%w", err)
	}
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	o := &probeOptions{}
	fs.StringVar(&o.url, "url", getenv(config.EnvEASURL), "Exchange ActiveSync URL (https)")
	fs.StringVar(&o.user, "user", firstUser(getenv(config.EnvAllowedUsers)), "login name")
	fs.StringVar(&o.version, "version", envOr(getenv, config.EnvEASVersion, "16.1"), "preferred protocol version (16.1, falls back to 14.1)")
	fs.StringVar(&o.dataDir, "data-dir", envOr(getenv, config.EnvDataDir, "/data"), "data directory shared with the service (for the DeviceId)")
	fs.IntVar(&o.pingSeconds, "ping-seconds", 60, "length of the final Ping (0 skips it)")
	fs.StringVar(&o.caFile, "ca-file", getenv(config.EnvEASCAFile), "extra CA certificates (PEM) to trust")
	fs.StringVar(&dev.Type, "device-type", dev.Type, "DeviceType")
	fs.StringVar(&dev.Model, "device-model", dev.Model, "device model in the device information (honest: never another product)")
	fs.StringVar(&dev.FriendlyName, "device-friendly-name", dev.FriendlyName, "friendly name in the device information")
	fs.StringVar(&dev.OS, "device-os", dev.OS, "operating system in the device information")
	fs.StringVar(&dev.OSLanguage, "device-os-language", dev.OSLanguage, "operating system language in the device information")
	fs.StringVar(&dev.UserAgent, "user-agent", dev.UserAgent, "HTTP User-Agent (also sent in the device information)")
	fs.BoolVar(&o.skipContacts, "skip-contacts", false, "leave out the contacts step and the directory (GAL) step after the mail steps")
	fs.BoolVar(&o.passwordStdin, "password-stdin", false, "read the password from one line of standard input")
	fs.BoolVar(&o.trace, "trace", false, "print the decoded request and response of every call (message bodies, subjects, addresses, folder names and credentials are not shown)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if o.url == "" || o.user == "" {
		return nil, errors.New("--url and --user are required (or set EASBRIDGE_EAS_URL and EASBRIDGE_ALLOWED_USERS)")
	}
	if err := dev.Validate(); err != nil {
		return nil, fmt.Errorf("device settings:\n%w", err)
	}
	o.device = dev
	return o, nil
}

// runProbe is the first look at a real Exchange server. The password comes from
// a no-echo prompt on the terminal or, with --password-stdin, from one line of
// standard input. The report holds only statuses, counts, sizes and timings.
func runProbe(args []string, stdin *os.File, stdout, stderr io.Writer) error {
	o, err := parseProbe(args, os.Getenv, stderr)
	if err != nil {
		return err
	}

	pw, err := readPassword(o.passwordStdin, stdin, stderr, o.user)
	if err != nil {
		return err
	}
	if pw == "" {
		return errors.New("empty password")
	}

	db, err := store.Open(o.dataDir)
	if err != nil {
		return fmt.Errorf("data directory %s: %w", o.dataDir, err)
	}
	devID, err := db.DeviceID()
	db.Close()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return easupstream.Probe(ctx, easupstream.ProbeConfig{
		Config: easConfig(o.url, o.version, o.caFile, o.device),
		User:   strings.ToLower(strings.TrimSpace(o.user)), Password: pw, DeviceID: devID, PingSeconds: o.pingSeconds, Out: stdout,
		Trace: o.trace, SkipContacts: o.skipContacts,
	})
}

// easConfig is the part of the Exchange configuration that serve and probe share:
// the endpoint and the device the bridge presents, so that both describe the same one.
func easConfig(url, version, caFile string, dev config.Device) easupstream.Config {
	return easupstream.Config{
		URL: url, Version: version, CAFile: caFile,
		DeviceType: dev.Type, DeviceModel: dev.Model, DeviceFriendlyName: dev.FriendlyName,
		DeviceOS: dev.OS, DeviceOSLanguage: dev.OSLanguage, UserAgent: dev.UserAgent,
	}
}

func readPassword(fromStdin bool, stdin *os.File, stderr io.Writer, user string) (string, error) {
	if fromStdin {
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && line == "" {
			return "", fmt.Errorf("reading the password from standard input: %w", err)
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	fd := int(stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", errors.New("standard input is not a terminal: use --password-stdin")
	}
	fmt.Fprintf(stderr, "Password for %s (not shown): ", user)
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(stderr)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func envOr(getenv func(string) string, name, def string) string {
	if v := strings.TrimSpace(getenv(name)); v != "" {
		return v
	}
	return def
}

func firstUser(list string) string {
	for _, u := range strings.Split(list, ",") {
		if u = strings.TrimSpace(u); u != "" && !strings.HasPrefix(u, "@") { // a domain entry names no login
			return u
		}
	}
	return ""
}
