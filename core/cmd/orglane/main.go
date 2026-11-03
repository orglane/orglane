// Command orglane is the Orglane core binary. It runs in one of three modes:
//
//	orglane server         run the server (API, jobs, migrations on start)
//	orglane agent          run the local agent (not implemented yet)
//	orglane cli <command>  administrative commands (migrate, version)
//
// Configuration comes from a YAML file (-config or ORGLANE_CONFIG) and
// ORGLANE_* environment variables; see core/internal/config.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/orglane/orglane/core/internal/config"
	"github.com/orglane/orglane/core/internal/database"
	"github.com/orglane/orglane/core/internal/logging"
	"github.com/orglane/orglane/core/internal/server"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `Usage: orglane <mode> [flags]

Modes:
  server             Run the Orglane server
  agent              Run the local agent (not implemented yet)
  cli migrate        Apply database migrations and exit
  cli version        Print the version

Flags for server and cli migrate:
  -config <path>     YAML config file (default: $ORGLANE_CONFIG)

Every setting can also be set with an ORGLANE_* environment variable,
for example ORGLANE_DATABASE_URL or ORGLANE_DOMAIN_BASE.
`

// errUsage marks errors caused by invalid command-line usage (exit code 2).
var errUsage = errors.New("usage")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, os.LookupEnv)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, lookupEnv func(string) (string, bool)) int {
	err := dispatch(ctx, args, stdout, stderr, lookupEnv)
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errUsage):
		_, _ = fmt.Fprintf(stderr, "orglane: %v\n\n%s", err, usage)
		return 2
	default:
		_, _ = fmt.Fprintf(stderr, "orglane: %v\n", err)
		return 1
	}
}

func dispatch(ctx context.Context, args []string, stdout, stderr io.Writer, lookupEnv func(string) (string, bool)) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: missing mode", errUsage)
	}
	switch mode, rest := args[0], args[1:]; mode {
	case "server":
		return runServer(ctx, rest, stderr, lookupEnv)
	case "agent":
		return runAgent(stderr)
	case "cli":
		return runCLI(ctx, rest, stdout, stderr, lookupEnv)
	case "-h", "-help", "--help", "help":
		_, err := io.WriteString(stdout, usage)
		return err
	case "-version", "--version":
		return printVersion(stdout)
	default:
		return fmt.Errorf("%w: unknown mode %q", errUsage, mode)
	}
}

// loadConfig parses the -config flag and loads the configuration.
func loadConfig(name string, args []string, stderr io.Writer, lookupEnv func(string) (string, bool)) (*config.Config, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	defaultPath, _ := lookupEnv(config.EnvConfigFile)
	path := fs.String("config", defaultPath, "YAML config file")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %w", errUsage, err)
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("%w: unexpected arguments %v", errUsage, fs.Args())
	}
	return config.Load(*path, lookupEnv)
}

func runServer(ctx context.Context, args []string, stderr io.Writer, lookupEnv func(string) (string, bool)) error {
	cfg, err := loadConfig("server", args, stderr, lookupEnv)
	if err != nil {
		return err
	}
	logger := logging.New(stderr, cfg.Log.Format, cfg.Log.Level)
	slog.SetDefault(logger)
	return server.Run(ctx, cfg, version, logger)
}

func runAgent(stderr io.Writer) error {
	logging.New(stderr, "text", "info").Info("agent mode is not implemented yet", "version", version)
	return nil
}

func runCLI(ctx context.Context, args []string, stdout, stderr io.Writer, lookupEnv func(string) (string, bool)) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: missing cli command", errUsage)
	}
	switch cmd, rest := args[0], args[1:]; cmd {
	case "version":
		return printVersion(stdout)
	case "migrate":
		return runMigrate(ctx, rest, stdout, stderr, lookupEnv)
	default:
		return fmt.Errorf("%w: unknown cli command %q", errUsage, cmd)
	}
}

func runMigrate(ctx context.Context, args []string, stdout, stderr io.Writer, lookupEnv func(string) (string, bool)) (err error) {
	cfg, err := loadConfig("migrate", args, stderr, lookupEnv)
	if err != nil {
		return err
	}
	logger := logging.New(stderr, cfg.Log.Format, cfg.Log.Level)
	pool, err := database.Connect(ctx, cfg.Database, logger)
	if err != nil {
		return err
	}
	defer pool.Close()
	migrator, err := database.NewMigrator(pool, logger)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, migrator.Close()) }()
	if err := migrator.Up(ctx); err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, "migrations are up to date")
	return err
}

func printVersion(w io.Writer) error {
	_, err := fmt.Fprintf(w, "orglane %s\n", version)
	return err
}
