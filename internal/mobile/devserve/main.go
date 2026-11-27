// devserve runs the phone core on a computer (developing the app's UI):
//
//	go run ./internal/mobile/devserve -dir <a longarm config dir>
//
// and prints the page's URL.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"

	"longarm/internal/mobile"
)

func main() {
	dir := flag.String("dir", "", "longarm config dir (hub.toml, tls/)")
	flag.Parse()
	c, err := mobile.Start(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(c.URL())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	<-ch
	c.Stop()
}
