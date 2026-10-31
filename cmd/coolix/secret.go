package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"coolix/internal/engine"
)

func runSecret(e *engine.Engine, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: coolix secret set|unset|list APP [KEY]")
	}
	app := args[1]
	switch args[0] {
	case "list":
		infos, err := e.Secrets(app)
		if err != nil {
			return err
		}
		for _, s := range infos {
			if s.Pending {
				fmt.Println(s.Name + " (changed, not deployed)")
			} else {
				fmt.Println(s.Name)
			}
		}
		return nil
	case "set":
		if len(args) != 3 {
			return errors.New("usage: coolix secret set APP KEY < value")
		}
		value, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		if err := e.SetSecret(app, args[2], strings.TrimSuffix(string(value), "\n")); err != nil {
			return err
		}
	case "unset":
		if len(args) != 3 {
			return errors.New("usage: coolix secret unset APP KEY")
		}
		if err := e.UnsetSecret(app, args[2]); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown secret command %q", args[0])
	}
	logf("%s %s for %s; run `coolix deploy %s` to apply", args[0], args[2], app, app)
	return nil
}
