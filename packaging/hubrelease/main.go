// Command hubrelease turns GoReleaser's dist/ into the flat directory that
// `longarm-hub release add` imports, with the canonical file names the hub,
// the install scripts and the self-update use:
//
//	longarm-<os>-<arch>[.exe], longarmd-..., longarm-tray-..., longarm-hub-..., longarm-relay-...   binaries
//	longarm-<os>-<arch>.tar.gz|zip                                       client/worker archives
//	longarm-linux-<arch>.deb|rpm                                          packages
//	longarm-windows-<arch>.msi                                            (copied by the wrapper)
//
// and signs every file with a minisign key (<file>.minisig; the trusted
// comment binds file name and version).
//
//	go run ./packaging/hubrelease --dist dist --out dist/hub-release [--key K]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/lubobill1990/longarm/internal/hub"
)

type artifact struct {
	Name   string         `json:"name"`
	Path   string         `json:"path"`
	Goos   string         `json:"goos"`
	Goarch string         `json:"goarch"`
	Type   string         `json:"type"`
	Extra  map[string]any `json:"extra"`
}

func main() {
	dist := flag.String("dist", "dist", "GoReleaser dist dir")
	out := flag.String("out", "", "output dir (default <dist>/hub-release)")
	key := flag.String("key", "", "minisign private key to sign with (optional)")
	passEnv := flag.String("password-env", "LONGARM_RELEASE_KEY_PASSWORD", "env var with the key password")
	flag.Parse()
	if *out == "" {
		*out = filepath.Join(*dist, "hub-release")
	}
	if err := run(*dist, *out, *key, os.Getenv(*passEnv)); err != nil {
		fmt.Fprintln(os.Stderr, "hubrelease:", err)
		os.Exit(1)
	}
}

func run(dist, out, key, pass string) error {
	var meta struct {
		Version string `json:"version"`
	}
	b, err := os.ReadFile(filepath.Join(dist, "metadata.json"))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		return err
	}
	b, err = os.ReadFile(filepath.Join(dist, "artifacts.json"))
	if err != nil {
		return err
	}
	var arts []artifact
	if err := json.Unmarshal(b, &arts); err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	n := 0
	for _, a := range arts {
		id, _ := a.Extra["ID"].(string)
		var name string
		switch a.Type {
		case "Binary":
			bin, _ := a.Extra["Binary"].(string)
			name = bin + "-" + a.Goos + "-" + a.Goarch
			if a.Goos == "windows" {
				name += ".exe"
			}
		case "Archive":
			if id != "longarm" {
				continue
			}
			ext := ".tar.gz"
			if strings.HasSuffix(a.Name, ".zip") {
				ext = ".zip"
			}
			name = "longarm-" + a.Goos + "-" + a.Goarch + ext
		case "Linux Package":
			if id != "longarm" {
				continue
			}
			name = "longarm-linux-" + a.Goarch + filepath.Ext(a.Name)
		default:
			continue
		}
		if err := copyFile(a.Path, filepath.Join(out, name)); err != nil {
			return err
		}
		n++
	}
	fmt.Printf("%d files for version %s in %s\n", n, meta.Version, out)
	if key == "" {
		fmt.Println("not signed (no --key): workers refuse unsigned updates")
		return nil
	}
	signed, err := hub.SignRelease(out, meta.Version, key, pass)
	if err != nil {
		return err
	}
	fmt.Printf("signed %d files\n", len(signed))
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	o, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(o, in); err != nil {
		o.Close()
		return err
	}
	return o.Close()
}
