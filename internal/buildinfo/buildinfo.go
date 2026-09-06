// Package buildinfo identifies the program independently of its installation path.
package buildinfo

import "runtime/debug"

// Version is the release version shared by zotigo and zotigod.
const Version = "0.0.1"

func Current() (string, string) {
	commit := "unknown"
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				commit = setting.Value
			}
		}
	}
	return Version, commit
}
