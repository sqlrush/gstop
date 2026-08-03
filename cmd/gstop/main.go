// Command gstop is the entry point: it parses flags, loads configuration,
// enforces the concurrent-user limit, optionally prompts for a password, and
// launches either the interactive TUI or the headless daemon loop. Port of the
// __main__ block in tool/gstop.py.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/term"

	"gstop/internal/alarm"
	"gstop/internal/app"
	"gstop/internal/config"
	"gstop/internal/dbconn"
	"gstop/internal/health"
	"gstop/internal/logging"
	"gstop/internal/monitor"
	"gstop/internal/oscmd"
	"gstop/internal/tui"
)

const version = "v1.6.3"

func main() {
	if versionRequested(os.Args[1:]) {
		fmt.Printf("gstop %s\n", version)
		return
	}
	args, configPath := parseFlags()

	cfg, err := config.Load(configPath, args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load config:", err)
		os.Exit(1)
	}
	if err := validateDaemonInvocation(cfg, args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// The terminate safety switches (main.support_terminate and
	// main.support_emergency_command) are honoured from the configuration file
	// and default to off. The original gstop.py force-disabled them here
	// regardless of the file; that lockdown is deliberately lifted so operators
	// can enable session termination through configuration alone.

	logger := logging.New("gstop", "gstop_app.log")
	runner := oscmd.New(logger, commandSlowThreshold(cfg), collectTimeout(cfg))

	if !withinUserLimit(cfg, runner) {
		fmt.Println("The user limit has been reached, exit.")
		return
	}

	if pw, ok := readPasswordIfNeeded(cfg); ok {
		cfg = cfg.With("main.db_password", pw)
	}

	if err := run(cfg, logger, runner, configPath); err != nil {
		logger.Error("gstop exited with error: %v", err)
		fmt.Fprintln(os.Stderr, "gstop:", err)
		os.Exit(1)
	}
}

func versionRequested(args []string) bool {
	seenVersion := false
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "version" || args[i] == "-version" || args[i] == "--version":
			if seenVersion {
				return false
			}
			seenVersion = true
		case args[i] == "-c":
			if i+1 >= len(args) || args[i+1] == "" {
				return false
			}
			i++
		case strings.HasPrefix(args[i], "-c="):
			if strings.TrimPrefix(args[i], "-c=") == "" {
				return false
			}
		default:
			return false
		}
	}
	return seenVersion
}

// run wires the dependencies and starts the appropriate loop.
func run(cfg *config.Config, logger *logging.Logger, runner *oscmd.Runner, configPath string) error {
	al := alarm.New(cfg)
	if err := al.Start(); err != nil {
		return fmt.Errorf("start alarm: %w", err)
	}
	defer al.Stop()

	deps := monitor.Deps{
		Cfg:       cfg,
		DB:        dbconn.New(cfg, logger),
		OS:        runner,
		Logger:    logger,
		Alarm:     al,
		Health:    health.New(cfg),
		ConfigDir: filepath.Dir(configPath),
	}
	defer deps.DB.Close()

	if cfg.GetBool("main.daemon", false) {
		return app.New(deps, nil).RunDaemon()
	}

	screen, err := tui.NewScreen()
	if err != nil {
		return fmt.Errorf("init terminal: %w", err)
	}
	defer screen.Close()
	return app.New(deps, screen).Run()
}

// parseFlags mirrors the argparse options, returning only the flags the user set
// (so unset flags fall back to the config file) and the config file path.
func parseFlags() (config.Args, string) {
	interval := flag.Int("i", 0, "refresh interval (seconds)")
	logInterval := flag.Int("l", 0, "log refresh interval (seconds)")
	user := flag.String("u", "", "production environment db user")
	port := flag.Int("p", 0, "production environment db port")
	var daemon bool
	flag.BoolVar(&daemon, "d", false, "run gstop in daemon mode")
	flag.BoolVar(&daemon, "daemon", false, "run gstop in daemon mode")
	configPath := flag.String("c", "", "path to gstop.cfg")
	flag.Parse()

	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })

	var args config.Args
	if set["i"] {
		args.Interval = interval
	}
	if set["l"] {
		args.LogInterval = logInterval
	}
	if set["u"] {
		args.User = user
	}
	if set["p"] {
		args.Port = port
	}
	if set["d"] || set["daemon"] {
		args.Daemon = &daemon
	}
	return args, resolveConfigPath(*configPath)
}

func validateDaemonInvocation(cfg *config.Config, args config.Args) error {
	if !cfg.GetBool("main.daemon", false) {
		return nil
	}
	if args.Daemon == nil || !*args.Daemon {
		return fmt.Errorf("daemon mode must be enabled explicitly with -d or --daemon")
	}
	return nil
}

// resolveConfigPath honours an explicit -c, otherwise prefers ./gstop.cfg and
// falls back to ./configs/gstop.cfg for the development layout.
func resolveConfigPath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if _, err := os.Stat("gstop.cfg"); err == nil {
		return "gstop.cfg"
	}
	return filepath.Join("configs", "gstop.cfg")
}

// withinUserLimit enforces the concurrent-instance guard. The current process
// is present in ps output, so daemon mode allows at most one daemon and
// interactive mode allows up to main.max_concurrent_users.
func withinUserLimit(cfg *config.Config, runner *oscmd.Runner) bool {
	out, _ := runner.Run("ps -ww -axo pid=,stat=,ucomm=,args=", false)
	pids := map[string]struct{}{}
	daemonOnly := cfg.GetBool("main.daemon", false)
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || strings.Contains(fields[1], "Z") {
			continue
		}
		if fields[2] != "gstop" {
			continue
		}
		argv := fields[3:]
		if daemonOnly && !hasDaemonArgument(argv[1:]) {
			continue
		}
		pids[fields[0]] = struct{}{}
	}
	if len(pids) == 0 {
		return true
	}
	if daemonOnly {
		return len(pids) <= 1
	}
	return len(pids) <= cfg.GetInt("main.max_concurrent_users", 3)
}

func hasDaemonArgument(args []string) bool {
	for _, arg := range args {
		switch arg {
		case "-d", "--d", "-daemon", "--daemon",
			"-d=true", "--d=true", "-daemon=true", "--daemon=true":
			return true
		}
	}
	return false
}

// readPasswordIfNeeded prompts for a database password when password-free login
// is disabled, matching the getpass branch. It returns ok=false when no password
// is needed.
func readPasswordIfNeeded(cfg *config.Config) (string, bool) {
	if cfg.GetBool("main.password_free", true) {
		return "", false
	}
	// A password supplied in config is used as-is (no prompt), which keeps
	// non-interactive/daemon launches from blocking on stdin.
	if cfg.GetString("main.db_password", "") != "" {
		return "", false
	}
	fmt.Print("Please input database user password: ")
	pw, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return "", false
	}
	fmt.Println("Input password successfully.")
	return string(pw), true
}

func commandSlowThreshold(cfg *config.Config) time.Duration {
	return time.Duration(cfg.GetFloat("main.sql_command_time_thresh", 3) * float64(time.Second))
}

func collectTimeout(cfg *config.Config) time.Duration {
	return config.CollectTimeout(cfg)
}
