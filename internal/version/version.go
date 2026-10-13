// Package version holds the build version, overridable with
// -ldflags "-X easbridge/internal/version.Version=...".
package version

// Version is the EASBridge release.
var Version = "0.3.0"

// UserAgent returns the default HTTP User-Agent for EAS requests.
func UserAgent() string { return "easbridge/" + Version }

// Image is where the published container image lives (without the tag). It is the one place the
// project names it: `easbridge init` writes it into the compose file it makes, and a build can
// change it with -ldflags "-X easbridge/internal/version.Image=..." (the Dockerfile's IMAGE
// argument does), or a run with `easbridge init --image`.
var Image = "ghcr.io/raed-alghamdi/easbridge"

// ImageRef is the image of this build: Image with the tag of its version.
func ImageRef() string { return Image + ":" + Version }
