// Command squared is the Squared management platform. One binary has all
// roles:
//
//	squared serve           web interface, API and outbox relay
//	squared worker          task consumer and Argo CD status watcher
//	squared guard           protects the groups that give rights in services
//	squared forgejo-config  serves the sign-in settings of Forgejo to the guard
//	squared migrate         apply database migrations
//	squared check-catalog   check package definitions
//	squared version         show the version
//
// All configuration comes from environment variables. See
// docs/configuration.md.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"squared/internal/catalog"
	"squared/internal/config"
	"squared/internal/render"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

const usage = `usage: squared <command>

Commands:
  serve           Run the web interface, the API and the outbox relay.
  worker          Run the task worker and the Argo CD status watcher.
  guard           Protect the Keycloak groups that the services give rights to.
  forgejo-config  Serve the sign-in settings of Forgejo (a sidecar of Forgejo).
  migrate         Apply the database migrations, then stop.
  check-catalog   Check the package definitions in a directory, then stop.
  version         Show the version.`

func run(args []string) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var env config.Reader
	log := newLogger(env.String("SQUARED_LOG_LEVEL", "info"), env.String("SQUARED_LOG_FORMAT", "json"))
	log = log.With("version", fullVersion())

	switch args[0] {
	case "serve":
		return serve(ctx, log)
	case "worker":
		return runWorker(ctx, log)
	case "guard":
		return runGuard(ctx, log)
	case "forgejo-config":
		return runForgejoConfig(ctx, log)
	case "migrate":
		return migrate(ctx, log)
	case "check-catalog":
		dir := env.String("SQUARED_CATALOG_DIR", "catalog")
		if len(args) > 1 {
			dir = args[1]
		}
		c, err := catalog.Load(os.DirFS(dir), render.Check)
		if err != nil {
			return err
		}
		for _, p := range c.Latest() {
			for _, v := range c.Versions(p.Name) {
				fmt.Println("ok", v.Ref())
			}
		}
		return nil
	case "version":
		fmt.Println(fullVersion())
		return nil
	case "help", "-h", "--help":
		fmt.Println(usage)
		return nil
	}
	return fmt.Errorf("unknown command %q\n\n%s", args[0], usage)
}

func newLogger(level, format string) *slog.Logger {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: l}
	if format == "text" {
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, opts))
}

// fullVersion returns the version and the Git commit of the build.
func fullVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return version
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" && len(s.Value) >= 12 {
			return version + "+" + s.Value[:12]
		}
	}
	return version
}
