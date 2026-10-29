// acirules checks deploy/azure/azuredeploy.json against Azure Container
// Instances' rules that ARM validation (and the bicep linter) don't know,
// for every combination of the parameters that shape the container group:
//
//	go run ./deploy/azure/test/acirules deploy/azure/azuredeploy.json [param=value ...]
//
// Rules (each one failed a real deployment, or would):
//   - a port number at most once per container and per group, whatever the
//     protocol (DuplicateContainerPorts: 7443/tcp + 7443/udp was refused)
//   - every port of the group's IP address is a port of some container
//   - at most 5 ports per IP address (an ACI hard limit; the hub with its
//     panel uses all 5, which is why the Headscale sidecar has no STUN port)
//   - memoryInGB in steps of 0.1 GB (the 0.25 GB sidecar was refused), CPU in
//     steps of 0.01, both > 0; the group at most 4 vCPU / 16 GB
//   - environment variable names unique, each with value or secureValue
//   - volume mounts name volumes of the group
//   - the hub's liveness probe survives a long Litestream restore: either
//     `healthcheck --live` (passes on the restore's heartbeat) or a plain
//     healthcheck no earlier than 20 min (a 7-minute restore was killed at
//     3 min: a crash loop)
//
// The template is evaluated with a small ARM expression evaluator (the
// functions the template uses; anything resolved at deploy time such as
// reference() or listKeys() is an opaque value).
package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: acirules azuredeploy.json [param=value ...]")
		os.Exit(2)
	}
	b, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	overrides := map[string]any{}
	for _, kv := range os.Args[2:] {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			fmt.Fprintln(os.Stderr, "acirules: want param=value:", kv)
			os.Exit(2)
		}
		var x any
		if json.Unmarshal([]byte(v), &x) != nil {
			x = v
		}
		overrides[k] = x
	}
	cases, problems, err := Check(b, overrides)
	if err != nil {
		fmt.Fprintln(os.Stderr, "acirules:", err)
		os.Exit(2)
	}
	for _, p := range problems {
		fmt.Println(p)
	}
	if len(problems) > 0 {
		fmt.Printf("acirules: %d problem(s) in %d parameter combinations\n", len(problems), cases)
		os.Exit(1)
	}
	fmt.Printf("acirules: ok (%d parameter combinations)\n", cases)
}

type template struct {
	Parameters map[string]struct {
		Type          string `json:"type"`
		DefaultValue  any    `json:"defaultValue"`
		AllowedValues []any  `json:"allowedValues"`
		MinValue      *int   `json:"minValue"`
		MaxValue      *int   `json:"maxValue"`
	} `json:"parameters"`
	Variables map[string]any   `json:"variables"`
	Resources []map[string]any `json:"resources"`
}

// notShaping are parameters that only add environment variables, role
// assignments or other resources: checked with one value (nil = the
// default). On adds the most environment variables, so a duplicate would
// still show.
var notShaping = map[string]any{
	"enableTelemetry":          true,
	"enableDiagnostics":        true,
	"telemetryRetentionDays":   nil,
	"diagnosticsRetentionDays": nil,
	"adminCodeTtlHours":        nil,
	"headscalePublicDerp":      true,
	// only the hub's liveness command (TestLivenessProbe checks each)
	"hubLivenessProbe": nil,
}

// Check evaluates the container group(s) of the template for every
// combination of the parameters that shape them (bools, allowed values,
// relayUdpPort-like ints at their bounds, secrets empty or set) and returns
// the number of combinations and the rule violations.
func Check(raw []byte, overrides map[string]any) (int, []string, error) {
	var t template
	if err := json.Unmarshal(raw, &t); err != nil {
		return 0, nil, err
	}
	// the axes: parameters with a few interesting values each
	type axis struct {
		name string
		vals []any
	}
	// fixed port numbers of the template
	var fixedPorts []float64
	for _, m := range regexp.MustCompile(`(?:"port":\s*|'port', )(\d+)`).FindAllSubmatch(raw, -1) {
		f, _ := strconv.ParseFloat(string(m[1]), 64)
		if !slices.Contains(fixedPorts, f) {
			fixedPorts = append(fixedPorts, f)
		}
	}
	var axes []axis
	base := map[string]any{"name": "myhub", "location": "eastasia"}
	for n, p := range t.Parameters {
		if v, ok := overrides[n]; ok {
			base[n] = v
			continue
		}
		if v, ok := notShaping[n]; ok {
			// no effect on ports, sizes or volumes: one value keeps the
			// combinations (and CI's time) from multiplying
			if v != nil {
				base[n] = v
			} else if p.DefaultValue != nil {
				base[n] = p.DefaultValue
			}
			continue
		}
		switch {
		case len(p.AllowedValues) > 0:
			axes = append(axes, axis{n, p.AllowedValues})
		case p.Type == "bool":
			axes = append(axes, axis{n, []any{true, false}})
		case p.Type == "int" && p.MinValue != nil && p.MaxValue != nil:
			// the default, the bounds, and next to every fixed port of the
			// template (a port parameter + 1 = 7443 would collide)
			def, _ := p.DefaultValue.(float64)
			vals := []any{def, float64(*p.MinValue), float64(*p.MaxValue)}
			for _, f := range fixedPorts {
				for _, d := range []float64{-2, -1, 0, 1} {
					if v := f + d; v >= float64(*p.MinValue) && v <= float64(*p.MaxValue) && !slices.Contains(vals, any(v)) {
						vals = append(vals, v)
					}
				}
			}
			axes = append(axes, axis{n, vals})
		case p.Type == "securestring" || p.Type == "secureString":
			axes = append(axes, axis{n, []any{"", "s3cret"}})
		case p.DefaultValue != nil:
			if _, set := base[n]; !set {
				base[n] = p.DefaultValue // may be an expression
			}
		}
	}
	// a username for registryPassword's combination to matter
	if _, ok := t.Parameters["registryUsername"]; ok {
		if _, set := overrides["registryUsername"]; !set {
			axes = append(axes, axis{"registryUsername", []any{"", "bot"}})
		}
	}
	var problems []string
	seen := map[string]bool{}
	cases := 0
	idx := make([]int, len(axes))
	for {
		params := map[string]any{}
		for k, v := range base {
			params[k] = v
		}
		var desc []string
		for i, a := range axes {
			params[a.name] = a.vals[idx[i]]
			desc = append(desc, fmt.Sprintf("%s=%v", a.name, a.vals[idx[i]]))
		}
		cases++
		ev := &evaluator{t: &t, params: params, vars: map[string]any{}}
		for _, r := range t.Resources {
			if r["type"] != "Microsoft.ContainerInstance/containerGroups" {
				continue
			}
			if c, ok := r["condition"]; ok {
				v, err := ev.value(c)
				if err != nil {
					return cases, nil, err
				}
				if v == false {
					continue
				}
			}
			props, err := ev.value(r["properties"])
			if err != nil {
				return cases, nil, fmt.Errorf("%s: %w", strings.Join(desc, " "), err)
			}
			for _, p := range checkGroup(props) {
				if !seen[p] {
					seen[p] = true
					problems = append(problems, p+"  ("+strings.Join(desc, " ")+")")
				}
			}
		}
		// next combination
		i := 0
		for ; i < len(axes); i++ {
			idx[i]++
			if idx[i] < len(axes[i].vals) {
				break
			}
			idx[i] = 0
		}
		if i == len(axes) {
			break
		}
	}
	return cases, problems, nil
}

func checkGroup(props any) []string {
	var out []string
	bad := func(f string, a ...any) { out = append(out, fmt.Sprintf(f, a...)) }
	g, _ := props.(map[string]any)
	containers, _ := g["containers"].([]any)
	if len(containers) == 0 {
		return []string{"no containers"}
	}
	volumes := map[string]bool{}
	if vs, ok := g["volumes"].([]any); ok {
		for _, v := range vs {
			if m, ok := v.(map[string]any); ok {
				volumes[fmt.Sprint(m["name"])] = true
			}
		}
	}
	exposed := map[float64]string{} // container port -> protocol
	var cpu, mem float64
	names := map[string]bool{}
	for _, c := range containers {
		cm, _ := c.(map[string]any)
		name := fmt.Sprint(cm["name"])
		if names[name] {
			bad("container %s: duplicate container name", name)
		}
		names[name] = true
		p, _ := cm["properties"].(map[string]any)
		ports := map[float64]bool{}
		for _, x := range list(p["ports"]) {
			pm, _ := x.(map[string]any)
			n, ok := pm["port"].(float64)
			proto := strings.ToUpper(fmt.Sprint(pm["protocol"]))
			if pm["protocol"] == nil {
				proto = "TCP"
			}
			switch {
			case !ok || n < 1 || n > 65535 || n != math.Trunc(n):
				bad("container %s: port %v is not 1-65535", name, pm["port"])
			case ports[n]:
				bad("container %s: port %v twice (DuplicateContainerPorts: ACI refuses one number for TCP and UDP)", name, n)
			case proto != "TCP" && proto != "UDP":
				bad("container %s: port %v protocol %q", name, n, proto)
			}
			ports[n] = true
			exposed[n] = proto
		}
		req, _ := mapAt(p, "resources", "requests")
		c, cok := req["cpu"].(float64)
		m, mok := req["memoryInGB"].(float64)
		if !cok || c <= 0 || !steps(c, 100) {
			bad("container %s: cpu %v: want > 0 in steps of 0.01", name, req["cpu"])
		}
		if !mok || m <= 0 || !steps(m, 10) {
			bad("container %s: memoryInGB %v: want > 0 in steps of 0.1 GB", name, req["memoryInGB"])
		}
		cpu += c
		mem += m
		env := map[string]bool{}
		for _, x := range list(p["environmentVariables"]) {
			em, _ := x.(map[string]any)
			n := fmt.Sprint(em["name"])
			if env[n] {
				bad("container %s: environment variable %s twice", name, n)
			}
			env[n] = true
			_, v := em["value"]
			_, s := em["secureValue"]
			if v == s {
				bad("container %s: environment variable %s needs exactly one of value / secureValue", name, n)
			}
		}
		if name == "hub" {
			out = append(out, checkHubProbes(p)...)
		}
		for _, x := range list(p["volumeMounts"]) {
			vm, _ := x.(map[string]any)
			if !volumes[fmt.Sprint(vm["name"])] {
				bad("container %s: volume mount %v names no volume of the group", name, vm["name"])
			}
		}
	}
	if cpu > 4+1e-9 || mem > 16+1e-9 {
		bad("the group requests %.2f vCPU / %.1f GB: ACI allows 4 vCPU / 16 GB per group", cpu, mem)
	}
	ip, _ := g["ipAddress"].(map[string]any)
	gports := map[float64]bool{}
	for _, x := range list(ip["ports"]) {
		pm, _ := x.(map[string]any)
		n, _ := pm["port"].(float64)
		proto := strings.ToUpper(fmt.Sprint(pm["protocol"]))
		if gports[n] {
			bad("ipAddress: port %v twice", n)
		}
		gports[n] = true
		if cp, ok := exposed[n]; !ok || cp != proto {
			bad("ipAddress: port %v/%s is not a port of any container", n, proto)
		}
	}
	if len(gports) > 5 {
		bad("ipAddress: %d ports: ACI allows at most 5 ports per IP address", len(gports))
	}
	return out
}

// checkHubProbes: the hub's probes are longarm-hub healthcheck; the
// liveness probe must not kill a restore that takes minutes.
func checkHubProbes(p map[string]any) []string {
	var out []string
	for _, k := range []string{"livenessProbe", "readinessProbe"} {
		pr, ok := p[k].(map[string]any)
		if !ok {
			out = append(out, "container hub: no "+k)
			continue
		}
		cmd, _ := mapAt(pr, "exec")
		args := list(cmd["command"])
		if len(args) < 2 || fmt.Sprint(args[0]) != "/usr/local/bin/longarm-hub" || fmt.Sprint(args[1]) != "healthcheck" {
			out = append(out, fmt.Sprintf("container hub: %s %v is not longarm-hub healthcheck", k, args))
			continue
		}
		live := slices.ContainsFunc(args[2:], func(a any) bool { return a == "--live" })
		delay, _ := pr["initialDelaySeconds"].(float64)
		switch {
		case k == "readinessProbe" && live:
			out = append(out, "container hub: the readiness probe has --live (a restoring hub is not ready)")
		case k == "livenessProbe" && !live && delay < 1200:
			out = append(out, fmt.Sprintf("container hub: liveness probe without --live after %v s kills a long Litestream restore", delay))
		}
	}
	return out
}

func steps(v float64, per float64) bool {
	return math.Abs(v*per-math.Round(v*per)) < 1e-6
}

func list(v any) []any { l, _ := v.([]any); return l }

func mapAt(m map[string]any, keys ...string) (map[string]any, bool) {
	for _, k := range keys {
		n, ok := m[k].(map[string]any)
		if !ok {
			return nil, false
		}
		m = n
	}
	return m, true
}

// opaque is a value only known at deploy time (reference(), listKeys(), ...).
type opaque struct{ what string }

func (o opaque) String() string { return "<" + o.what + ">" }

type evaluator struct {
	t      *template
	params map[string]any
	vars   map[string]any
	depth  int
}

// value evaluates a template value: JSON with "[expression]" strings.
func (e *evaluator) value(v any) (any, error) {
	switch x := v.(type) {
	case string:
		if strings.HasPrefix(x, "[[") {
			return x[1:], nil
		}
		if strings.HasPrefix(x, "[") && strings.HasSuffix(x, "]") {
			p := &parser{s: x[1 : len(x)-1]}
			n, err := p.expr()
			if err != nil {
				return nil, fmt.Errorf("%q: %w", x, err)
			}
			if p.skip(); p.i != len(p.s) {
				return nil, fmt.Errorf("%q: trailing %q", x, p.s[p.i:])
			}
			return e.eval(n)
		}
		return x, nil
	case map[string]any:
		out := map[string]any{}
		for k, f := range x {
			r, err := e.value(f)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, f := range x {
			r, err := e.value(f)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	}
	return v, nil
}

type node struct {
	kind string // lit, call, prop, index
	val  any
	name string
	args []*node
}

type parser struct {
	s string
	i int
}

func (p *parser) skip() {
	for p.i < len(p.s) && p.s[p.i] == ' ' {
		p.i++
	}
}

func (p *parser) expr() (*node, error) {
	p.skip()
	if p.i >= len(p.s) {
		return nil, fmt.Errorf("unexpected end")
	}
	var n *node
	switch c := p.s[p.i]; {
	case c == '\'':
		var sb strings.Builder
		p.i++
		for {
			if p.i >= len(p.s) {
				return nil, fmt.Errorf("unterminated string")
			}
			if p.s[p.i] == '\'' {
				if p.i+1 < len(p.s) && p.s[p.i+1] == '\'' {
					sb.WriteByte('\'')
					p.i += 2
					continue
				}
				p.i++
				break
			}
			sb.WriteByte(p.s[p.i])
			p.i++
		}
		n = &node{kind: "lit", val: sb.String()}
	case c == '-' || (c >= '0' && c <= '9'):
		j := p.i + 1
		for j < len(p.s) && (p.s[j] >= '0' && p.s[j] <= '9' || p.s[j] == '.') {
			j++
		}
		f, err := strconv.ParseFloat(p.s[p.i:j], 64)
		if err != nil {
			return nil, err
		}
		p.i = j
		n = &node{kind: "lit", val: f}
	default:
		j := p.i
		for j < len(p.s) && (p.s[j] == '_' || p.s[j] >= 'a' && p.s[j] <= 'z' || p.s[j] >= 'A' && p.s[j] <= 'Z' || p.s[j] >= '0' && p.s[j] <= '9') {
			j++
		}
		if j == p.i {
			return nil, fmt.Errorf("unexpected %q at %d", c, p.i)
		}
		name := p.s[p.i:j]
		p.i = j
		p.skip()
		switch {
		case (name == "true" || name == "false") && !(p.i < len(p.s) && p.s[p.i] == '('):
			n = &node{kind: "lit", val: name == "true"}
		case name == "null" && !(p.i < len(p.s) && p.s[p.i] == '('):
			n = &node{kind: "lit", val: nil}
		case p.i < len(p.s) && p.s[p.i] == '(':
			p.i++
			n = &node{kind: "call", name: name}
			for {
				p.skip()
				if p.i < len(p.s) && p.s[p.i] == ')' {
					p.i++
					break
				}
				a, err := p.expr()
				if err != nil {
					return nil, err
				}
				n.args = append(n.args, a)
				p.skip()
				if p.i < len(p.s) && p.s[p.i] == ',' {
					p.i++
				}
			}
		default:
			return nil, fmt.Errorf("bare identifier %s", name)
		}
	}
	// .prop, [index], .prop? (safe dereference)
	for {
		p.skip()
		if p.i >= len(p.s) {
			return n, nil
		}
		switch p.s[p.i] {
		case '.':
			p.i++
			j := p.i
			for j < len(p.s) && (p.s[j] == '_' || p.s[j] >= 'a' && p.s[j] <= 'z' || p.s[j] >= 'A' && p.s[j] <= 'Z' || p.s[j] >= '0' && p.s[j] <= '9') {
				j++
			}
			n = &node{kind: "prop", name: p.s[p.i:j], args: []*node{n}}
			p.i = j
			if p.i < len(p.s) && p.s[p.i] == '?' {
				p.i++
			}
		case '[':
			p.i++
			ix, err := p.expr()
			if err != nil {
				return nil, err
			}
			p.skip()
			if p.i >= len(p.s) || p.s[p.i] != ']' {
				return nil, fmt.Errorf("missing ]")
			}
			p.i++
			n = &node{kind: "index", args: []*node{n, ix}}
		default:
			return n, nil
		}
	}
}

func (e *evaluator) eval(n *node) (any, error) {
	switch n.kind {
	case "lit":
		return n.val, nil
	case "prop", "index":
		base, err := e.eval(n.args[0])
		if err != nil {
			return nil, err
		}
		key := any(n.name)
		if n.kind == "index" {
			if key, err = e.eval(n.args[1]); err != nil {
				return nil, err
			}
		}
		switch b := base.(type) {
		case opaque:
			return b, nil
		case map[string]any:
			return b[fmt.Sprint(key)], nil
		case []any:
			i, ok := key.(float64)
			if !ok || int(i) < 0 || int(i) >= len(b) {
				return nil, fmt.Errorf("index %v of a list of %d", key, len(b))
			}
			return b[int(i)], nil
		}
		return nil, fmt.Errorf("%v of %T", key, base)
	}
	// calls; if() is lazy
	if n.name == "if" {
		if len(n.args) != 3 {
			return nil, fmt.Errorf("if: 3 arguments")
		}
		c, err := e.eval(n.args[0])
		if err != nil {
			return nil, err
		}
		if _, ok := c.(opaque); ok {
			return nil, fmt.Errorf("if() on a deploy-time value")
		}
		if c == true {
			return e.eval(n.args[1])
		}
		return e.eval(n.args[2])
	}
	args := make([]any, len(n.args))
	for i, a := range n.args {
		v, err := e.eval(a)
		if err != nil {
			return nil, err
		}
		args[i] = v
	}
	str := func(i int) string {
		if i < len(args) {
			if f, ok := args[i].(float64); ok {
				return strconv.FormatFloat(f, 'f', -1, 64)
			}
			return fmt.Sprint(args[i])
		}
		return ""
	}
	num := func(i int) float64 { f, _ := args[i].(float64); return f }
	switch n.name {
	case "parameters":
		v, ok := e.params[str(0)]
		if !ok {
			if _, declared := e.t.Parameters[str(0)]; declared {
				return "", nil // no default: deploy.sh passes it or it's optional
			}
			return nil, fmt.Errorf("parameters(%q): not declared", str(0))
		}
		if s, ok := v.(string); ok && strings.HasPrefix(s, "[") {
			return e.value(s)
		}
		return v, nil
	case "variables":
		if v, ok := e.vars[str(0)]; ok {
			return v, nil
		}
		raw, ok := e.t.Variables[str(0)]
		if !ok {
			return nil, fmt.Errorf("variables(%q): not declared", str(0))
		}
		if e.depth++; e.depth > 50 {
			return nil, fmt.Errorf("variables: too deep")
		}
		v, err := e.value(raw)
		e.depth--
		if err != nil {
			return nil, err
		}
		e.vars[str(0)] = v
		return v, nil
	case "createArray":
		return append([]any{}, args...), nil
	case "createObject":
		m := map[string]any{}
		for i := 0; i+1 < len(args); i += 2 {
			m[str(i)] = args[i+1]
		}
		return m, nil
	case "concat":
		if len(args) > 0 {
			if _, isList := args[0].([]any); isList {
				var out []any
				for _, a := range args {
					l, ok := a.([]any)
					if !ok {
						return nil, fmt.Errorf("concat of a list and %T", a)
					}
					out = append(out, l...)
				}
				return out, nil
			}
		}
		var sb strings.Builder
		for i := range args {
			sb.WriteString(str(i))
		}
		return sb.String(), nil
	case "union":
		out := map[string]any{}
		for _, a := range args {
			m, ok := a.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("union of %T", a)
			}
			for k, v := range m {
				out[k] = v
			}
		}
		return out, nil
	case "add":
		return num(0) + num(1), nil
	case "sub":
		return num(0) - num(1), nil
	case "json":
		var v any
		if err := json.Unmarshal([]byte(str(0)), &v); err != nil {
			return nil, fmt.Errorf("json(%q): %w", str(0), err)
		}
		return v, nil
	case "format":
		s := str(0)
		for i := 1; i < len(args); i++ {
			s = strings.ReplaceAll(s, "{"+strconv.Itoa(i-1)+"}", str(i))
		}
		return s, nil
	case "empty":
		switch a := args[0].(type) {
		case nil:
			return true, nil
		case string:
			return a == "", nil
		case []any:
			return len(a) == 0, nil
		case map[string]any:
			return len(a) == 0, nil
		case opaque:
			return false, nil
		}
		return false, nil
	case "equals":
		return fmt.Sprint(args[0]) == fmt.Sprint(args[1]), nil
	case "not":
		return args[0] != true, nil
	case "and":
		return args[0] == true && args[1] == true, nil
	case "or":
		return args[0] == true || args[1] == true, nil
	case "string":
		return str(0), nil
	case "int":
		f, err := strconv.ParseFloat(str(0), 64)
		return math.Trunc(f), err
	case "toLower":
		return strings.ToLower(str(0)), nil
	case "replace":
		return strings.ReplaceAll(str(0), str(1), str(2)), nil
	case "take":
		s := str(0)
		if k := int(num(1)); k < len(s) {
			s = s[:k]
		}
		return s, nil
	case "skip":
		s := str(0)
		k := min(max(int(num(1)), 0), len(s))
		return s[k:], nil
	case "startsWith":
		return strings.HasPrefix(str(0), str(1)), nil
	case "first", "last":
		switch a := args[0].(type) {
		case string:
			if a == "" {
				return "", nil
			}
			if n.name == "first" {
				return a[:1], nil
			}
			return a[len(a)-1:], nil
		case []any:
			if len(a) == 0 {
				return nil, nil
			}
			if n.name == "first" {
				return a[0], nil
			}
			return a[len(a)-1], nil
		}
		return nil, fmt.Errorf("%s of %T", n.name, args[0])
	case "contains":
		switch a := args[0].(type) {
		case string:
			return strings.Contains(a, str(1)), nil
		case []any:
			for _, x := range a {
				if fmt.Sprint(x) == fmt.Sprint(args[1]) {
					return true, nil
				}
			}
			return false, nil
		case map[string]any:
			_, ok := a[str(1)]
			return ok, nil
		}
		return nil, fmt.Errorf("contains of %T", args[0])
	case "join":
		l, _ := args[0].([]any)
		parts := make([]string, len(l))
		for i, x := range l {
			parts[i] = fmt.Sprint(x)
		}
		return strings.Join(parts, str(1)), nil
	case "split":
		var out []any
		for _, s := range strings.Split(str(0), str(1)) {
			out = append(out, s)
		}
		return out, nil
	case "length":
		switch a := args[0].(type) {
		case string:
			return float64(len(a)), nil
		case []any:
			return float64(len(a)), nil
		case map[string]any:
			return float64(len(a)), nil
		}
		return nil, fmt.Errorf("length of %T", args[0])
	case "null":
		return nil, nil
	case "base64":
		return "base64:" + str(0), nil
	case "resourceGroup":
		return map[string]any{"location": "eastasia", "id": "/subscriptions/0/resourceGroups/rg", "name": "rg"}, nil
	case "reference", "listKeys", "listServiceSas", "listAccountSas", "resourceId", "uniqueString",
		"utcNow", "dateTimeAdd", "subscription", "guid", "deployment":
		return opaque{n.name}, nil
	}
	return nil, fmt.Errorf("acirules: ARM function %s() not supported (extend deploy/azure/test/acirules)", n.name)
}
