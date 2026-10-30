// Command icons builds Longarm's icon files from the two masters in this
// directory (packaging/icons/README.md):
//
//	longarm.png        the icon (48 px and up)
//	longarm-small.png  its simplification for 16-32 px (fewer, bigger shapes)
//
// It writes longarm.ico (the MSI: Add/Remove Programs, the Start menu
// shortcut), the Windows resource objects that put that icon into the exes
// (cmd/<tool>/rsrc_windows_<arch>.syso, linked in by go build) and the web
// UI's favicon (web/public/favicon.png). Run it from the repository root
// after changing a master and commit what it wrote:
//
//	go run ./packaging/icons
package main

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"log"
	"os"
	"path/filepath"

	"github.com/nfnt/resize"
	"github.com/tc-hib/winres"
)

// The Windows exes that carry the icon.
var tools = []string{"longarm", "longarmd", "longarm-tray", "longarm-update", "longarm-hub", "longarm-relay"}

// Icon sizes: Windows picks the closest; the small ones come from the
// simplified master (the full one turns to mush below 40 px).
var (
	smallSizes = []uint{16, 20, 24, 32}
	largeSizes = []uint{40, 48, 64, 96, 128, 256}
)

func load(path string) image.Image {
	f, err := os.Open(path)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		log.Fatalf("%s: %v", path, err)
	}
	return img
}

func scaled(img image.Image, size uint) image.Image {
	return resize.Resize(size, size, img, resize.Lanczos3)
}

func write(path string, data []byte) {
	if err := os.WriteFile(path, data, 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Println("wrote", path)
}

func main() {
	dir := filepath.Join("packaging", "icons")
	large, small := load(filepath.Join(dir, "longarm.png")), load(filepath.Join(dir, "longarm-small.png"))

	var imgs []image.Image
	for _, s := range smallSizes {
		imgs = append(imgs, scaled(small, s))
	}
	for _, s := range largeSizes {
		imgs = append(imgs, scaled(large, s))
	}
	icon, err := winres.NewIconFromImages(imgs)
	if err != nil {
		log.Fatal(err)
	}
	var ico bytes.Buffer
	if err := icon.SaveICO(&ico); err != nil {
		log.Fatal(err)
	}
	write(filepath.Join(dir, "longarm.ico"), ico.Bytes())

	// Resource 1: the first icon group is the one Explorer and the taskbar show.
	rs := &winres.ResourceSet{}
	if err := rs.SetIcon(winres.ID(1), icon); err != nil {
		log.Fatal(err)
	}
	for _, tool := range tools {
		for _, arch := range []winres.Arch{winres.ArchAMD64, winres.ArchARM64} {
			var obj bytes.Buffer
			if err := rs.WriteObject(&obj, arch); err != nil {
				log.Fatal(err)
			}
			write(filepath.Join("cmd", tool, fmt.Sprintf("rsrc_windows_%s.syso", arch)), obj.Bytes())
		}
	}

	var fav bytes.Buffer
	if err := png.Encode(&fav, scaled(small, 64)); err != nil {
		log.Fatal(err)
	}
	write(filepath.Join("web", "public", "favicon.png"), fav.Bytes())
}
