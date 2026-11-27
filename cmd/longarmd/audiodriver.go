package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"longarm/internal/audiodrv"
	"longarm/internal/platform"
)

// audioDriverCmd: the Longarm audio driver (internal/audiodrv).
func audioDriverCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "audio-driver",
		Short: "The Longarm audio driver (Windows): your microphone on this worker in longarm desktop",
		Long: `The Longarm audio driver adds a virtual microphone, "Longarm Microphone":
what longarm desktop's viewer says into its microphone comes out of it here,
for any program on this machine. It is a kernel driver: Windows loads it
with Secure Boot on only once Microsoft signed it (until then, on machines
in test-signing mode). docs/remote-desktop.md "Microphone".`,
	}
	install := &cobra.Command{
		Use:   "install [LongarmAudio.inf]",
		Short: "Install (or update) the driver (elevated); default: the one beside longarmd",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !platform.Elevated() {
				return errors.New("run it elevated (as administrator): it installs a driver")
			}
			inf := ""
			if len(args) == 1 {
				inf = args[0]
			} else {
				exe, err := os.Executable()
				if err != nil {
					return err
				}
				inf = filepath.Join(filepath.Dir(exe), "audio", "LongarmAudio.inf")
			}
			reboot, err := audiodrv.Install(inf)
			if err != nil {
				return err
			}
			fmt.Println(`installed: "Longarm Microphone" is a microphone here`)
			if reboot {
				fmt.Println("Windows asks for a restart to finish")
			}
			return nil
		},
	}
	uninstall := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the driver (elevated)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !platform.Elevated() {
				return errors.New("run it elevated (as administrator)")
			}
			return audiodrv.Uninstall()
		},
	}
	status := &cobra.Command{
		Use:   "status",
		Short: "Whether the driver is installed",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ds, err := audiodrv.Devices()
			if err != nil {
				return err
			}
			if len(ds) == 0 {
				fmt.Println("not installed")
			}
			for _, d := range ds {
				fmt.Println(d)
			}
			return nil
		},
	}
	c.AddCommand(install, uninstall, status)
	return c
}
