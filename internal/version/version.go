// Package version holds the single version string shared by the CLI's
// `version` command and anywhere else (e.g. MQTT discovery payloads) that
// wants to report it.
package version

// Version defaults to a development marker. Release builds overwrite it at
// link time via -ldflags, e.g.:
//
//	go build -ldflags "-X dellfanctl/internal/version.Version=v1.2.3" ...
//
// (see .github/workflows/release.yml, which sets it from the pushed tag).
var Version = "dev"
