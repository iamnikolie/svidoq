package cmd

import (
	"fmt"
	"runtime"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// Build metadata, injected at link time by the Makefile and by GoReleaser:
//
//	-ldflags "-X github.com/iamnikolie/svidoq/cmd.version=v1.0.0"
//
// An unstamped build reports "dev" rather than claiming a version it does not
// have; buildVersion appends the VCS revision when the toolchain embedded one.
var version = "dev"

func buildVersion() string {
	v := version

	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" {
				rev := s.Value
				if len(rev) > 12 {
					rev = rev[:12]
				}

				v += " (" + rev + ")"
			}
		}
	}

	return v
}

func versionString() string {
	return fmt.Sprintf("svidoq %s %s/%s %s", buildVersion(), runtime.GOOS, runtime.GOARCH, runtime.Version())
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the version",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Version is data, so it goes to stdout: `v=$(svidoq version)` must work.
		fmt.Fprintln(stdout, versionString())

		return nil
	},
}

func init() {
	rootCmd.Version = versionString()
	rootCmd.SetVersionTemplate("{{.Version}}\n")
	rootCmd.AddCommand(versionCmd)
}
