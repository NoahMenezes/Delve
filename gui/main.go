package main

import (
	"embed"
	"fmt"
	"os"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"

	"github.com/NoahMenezes/Delve/gui/backend"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	app := backend.New()

	// The bound App's exported methods become async JS promises at
	// window.go.main.App.*. Only backend is bound: the frontend can
	// reach the engine solely through these thin wrappers, never the
	// database or filesystem directly.
	err := wails.Run(&options.App{
		Title:  "Delve",
		Width:  1200,
		Height: 800,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		Bind: []interface{}{
			app,
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "delve-gui failed:", err)
		os.Exit(1)
	}
}
