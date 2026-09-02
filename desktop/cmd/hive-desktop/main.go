// Command hive-desktop is the hive-sandbox desktop client's entrypoint.
//
// This file is deliberately thin: it embeds the frontend, builds one session,
// mounts the /api surface over the asset server, and opens a window. Every
// behavior worth testing lives in internal/session and ui, which compile and
// run headlessly.
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/wailsapp/wails/v3/pkg/application"

	webui "github.com/bees-roadhouse/hive-sandbox/desktop"
	"github.com/bees-roadhouse/hive-sandbox/desktop/internal/keyring"
	"github.com/bees-roadhouse/hive-sandbox/desktop/internal/session"
	"github.com/bees-roadhouse/hive-sandbox/desktop/ui"
)

// version is overridden at build time with -ldflags "-X main.version=...",
// same contract as the daemon.
var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version) //nolint:forbidigo // --version prints to stdout by design
		os.Exit(0)
	}

	sess := session.New(keyring.OS{})
	api := ui.New(sess)

	app := application.New(application.Options{
		Name:        "hive",
		Description: "hive-sandbox desktop client",
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(webui.FS()),
			Middleware: func(next http.Handler) http.Handler {
				// The API rides the asset server's chain so no second listener
				// exists to secure; anything not under /api/ falls through to
				// the embedded frontend.
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasPrefix(r.URL.Path, "/api/") {
						api.ServeHTTP(w, r)
						return
					}
					next.ServeHTTP(w, r)
				})
			},
		},
	})

	app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:   "main",
		Title:  "hive",
		Width:  980,
		Height: 720,
		URL:    "/",
	})

	if err := app.Run(); err != nil {
		panic("desktop: " + err.Error())
	}
}
