package telemetry

import (
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/lubobill1990/longarm/internal/pii"
)

// Size caps of an item.
const (
	MaxMessage = 8 << 10 // message, exception message
	MaxName    = 256     // names, codes, types
	MaxProp    = 1 << 10 // a property value
	MaxProps   = 64      // properties (and measurements) per item
	MaxFrames  = 64
)

// sensitiveKeys are property keys whose values are never sent, whatever
// they contain: command lines, environment, paths, output.
var sensitiveKeys = map[string]bool{
	"argv": true, "args": true, "cmd": true, "command": true, "cmdline": true, "command_line": true,
	"env": true, "environment": true, "cwd": true, "dir": true, "workdir": true, "path": true, "file": true,
	"output": true, "stdout": true, "stderr": true, "body": true, "script": true, "prompt": true,
	"password": true, "secret": true, "token": true, "key": true, "code": true, "join_code": true,
	"hostname": true, "host": true, "remote": true, "peer": true, "addr": true, "address": true, "ip": true,
	"endpoints": true, "listen": true, "url": true, "email": true,
}

// SensitiveKey reports whether a property (or log attribute) key names
// data that is never sent; its value becomes "<redacted>".
func SensitiveKey(k string) bool {
	k = strings.ToLower(k)
	if i := strings.LastIndexByte(k, '.'); i >= 0 {
		k = k[i+1:]
	}
	return sensitiveKeys[k]
}

// quotedRE finds double-quoted strings in messages: Go errors quote what
// they are about (%q) - a program name, a path, a user's input.
var quotedRE = regexp.MustCompile(`"[^"\n]{1,512}"`)

// redactQuoted replaces quoted strings in free text by "<q>".
func redactQuoted(s string) string {
	if !strings.Contains(s, `"`) {
		return s
	}
	return quotedRE.ReplaceAllString(s, `"<q>"`)
}

// Prepare scrubs every string of it with s and applies the size caps. The
// hub runs it again with its own scrubber.
func Prepare(it *Item, s *pii.Scrubber) {
	str := func(v string, max int) string { return capString(s.String(v), max) }
	free := func(v string, max int) string { return str(redactQuoted(v), max) }
	it.Name = str(it.Name, MaxName)
	it.Message = free(it.Message, MaxMessage)
	it.ResultCode = str(it.ResultCode, MaxName)
	it.Type = str(it.Type, MaxName)
	it.Target = str(it.Target, MaxName)
	if len(it.Props) > 0 {
		keys := make([]string, 0, len(it.Props))
		for k := range it.Props {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if len(keys) > MaxProps {
			keys = keys[:MaxProps]
		}
		out := make(map[string]string, len(keys))
		for _, k := range keys {
			v := it.Props[k]
			if SensitiveKey(k) {
				v = "<redacted>"
			}
			out[str(k, MaxName)] = free(v, MaxProp)
		}
		it.Props = out
	}
	if len(it.Measurements) > MaxProps {
		keys := make([]string, 0, len(it.Measurements))
		for k := range it.Measurements {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make(map[string]float64, MaxProps)
		for _, k := range keys[:MaxProps] {
			out[k] = it.Measurements[k]
		}
		it.Measurements = out
	}
	if len(it.Stack) > MaxFrames {
		it.Stack = it.Stack[:MaxFrames]
	}
	for i := range it.Stack {
		f := &it.Stack[i]
		f.Function = str(f.Function, MaxName)
		f.File = str(trimFile(f.File), MaxName)
	}
}

// capString cuts s to at most max bytes (on a rune boundary).
func capString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max - len("...")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}
