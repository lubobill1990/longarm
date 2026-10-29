package main

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/lubobill1990/longarm/internal/hub"
)

func healthcheckCmd() *cobra.Command {
	var url, marker string
	var live bool
	var stale time.Duration
	c := &cobra.Command{
		Use:   "healthcheck",
		Short: "Exit 0 when the hub's /healthz answers (for container health checks)",
		Long: `Exit 0 when the hub's /healthz answers: the readiness probe.

--live is the liveness probe: it also passes while the container is still
starting (longarm-hub container: Litestream restore, bootstrap, the hub
opening its database) as long as that start refreshed its startup marker
(<database dir>/` + hub.StartupMarkerFile + `) within --stale, so a long
restore is not killed but a dead or hung one is.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if url == "" || (live && marker == "") {
				cfg, err := loadConfig()
				if err != nil {
					return err
				}
				if url == "" {
					url = healthURL(cfg.HTTP)
				}
				if marker == "" {
					marker = hub.StartupMarkerPath(cfg)
				}
			}
			herr := getHealthz(url)
			if !live {
				return herr
			}
			msg, err := liveCheck(herr, marker, stale, time.Now())
			if msg != "" {
				fmt.Fprintln(os.Stderr, msg)
			}
			return err
		},
	}
	c.Flags().StringVar(&url, "url", "", "health URL (default: from the config's http listener)")
	c.Flags().BoolVar(&live, "live", false, "liveness: also pass while the container start (restore, bootstrap) is alive")
	c.Flags().StringVar(&marker, "marker", "", "with --live: the startup marker (default: next to the database)")
	c.Flags().DurationVar(&stale, "stale", hub.StartupMarkerStale, "with --live: a startup marker older than this does not count")
	return c
}

// healthURL is /healthz on the http listener, a wildcard one on loopback.
func healthURL(addr string) string {
	if strings.HasPrefix(addr, ":") || strings.HasPrefix(addr, "0.0.0.0:") {
		addr = "127.0.0.1:" + addr[strings.LastIndex(addr, ":")+1:]
	}
	return "http://" + addr + "/healthz"
}

func getHealthz(url string) error {
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	return nil
}

// liveCheck is the liveness verdict: alive when /healthz answered (herr nil)
// or the startup marker is fresh. msg is a line for the probe's output.
func liveCheck(herr error, marker string, stale time.Duration, now time.Time) (msg string, err error) {
	if herr == nil {
		return "", nil
	}
	phase, age, ok, merr := hub.StartupInProgress(marker, stale, now)
	switch {
	case merr != nil:
		return "", fmt.Errorf("%w (startup marker: %v)", herr, merr)
	case ok:
		return fmt.Sprintf("starting (%s; marker refreshed %s ago): %v", phase, age.Round(time.Second), herr), nil
	case phase != "" || age != 0:
		return "", fmt.Errorf("%w (start %q stalled: marker not refreshed for %s)", herr, phase, age.Round(time.Second))
	}
	return "", herr
}
