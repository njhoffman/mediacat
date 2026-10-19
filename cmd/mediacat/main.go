// Command mediacat catalogs media files and manages duplicates.
package main

import (
	"os"
	"runtime/debug"

	"github.com/njhoffman/mediacat/internal/cli"
)

// version is set at build time from git tags (see the justfile's build recipe).
var version = "dev"

func main() {
	os.Exit(cli.Execute(resolveVersion()))
}

// resolveVersion falls back to the module version Go stamps from VCS tags when the binary
// was built without the justfile (plain go build or go install).
func resolveVersion() string {
	if version != "dev" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return version
}
