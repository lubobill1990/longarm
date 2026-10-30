// Command longarm-update is the Longarm Update service (LongarmUpdate,
// Windows, installed by the MSI; internal/updatesvc, docs/security.md
// "Update service"): it installs newer Longarm MSIs from the recorded hub,
// signed with the release key built into it, and nothing else.
//
//	longarm-update run                       the service (as LocalSystem, started by the service manager)
//	longarm-update status                    the service's status (JSON)
//	longarm-update install --latest | --version V [--reinstall]
//	longarm-update auto on|off [--hours N]   automatic updates (the machine's owner or an administrator)
//	longarm-update channel [stable|preview|dev]   the release channel (setting it: the owner or an administrator)
//	longarm-update version
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/lubobill1990/longarm/internal/update"
	"github.com/lubobill1990/longarm/internal/updatesvc"
	"github.com/lubobill1990/longarm/internal/version"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = runService()
	case "status":
		err = status()
	case "install":
		err = install(os.Args[2:])
	case "auto":
		err = auto(os.Args[2:])
	case "channel":
		err = channel(os.Args[2:])
	case "version":
		fmt.Println("longarm-update", version.Version)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "longarm-update:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: longarm-update run | status | install --latest|--version V [--reinstall] | auto on|off [--hours N] | channel [stable|preview|dev] | version")
	os.Exit(2)
}

func client() *updatesvc.Client { return updatesvc.NewClient("longarm-update (local)") }

func show(r *updatesvc.Response) error {
	b, _ := json.MarshalIndent(r, "", "  ")
	fmt.Println(string(b))
	return r.Err()
}

func status() error {
	r, err := client().Do(context.Background(), &updatesvc.Request{Op: updatesvc.OpStatus})
	if err != nil {
		return err
	}
	return show(r)
}

func install(args []string) error {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	latest := fs.Bool("latest", false, "the hub's newest release of the machine's channel")
	ver := fs.String("version", "", "a version the hub has")
	re := fs.Bool("reinstall", false, "install the installed version again (administrators)")
	fs.Parse(args)
	if *latest == (*ver != "") {
		return errors.New("--latest or --version")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	r, err := client().Do(ctx, &updatesvc.Request{Op: updatesvc.OpInstall, Latest: *latest, Version: *ver, Reinstall: *re})
	if err != nil {
		return err
	}
	return show(r)
}

func auto(args []string) error {
	if len(args) < 1 || (args[0] != "on" && args[0] != "off") {
		return errors.New("auto on|off [--hours N]")
	}
	fs := flag.NewFlagSet("auto", flag.ExitOnError)
	hours := fs.Int("hours", 0, "hours between checks (default 6)")
	fs.Parse(args[1:])
	r, err := client().Do(context.Background(), &updatesvc.Request{Op: updatesvc.OpSetAuto, Auto: args[0] == "on", IntervalHours: *hours})
	if err != nil {
		return err
	}
	return show(r)
}

// channel prints the machine's release channel, or sets it.
func channel(args []string) error {
	if len(args) == 0 {
		r, err := client().Do(context.Background(), &updatesvc.Request{Op: updatesvc.OpStatus})
		if err != nil {
			return err
		}
		if r.Status == nil {
			return r.Err()
		}
		fmt.Println(cmp.Or(r.Status.Channel, update.ChannelStable)) // older services: stable
		return nil
	}
	if _, ok := update.NormalizeChannel(args[0]); !ok || len(args) > 1 {
		return errors.New("channel [stable|preview|dev]")
	}
	r, err := client().Do(context.Background(), &updatesvc.Request{Op: updatesvc.OpSetChannel, Channel: args[0]})
	if err != nil {
		return err
	}
	return show(r)
}
