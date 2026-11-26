//go:build darwin

package main

import (
	"fmt"
	"math"
	"os"
	"runtime"
	"structs"
	"time"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
)

// AppKit runs on the process's main thread: keep the main goroutine there.
func init() { runtime.LockOSThread() }

type nsPoint struct {
	_    structs.HostLayout
	X, Y float64
}

type nsSize struct {
	_    structs.HostLayout
	W, H float64
}

type nsRect struct {
	_      structs.HostLayout
	Origin nsPoint
	Size   nsSize
}

// pattern shows the remote desktop test's picture (pattern_windows.go)
// over the whole main display, above everything (the screen saver's
// level), for secs seconds: red, green, blue and white quadrants and a
// black square in the middle that turns yellow when clicked. It writes
// "ready W H" (pixels) to stdout once shown and every click ("click X Y",
// pixels from the top left) and character ("char C") to logPath.
func pattern(logPath string, secs int) {
	fail := func(err error) {
		fmt.Fprintln(os.Stderr, "pattern:", err)
		os.Exit(1)
	}
	if _, err := purego.Dlopen("/System/Library/Frameworks/AppKit.framework/AppKit", purego.RTLD_NOW|purego.RTLD_GLOBAL); err != nil {
		fail(err)
	}
	cg, err := purego.Dlopen("/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics", purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		fail(err)
	}
	var colorSRGB func(r, g, b, a float64) uintptr // CGColorCreateSRGB
	purego.RegisterLibFunc(&colorSRGB, cg, "CGColorCreateSRGB")
	log, err := os.Create(logPath)
	if err != nil {
		fail(err)
	}
	defer log.Close()

	s := func(id objc.ID, name string, args ...any) objc.ID { return id.Send(objc.RegisterName(name), args...) }
	cls := func(name string) objc.ID { return objc.ID(objc.GetClass(name)) }
	pool := s(s(cls("NSAutoreleasePool"), "alloc"), "init")
	defer s(pool, "drain")

	app := s(cls("NSApplication"), "sharedApplication")
	s(app, "setActivationPolicy:", 0) // regular: it may come to the front
	screen := s(cls("NSScreen"), "mainScreen")
	frame := objc.Send[nsRect](screen, objc.RegisterName("frame"))
	scale := objc.Send[float64](screen, objc.RegisterName("backingScaleFactor"))
	w, h := frame.Size.W, frame.Size.H

	var square objc.ID
	yellow := colorSRGB(1, 1, 0, 1)
	// A borderless window can't be the key window (take keys) unless it says so.
	winClass, err := objc.RegisterClass("LongarmPatternWindow", objc.GetClass("NSWindow"), nil, nil, []objc.MethodDef{
		{Cmd: objc.RegisterName("canBecomeKeyWindow"), Fn: func(objc.ID, objc.SEL) bool { return true }},
		{Cmd: objc.RegisterName("canBecomeMainWindow"), Fn: func(objc.ID, objc.SEL) bool { return true }},
	})
	if err != nil {
		fail(err)
	}
	viewClass, err := objc.RegisterClass("LongarmPatternView", objc.GetClass("NSView"), nil, nil, []objc.MethodDef{
		{Cmd: objc.RegisterName("acceptsFirstResponder"), Fn: func(objc.ID, objc.SEL) bool { return true }},
		{Cmd: objc.RegisterName("acceptsFirstMouse:"), Fn: func(objc.ID, objc.SEL, objc.ID) bool { return true }},
		{Cmd: objc.RegisterName("mouseDown:"), Fn: func(_ objc.ID, _ objc.SEL, ev objc.ID) {
			p := objc.Send[nsPoint](ev, objc.RegisterName("locationInWindow")) // points, from the bottom left
			fmt.Fprintf(log, "click %d %d\n", int(math.Round(p.X*scale)), int(math.Round((h-p.Y)*scale)))
			log.Sync()
			s(s(square, "layer"), "setBackgroundColor:", objc.ID(yellow))
		}},
		{Cmd: objc.RegisterName("keyDown:"), Fn: func(_ objc.ID, _ objc.SEL, ev objc.ID) {
			chars := objc.Send[string](s(ev, "characters"), objc.RegisterName("UTF8String"))
			for _, r := range chars {
				fmt.Fprintf(log, "char %c\n", r)
			}
			log.Sync()
		}},
	})
	if err != nil {
		fail(err)
	}

	const borderless, backingBuffered, screenSaverLevel = 0, 2, 1000
	win := s(s(objc.ID(winClass), "alloc"), "initWithContentRect:styleMask:backing:defer:", frame, uint(borderless), uint(backingBuffered), false)
	s(win, "setLevel:", screenSaverLevel)
	content := s(s(objc.ID(viewClass), "alloc"), "initWithFrame:", nsRect{Size: frame.Size})
	s(content, "setWantsLayer:", true)
	// AppKit's y goes up: the top quadrants are at h/2.
	box := func(x, y, bw, bh float64, color uintptr) objc.ID {
		v := s(s(cls("NSView"), "alloc"), "initWithFrame:", nsRect{Origin: nsPoint{X: x, Y: y}, Size: nsSize{W: bw, H: bh}})
		s(v, "setWantsLayer:", true)
		s(s(v, "layer"), "setBackgroundColor:", objc.ID(color))
		s(content, "addSubview:", v)
		return v
	}
	box(0, h/2, w/2, h/2, colorSRGB(1, 0, 0, 1))   // red, top left
	box(w/2, h/2, w/2, h/2, colorSRGB(0, 1, 0, 1)) // green, top right
	box(0, 0, w/2, h/2, colorSRGB(0, 0, 1, 1))     // blue, bottom left
	box(w/2, 0, w/2, h/2, colorSRGB(1, 1, 1, 1))   // white, bottom right
	side := 120 / scale                            // 120 pixels, as on Windows
	square = box(w/2-side/2, h/2-side/2, side, side, colorSRGB(0, 0, 0, 1))
	s(win, "setContentView:", content)
	s(win, "makeKeyAndOrderFront:", objc.ID(0))
	s(win, "makeFirstResponder:", content)
	s(app, "activateIgnoringOtherApps:", true)

	time.AfterFunc(500*time.Millisecond, func() { fmt.Printf("ready %d %d\n", int(w*scale), int(h*scale)) })
	time.AfterFunc(time.Duration(secs)*time.Second, func() { os.Exit(0) })
	s(app, "run")
}
