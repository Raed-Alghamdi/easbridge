package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"easbridge/internal/config"
	"easbridge/internal/install"
	"easbridge/internal/version"
)

const initUsage = `usage: easbridge init [flags]

Writes docker-compose.easbridge.yml and easbridge.env next to MailFlow's compose file, and prints the
steps that follow. Run it from the folder that holds MailFlow's docker-compose.yml:

  docker run --rm -it --user "$(id -u):$(id -g)" -v "$PWD:/out" %s init

The --user flag matters: the image runs as uid 65532, which cannot write to your folder; with your
own uid:gid the files are yours. Nothing secret is written.

flags:
`

// stdinIsTerminal reports whether standard input is a terminal (a character device).
func stdinIsTerminal() bool {
	st, err := os.Stdin.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// runInit is `easbridge init`. tty says that in is a person at a terminal: only then are missing
// values asked for.
func runInit(args []string, in io.Reader, out, errOut io.Writer, tty bool) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(errOut)
	var easURL, users, outDir, image, backend, network string
	var force bool
	fs.StringVar(&easURL, "eas-url", "", "the ActiveSync URL of your Exchange server (https), for example https://mail.example.com/Microsoft-Server-ActiveSync")
	fs.StringVar(&users, "users", "", "the logins that may use the bridge: comma-separated addresses, or @example.com for a whole domain")
	fs.StringVar(&outDir, "out", "/out", "the folder to write to (the one that holds MailFlow's compose file)")
	fs.StringVar(&image, "image", version.ImageRef(), "the EASBridge image the compose file uses")
	fs.BoolVar(&force, "force", false, "overwrite "+install.OverrideName+" and "+install.EnvName+" if they exist")
	fs.StringVar(&backend, "backend-service", "backend", "the name of MailFlow's backend service in its compose file")
	fs.StringVar(&network, "network", "mailflow", "the name of the network MailFlow's services share")
	fs.Usage = func() {
		fmt.Fprintf(errOut, initUsage, version.ImageRef())
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("init takes flags only, got %q", fs.Args())
	}

	// Missing values: ask at a terminal, name the flags otherwise.
	var missing []string
	if strings.TrimSpace(easURL) == "" {
		missing = append(missing, "--eas-url")
	}
	if strings.TrimSpace(users) == "" {
		missing = append(missing, "--users")
	}
	if len(missing) > 0 && !tty {
		return fmt.Errorf("%s needed: init is not running in a terminal, so it cannot ask (use docker run -it, or give the flags)", strings.Join(missing, " and "))
	}
	rd := bufio.NewReader(in)
	ask := func(prompt string, valid func(string) error) (string, error) {
		for tries := 0; tries < 3; tries++ {
			fmt.Fprint(out, prompt)
			line, err := rd.ReadString('\n')
			line = strings.TrimSpace(line)
			if line == "" && err != nil {
				return "", fmt.Errorf("no answer (%v)", err)
			}
			if verr := valid(line); verr != nil {
				fmt.Fprintln(out, "  "+verr.Error())
				continue
			}
			return line, nil
		}
		return "", errors.New("three answers that did not fit; run init again")
	}
	var err error
	if strings.TrimSpace(easURL) == "" {
		if easURL, err = ask("ActiveSync URL of your Exchange server\n  (for example https://mail.example.com/Microsoft-Server-ActiveSync): ", checkURL); err != nil {
			return err
		}
	}
	if strings.TrimSpace(users) == "" {
		if users, err = ask("Logins that may use EASBridge: addresses separated by commas, or @example.com for a whole domain\n  (a domain entry lets anyone with a login in that domain try a password through the bridge): ", checkUsers); err != nil {
			return err
		}
	}

	easURL = strings.TrimSpace(easURL)
	if err := checkURL(easURL); err != nil {
		return fmt.Errorf("--eas-url: %w", err)
	}
	list, uerr := config.ParseAllowedUsers(users)
	if len(uerr) > 0 {
		return fmt.Errorf("--users: %w", errors.Join(uerr...))
	}
	if len(list) == 0 {
		return errors.New("--users: no login given")
	}
	p := install.Params{EASURL: easURL, Users: list, Image: strings.TrimSpace(image), BackendService: strings.TrimSpace(backend), Network: strings.TrimSpace(network)}
	if err := install.CheckNames(p); err != nil {
		return err
	}
	if err := install.CheckValues(p); err != nil {
		return err
	}

	st, err := os.Stat(outDir)
	if err != nil || !st.IsDir() {
		return fmt.Errorf("--out %s is not a folder; mount yours there: -v \"$PWD:/out\"", outDir)
	}
	var warnings []string
	if u, _ := url.Parse(easURL); u != nil && !strings.EqualFold(strings.TrimRight(u.Path, "/"), "/Microsoft-Server-ActiveSync") {
		warnings = append(warnings, fmt.Sprintf("the path of the URL is %q; Exchange's ActiveSync endpoint is normally /Microsoft-Server-ActiveSync", u.Path))
	}
	for _, u := range list {
		if !strings.HasPrefix(u, "@") && !strings.Contains(u, "@") && !strings.Contains(u, `\`) {
			warnings = append(warnings, fmt.Sprintf("the login %q is not an address; it will be used as it is, check it is what Exchange expects", u))
		}
	}
	composeFile, note := install.FindCompose(outDir)
	switch {
	case composeFile == "":
		warnings = append(warnings, "no docker-compose.yml or docker-compose.ghcr.yml in "+outDir+": run init from the folder that holds MailFlow's compose file (docker run ... -v \"$PWD:/out\"); the commands below name docker-compose.yml")
	default:
		text, rerr := os.ReadFile(filepath.Join(outDir, composeFile))
		if rerr != nil {
			warnings = append(warnings, fmt.Sprintf("cannot read %s to check it: %v", composeFile, rerr))
		} else {
			warnings = append(warnings, install.CheckCompose(string(text), p.BackendService, p.Network)...)
		}
		if note != "" {
			warnings = append(warnings, note)
		}
	}

	override, envf := filepath.Join(outDir, install.OverrideName), filepath.Join(outDir, install.EnvName)
	if !force {
		for _, f := range []string{override, envf} {
			if _, err := os.Lstat(f); err == nil {
				return fmt.Errorf("%s already exists; nothing was written (use --force to replace both files)", f)
			}
		}
	}
	if err := os.WriteFile(envf, []byte(install.EnvFile(p)), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w (did you run it with --user \"$(id -u):$(id -g)\"?)", envf, err)
	}
	if err := os.WriteFile(override, []byte(install.Override(p)), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", override, err)
	}

	fmt.Fprintf(out, "\nWrote %s and %s (no secrets in either).\n", install.OverrideName, install.EnvName)
	for _, w := range warnings {
		fmt.Fprintf(out, "WARNING: %s\n", w)
	}
	fmt.Fprintln(out)
	fmt.Fprint(out, install.NextSteps(p, composeFile))
	return nil
}

func checkURL(s string) error {
	if err := config.ValidateEASURL(strings.TrimSpace(s)); err != nil {
		return fmt.Errorf("the ActiveSync URL %v", err)
	}
	return nil
}

func checkUsers(s string) error {
	list, errs := config.ParseAllowedUsers(s)
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	if len(list) == 0 {
		return errors.New("give at least one login")
	}
	return nil
}
