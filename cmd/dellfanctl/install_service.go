package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

const systemdUnitTemplate = `[Unit]
Description=Dell iDRAC fan control (dellfanctl)
After=network.target

[Service]
Type=simple
ExecStart=%s run --config %s
Restart=on-failure
RestartSec=5
# SIGTERM triggers a graceful shutdown that hands fan control back to the
# iDRAC before exiting; give it a little room to do so.
TimeoutStopSec=15

# systemd creates/owns this directory (root:root, mode 0750) and expands
# %%L to its parent (/var/log); the service's own stdout/stderr (all of
# dellfanctl's logging - see log/slog in cmd/dellfanctl) is appended there
# instead of only going to the journal.
LogsDirectory=%s
StandardOutput=append:%%L/%s/%s
StandardError=append:%%L/%s/%s

[Install]
WantedBy=multi-user.target
`

// cmdInstallService writes a systemd unit file for 'dellfanctl run'. It
// does not enable or start the service itself; the operator does that
// explicitly once they've reviewed both the unit and their config.
func cmdInstallService(args []string) error {
	fs := flag.NewFlagSet("install-service", flag.ExitOnError)
	unitPath := fs.String("unit-path", "/etc/systemd/system/dellfanctl.service", "where to write the systemd unit file")
	configPath := fs.String("config", defaultConfigPath, "config path the service should use")
	binPath := fs.String("bin", "", "path to the dellfanctl binary (default: resolve current executable)")
	logFile := fs.String("log-file", "dellfanctl.log", "log file name, created under systemd's LogsDirectory (/var/log/dellfanctl/)")
	force := fs.Bool("force", false, "overwrite an existing unit file")
	fs.Parse(args)

	bin := *binPath
	if bin == "" {
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("resolving dellfanctl executable path (pass --bin explicitly): %w", err)
		}
		abs, err := filepath.Abs(exe)
		if err != nil {
			return err
		}
		bin = abs
	}

	if !*force {
		if _, err := os.Stat(*unitPath); err == nil {
			return fmt.Errorf("%s already exists (pass --force to overwrite)", *unitPath)
		}
	}

	const logsDirName = "dellfanctl"
	logDir := "/var/log/" + logsDirName
	// LogsDirectory= in the unit asks systemd to create this before each
	// start, which is the "correct" systemd-native way and normally
	// sufficient on its own. Belt-and-suspenders: create it here too,
	// since that directive has been observed to not reliably create the
	// directory in at least one real deployment (symptom: the service
	// fails immediately with "Failed to set up standard output: No such
	// file or directory" / exit code 209/STDOUT) - and an install step
	// that leaves the service unable to start is worse than a redundant
	// mkdir.
	if err := os.MkdirAll(logDir, 0o750); err != nil {
		return fmt.Errorf("creating log directory %s: %w", logDir, err)
	}

	unit := fmt.Sprintf(systemdUnitTemplate, bin, *configPath, logsDirName, logsDirName, *logFile, logsDirName, *logFile)
	if err := os.WriteFile(*unitPath, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("writing unit file (are you root?): %w", err)
	}

	logPath := logDir + "/" + *logFile
	fmt.Printf("Wrote %s\n\n", *unitPath)
	fmt.Println("Next steps:")
	fmt.Println("  sudo systemctl daemon-reload")
	fmt.Println("  sudo systemctl enable --now dellfanctl")
	fmt.Printf("\nLogs: %s\n", logPath)
	fmt.Println("(StandardOutput=append: sends the app's own logs only to that file, not the journal;")
	fmt.Println(" journalctl -u dellfanctl still shows systemd's own start/stop/failure messages.)")
	fmt.Println("Consider a logrotate policy for that file - see deploy/dellfanctl.logrotate.")
	return nil
}
