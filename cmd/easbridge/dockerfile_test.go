package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"easbridge/internal/version"
)

// Everything the build pulls is pinned by digest: a rebuild cannot silently pick
// up other bytes than the ones that were reviewed (security review, finding 12):
// the base images and the BuildKit frontend named by the syntax directive.
func TestDockerfileBaseImagesArePinnedByDigest(t *testing.T) {
	b, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	first, _, _ := strings.Cut(string(b), "\n")
	if !regexp.MustCompile(`^# syntax=\S+@sha256:[0-9a-f]{64}$`).MatchString(first) {
		t.Errorf("the syntax directive (BuildKit frontend) is not pinned by digest: %q", first)
	}
	pinned := regexp.MustCompile(`^FROM (--platform=\S+ )?\S+@sha256:[0-9a-f]{64}( AS \S+)?$`)
	stages := map[string]bool{}
	froms := 0
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "FROM ") {
			continue
		}
		froms++
		fields := strings.Fields(line)[1:]
		if len(fields) > 0 && strings.HasPrefix(fields[0], "--platform=") {
			fields = fields[1:]
		}
		if stages[fields[0]] { // FROM an earlier stage
			continue
		}
		if !pinned.MatchString(line) {
			t.Errorf("not pinned by digest: %q", line)
		}
		if len(fields) >= 3 {
			stages[fields[2]] = true
		}
	}
	if froms < 2 {
		t.Errorf("expected a builder and a runtime stage, found %d FROM lines", froms)
	}
}

// The version in the build files and in the binary agree.
func TestVersionIsTheSameEverywhere(t *testing.T) {
	for _, f := range []struct{ file, want string }{
		{"../../Makefile", "VERSION ?= " + version.Version},
		{"../../Dockerfile", "ARG VERSION=" + version.Version},
	} {
		b, err := os.ReadFile(f.file)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), f.want) {
			t.Errorf("%s does not contain %q", f.file, f.want)
		}
	}
	cl, err := os.ReadFile("../../CHANGELOG.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cl), "## ["+version.Version+"]") {
		t.Errorf("CHANGELOG.md has no entry for %s", version.Version)
	}
}

// scripts/govulncheck.sh is the one command that checks the dependencies for known
// vulnerabilities. It must stay pinned (a floating tool would make the result
// depend on the day), use the project-local toolchain and keep its caches inside
// the project.
func TestGovulncheckScriptIsPinnedAndProjectLocal(t *testing.T) {
	st, err := os.Stat("../../scripts/govulncheck.sh")
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&0o111 == 0 {
		t.Error("scripts/govulncheck.sh is not executable")
	}
	b, err := os.ReadFile("../../scripts/govulncheck.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	if !regexp.MustCompile(`(?m)^GOVULNCHECK_VERSION=v\d+\.\d+\.\d+$`).MatchString(text) {
		t.Error("the govulncheck version is not pinned to a release")
	}
	for _, want := range []string{`source "$here/env.sh"`, `XDG_CACHE_HOME="$here/`, "golang.org/x/vuln/cmd/govulncheck@${GOVULNCHECK_VERSION}"} {
		if !strings.Contains(text, want) {
			t.Errorf("scripts/govulncheck.sh does not contain %q", want)
		}
	}
	if strings.Contains(text, "@latest") {
		t.Error("scripts/govulncheck.sh must not use @latest")
	}
}

// /data exists in the image owned by the nonroot user with mode 0700 and is declared a volume
// afterwards, so that a named volume gets the right owner and mode without a chown (0.3).
func TestTheDataDirectoryIsOwnedByTheRuntimeUserAndDeclaredAVolume(t *testing.T) {
	b, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, want := range []string{
		"mkdir -p /out/data /out/ca && chmod 0700 /out/data",
		"COPY --from=build --chown=65532:65532 /out/data /data",
		"USER 65532:65532",
		`VOLUME ["/data"]`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the Dockerfile lacks %q", want)
		}
	}
	// The volume is declared after the directory has its owner and mode (Docker copies the
	// image's content and ownership into a new named volume at first use).
	copyAt, volAt := strings.Index(text, "COPY --from=build --chown=65532:65532 /out/data /data"), strings.Index(text, `VOLUME ["/data"]`)
	if copyAt < 0 || volAt < copyAt {
		t.Error("VOLUME /data must come after the COPY that gives /data its owner")
	}
	if !strings.Contains(text, "EASBRIDGE_DATA_DIR=/data") {
		t.Error("the data directory of the service is not /data")
	}
}

// `docker exec easbridge /easbridge ca` is the documented way to print the CA: the binary is at /easbridge.
func TestTheBinaryIsAtASlashEasbridgeForDockerExec(t *testing.T) {
	b, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"COPY --from=build /out/easbridge /easbridge", `ENTRYPOINT ["/easbridge"]`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the Dockerfile lacks %q", want)
		}
	}
}

// With the TLS variables set in the image the automatic certificate could never be used (a variable set
// means "provided"); /ca is made for the exported CA, owned by the runtime user like /data.
func TestTheImageSetsNoTLSVariablesAndHasACADirectory(t *testing.T) {
	b, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, line := range strings.Split(text, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "#") {
			continue
		}
		if strings.Contains(l, "EASBRIDGE_TLS_") {
			t.Errorf("the image sets a TLS variable: %q", l)
		}
	}
	for _, want := range []string{"mkdir -p /out/data /out/ca", "COPY --from=build --chown=65532:65532 /out/ca /ca", "ARG IMAGE=", "ARG GOTAGS=", "internal/version.Image="} {
		if !strings.Contains(text, want) {
			t.Errorf("the Dockerfile lacks %q", want)
		}
	}
}

// The build stage runs natively and cross-compiles (an arm64 image must not build under emulation), and no
// port is EXPOSEd: nothing should publish the listeners.
func TestTheBuildStageCrossCompilesAndNothingIsExposed(t *testing.T) {
	b, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, want := range []string{"FROM --platform=$BUILDPLATFORM golang:", "ARG TARGETARCH", "GOARCH=$TARGETARCH"} {
		if !strings.Contains(text, want) {
			t.Errorf("the Dockerfile lacks %q", want)
		}
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "EXPOSE") {
			t.Errorf("the Dockerfile exposes ports: %q", line)
		}
	}
	// The runtime stage is the target platform's, not the builder's.
	if strings.Count(text, "--platform=") != 1 {
		t.Error("only the build stage may be pinned to the build platform")
	}
}

// Every action is pinned by commit SHA with its version in a comment, every workflow starts with read-only permissions, and the
// release workflow checks the tag against the code and the branch and runs the e2e run with Docker required.
func TestWorkflowsArePinnedAndTheReleaseChecksTheTag(t *testing.T) {
	uses := regexp.MustCompile(`^\s*-?\s*uses:\s*\S+@[0-9a-f]{40}\s+# v\d+(\.\d+)*\s*$`)
	files, _ := filepath.Glob("../../.github/workflows/*.yml")
	if len(files) < 2 {
		t.Fatalf("workflows: %v", files)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		text := string(b)
		if !regexp.MustCompile(`(?m)^permissions:\n  contents: read\n`).MatchString(text) {
			t.Errorf("%s: no top-level read-only permissions", f)
		}
		for _, line := range strings.Split(text, "\n") {
			if strings.Contains(line, "uses:") && !uses.MatchString(line) {
				t.Errorf("%s: an action not pinned by SHA with a version comment: %q", f, strings.TrimSpace(line))
			}
		}
	}
	b, _ := os.ReadFile("../../.github/workflows/release.yml")
	rel := string(b)
	for _, want := range []string{"internal/version/version.go", "## [${version}]", "merge-base --is-ancestor", "make vulncheck", "make compat", "make e2e",
		"EASBRIDGE_E2E_REQUIRED", "linux/amd64,linux/arm64", "sbom: true", "provenance: mode=max", "type=semver,pattern={{major}}.{{minor}}"} {
		if !strings.Contains(rel, want) {
			t.Errorf("release.yml lacks %q", want)
		}
	}
	if n := strings.Count(rel, "packages: write"); n != 1 {
		t.Errorf("packages: write appears %d times, want once (the image job only)", n)
	}
	if _, err := os.Stat("../../RELEASING.md"); err != nil {
		t.Error("RELEASING.md is missing")
	}
}
