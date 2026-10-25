// Command keyguard manages short-lived Meta Model API keys through a Chrome extension.
package main

import (
	"io/fs"
	"os"

	metakeyguard "github.com/omenking/meta-keyguard"
	"github.com/omenking/meta-keyguard/internal/keyguard"
)

func main() {
	extension, err := fs.Sub(metakeyguard.Extension, "extension")
	if err != nil {
		panic(err)
	}
	os.Exit(keyguard.Main(os.Args[1:], extension))
}
