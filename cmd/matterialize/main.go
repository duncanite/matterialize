// The matterialize daemon makes the devices of a home that do not speak
// Matter into Matter devices, behind one bridge. Run it with the path of
// its configuration.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/mycophonic/primordium/app"

	"github.com/duncanite/matterialize/internal/config"
	"github.com/duncanite/matterialize/internal/daemon"
	"github.com/duncanite/matterialize/plugin"
	"github.com/duncanite/matterialize/plugin/lutron"
	"github.com/duncanite/matterialize/plugin/wemo"
)

// errUsage is the daemon asked wrongly.
var errUsage = errors.New("usage: matterialize CONFIG")

// makers is the plugins this build has, by the name the configuration
// enables each under.
func makers() map[string]plugin.Maker {
	return map[string]plugin.Maker{"lutron": lutron.New, "wemo": wemo.New}
}

func main() {
	app.Main(&app.Options{Name: "matterialize"}, func(ctx context.Context) error {
		if len(os.Args) != 2 {
			return errUsage
		}

		cfg, err := config.Load(os.Args[1])
		if err != nil {
			return fmt.Errorf("%s: %w", os.Args[1], err)
		}

		return daemon.Run(ctx, cfg, makers(), os.Stdout)
	})
}
