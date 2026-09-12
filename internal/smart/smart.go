// Package smart wraps smartctl invocations to discover disks and read their
// temperature. It relies on smartctl's JSON output (-j), which normalizes
// ATA, SCSI/SAS, and NVMe temperature reporting into one "temperature.current"
// field, so callers don't need per-protocol parsing.
package smart

import (
	"context"
	"encoding/json"
	"fmt"

	"dellfanctl/internal/execx"
)

// Device is one entry from `smartctl --scan -j`.
type Device struct {
	Name string `json:"name"` // e.g. /dev/bus/0
	Type string `json:"type"` // -d argument, e.g. "megaraid,1", "scsi", "sat"
	Info string `json:"info_name"`
}

type scanResult struct {
	Devices []Device `json:"devices"`
}

// Scan returns every device smartctl can identify, each with the -d
// argument needed to query it directly (important behind RAID controllers
// like a Dell PERC/MegaRAID, where the block device itself isn't queryable
// without specifying which physical disk behind it to address).
func Scan(ctx context.Context, bin string) ([]Device, error) {
	out, err := execx.Run(ctx, 0, bin, "--scan", "-j")
	if err != nil {
		return nil, err
	}
	var res scanResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		return nil, fmt.Errorf("parsing smartctl scan output: %w", err)
	}
	return res.Devices, nil
}

type attrResult struct {
	ModelName     string `json:"model_name"`      // ATA/NVMe
	ScsiModelName string `json:"scsi_model_name"` // SCSI/SAS
	SerialNumber  string `json:"serial_number"`
	Temperature   *struct {
		Current float64 `json:"current"`
	} `json:"temperature"`
	Device struct {
		InfoName string `json:"info_name"`
	} `json:"device"`
}

// Info is a snapshot of one disk's identity and temperature.
type Info struct {
	Model   string
	Serial  string
	TempC   float64
	HasTemp bool
}

// ReadInfo queries one device (by the Name/Type smartctl reported in Scan)
// for its model, serial, and current temperature. It's used by discovery
// only: "-a" pulls identity info (model/serial) in addition to attributes,
// which is more than the hot polling path needs. smartctl's exit code is
// nonzero whenever ANY SMART status bit is set (even benign ones), so a
// nonzero exit is tolerated as long as JSON was produced and parses; only a
// hard failure to get any output is treated as an error.
func ReadInfo(ctx context.Context, bin, name, devType string) (Info, error) {
	return read(ctx, bin, name, devType, "-a")
}

// ReadTemp reads just the current temperature, using "-A" (attributes
// only) since that's all the control loop's hot polling path needs.
func ReadTemp(ctx context.Context, bin, name, devType string) (float64, error) {
	info, err := read(ctx, bin, name, devType, "-A")
	if err != nil {
		return 0, err
	}
	if !info.HasTemp {
		return 0, fmt.Errorf("smartctl reported no temperature for %s (-d %s)", name, devType)
	}
	return info.TempC, nil
}

func read(ctx context.Context, bin, name, devType, infoFlag string) (Info, error) {
	out, err := execx.Run(ctx, 0, bin, infoFlag, "-j", "-d", devType, name)
	if out == "" && err != nil {
		return Info{}, err
	}
	var res attrResult
	if jerr := json.Unmarshal([]byte(out), &res); jerr != nil {
		if err != nil {
			return Info{}, err
		}
		return Info{}, fmt.Errorf("parsing smartctl output for %s (-d %s): %w", name, devType, jerr)
	}
	model := res.ModelName
	if model == "" {
		model = res.ScsiModelName
	}
	info := Info{Model: model, Serial: res.SerialNumber}
	if res.Temperature != nil {
		info.TempC = res.Temperature.Current
		info.HasTemp = true
	}
	return info, nil
}
