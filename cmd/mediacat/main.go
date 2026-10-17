// Command mediacat catalogs media files and manages duplicates.
package main

import (
	"os"

	"github.com/njhoffman/mediacat/internal/cli"
)

var version = "dev"

func main() {
	os.Exit(cli.Execute(version))
}
