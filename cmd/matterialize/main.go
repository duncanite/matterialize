// The matterialize daemon makes the devices of a home that do not speak
// Matter into Matter devices, behind one bridge. Run it with the path of
// its configuration; with "keep", it keeps a secret for one of its
// plugins and ends.
package main

import (
	"context"
	"os"

	"github.com/mycophonic/primordium/app"

	"github.com/duncanite/matterialize/internal/command"
	"github.com/duncanite/matterialize/plugin"
	"github.com/duncanite/matterialize/plugin/dyson"
	"github.com/duncanite/matterialize/plugin/lutron"
	"github.com/duncanite/matterialize/plugin/wemo"
)

// makers is the plugins this build has, by the name the configuration
// enables each under.
func makers() map[string]plugin.Maker {
	return map[string]plugin.Maker{"dyson": dyson.New, "lutron": lutron.New, "wemo": wemo.New}
}

func main() {
	app.Main(&app.Options{Name: "matterialize"}, func(ctx context.Context) error {
		return command.Run(ctx, os.Args[1:], makers(), os.Stdin, os.Stdout)
	})
}
