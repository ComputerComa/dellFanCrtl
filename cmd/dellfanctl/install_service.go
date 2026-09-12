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

	unit := fmt.Sprintf(systemdUnitTemplate, bin, *configPath)
	if err := os.WriteFile(*unitPath, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("writing unit file (are you root?): %w", err)
	}

	fmt.Printf("Wrote %s\n\n", *unitPath)
	fmt.Println("Next steps:")
	fmt.Println("  sudo systemctl daemon-reload")
	fmt.Println("  sudo systemctl enable --now dellfanctl")
	fmt.Println("\nCheck logs with: journalctl -u dellfanctl -f")
	return nil
}
