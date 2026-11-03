// Command orglane is the Orglane core binary. It runs in one of three modes:
//
//	orglane server         run the server (API, jobs, migrations on start);
//	                       --demo loads the demo dataset (demo mode)
//	orglane agent          run the local agent (not implemented yet)
//	orglane cli <command>  administrative commands (migrate, setup-token,
//	                       oidc-client add, version)
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
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/config"
	"github.com/orglane/orglane/core/internal/database"
	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/core/internal/logging"
	"github.com/orglane/orglane/core/internal/server"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `Usage: orglane <mode> [flags]

Modes:
  server             Run the Orglane server
  agent              Run the local agent (not implemented yet)
  cli migrate        Apply database migrations, seed built-in roles and exit
  cli setup-token    Print a new one-time setup token for creating the first
                     owner (only while no owner exists)
  cli oidc-client add
                     Register an OIDC client and print its secret:
                       -id <client id> -name <name> -redirect-uri <uri>...
                       [-post-logout-redirect-uri <uri>]... [-public]
                       [-native] [-refresh-tokens] [-auth-method
                       client_secret_basic|client_secret_post]
  cli version        Print the version

Flags for server and the cli commands that use the database:
  -config <path>     YAML config file (default: $ORGLANE_CONFIG)

Flags for server:
  --demo             Demo mode: on an empty database, load the "Acme
                     Robotics" demo dataset instead of first-run setup and
                     allow one-click sign-in as any demo person (also
                     demo.enabled / ORGLANE_DEMO_ENABLED). Refuses a
                     database that holds other data.

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
	return loadConfigWith(flag.NewFlagSet(name, flag.ContinueOnError), args, stderr, lookupEnv)
}

// loadConfigWith is loadConfig with a flag set that may define more flags.
func loadConfigWith(fs *flag.FlagSet, args []string, stderr io.Writer, lookupEnv func(string) (string, bool)) (*config.Config, error) {
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
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	demoMode := fs.Bool("demo", false, "run in demo mode with the Acme demo dataset (needs an empty database)")
	cfg, err := loadConfigWith(fs, args, stderr, lookupEnv)
	if err != nil {
		return err
	}
	if *demoMode {
		cfg.Demo.Enabled = true
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
	case "setup-token":
		return runSetupToken(ctx, rest, stdout, stderr, lookupEnv)
	case "oidc-client":
		if len(rest) == 0 || rest[0] != "add" {
			return fmt.Errorf("%w: usage: orglane cli oidc-client add -id <id> -name <name> -redirect-uri <uri>", errUsage)
		}
		return runOIDCClientAdd(ctx, rest[1:], stdout, stderr, lookupEnv)
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
	if err := identity.NewService(pool).SeedBuiltinRoles(ctx); err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, "migrations are up to date")
	return err
}

// openMigrated connects, applies pending migrations and seeds the built-in
// roles, so administrative commands work on a fresh database too.
func openMigrated(ctx context.Context, cfg *config.Config, stderr io.Writer) (*pgxpool.Pool, error) {
	logger := logging.New(stderr, cfg.Log.Format, cfg.Log.Level)
	pool, err := database.Connect(ctx, cfg.Database, logger)
	if err != nil {
		return nil, err
	}
	migrator, err := database.NewMigrator(pool, logger)
	if err != nil {
		pool.Close()
		return nil, err
	}
	err = errors.Join(migrator.Up(ctx), migrator.Close())
	if err == nil {
		err = identity.NewService(pool).SeedBuiltinRoles(ctx)
	}
	if err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

func runSetupToken(ctx context.Context, args []string, stdout, stderr io.Writer, lookupEnv func(string) (string, bool)) error {
	cfg, err := loadConfig("setup-token", args, stderr, lookupEnv)
	if err != nil {
		return err
	}
	pool, err := openMigrated(ctx, cfg, stderr)
	if err != nil {
		return err
	}
	defer pool.Close()
	token, err := identity.NewService(pool).ResetSetupToken(ctx, audit.Actor{Kind: audit.ActorSystem, UserAgent: "orglane-cli"})
	if errors.Is(err, identity.ErrSetupDone) {
		return errors.New("setup is already complete: an owner exists")
	} else if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "Setup token (valid until setup completes, replaces any earlier token):\n\n  %s\n\nOpen %s and create the owner account with it.\n", token, cfg.WorkspaceURL())
	return err
}

// stringsFlag collects a repeatable string flag.
type stringsFlag []string

func (f *stringsFlag) String() string     { return strings.Join(*f, ",") }
func (f *stringsFlag) Set(v string) error { *f = append(*f, v); return nil }

func runOIDCClientAdd(ctx context.Context, args []string, stdout, stderr io.Writer, lookupEnv func(string) (string, bool)) error {
	fs := flag.NewFlagSet("oidc-client add", flag.ContinueOnError)
	var in identity.NewOIDCClient
	var redirects, postLogout stringsFlag
	fs.StringVar(&in.ID, "id", "", "client ID")
	fs.StringVar(&in.Name, "name", "", "display name")
	fs.Var(&redirects, "redirect-uri", "allowed redirect URI (repeatable)")
	fs.Var(&postLogout, "post-logout-redirect-uri", "allowed post-logout redirect URI (repeatable)")
	fs.BoolVar(&in.Public, "public", false, "public client without a secret (PKCE with S256 required)")
	fs.BoolVar(&in.Native, "native", false, "native app (with -public)")
	fs.BoolVar(&in.RefreshTokens, "refresh-tokens", false, "allow refresh tokens (offline_access)")
	fs.StringVar(&in.AuthMethod, "auth-method", "", "client_secret_basic (default) or client_secret_post")
	cfg, err := loadConfigWith(fs, args, stderr, lookupEnv)
	if err != nil {
		return err
	}
	in.RedirectURIs, in.PostLogoutRedirectURIs = redirects, postLogout
	pool, err := openMigrated(ctx, cfg, stderr)
	if err != nil {
		return err
	}
	defer pool.Close()
	client, secret, err := identity.NewService(pool).CreateOIDCClient(ctx, audit.Actor{Kind: audit.ActorSystem, UserAgent: "orglane-cli"}, in)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "client_id: %s\nissuer: %s\n", client.ID, cfg.WorkspaceURL()); err != nil {
		return err
	}
	if secret != "" {
		_, err = fmt.Fprintf(stdout, "client_secret: %s\n(shown once: store it now)\n", secret)
	}
	return err
}

func printVersion(w io.Writer) error {
	_, err := fmt.Fprintf(w, "orglane %s\n", version)
	return err
}
