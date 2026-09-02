// SPDX-License-Identifier: BSD-3-Clause
//
// Copyright (c) 2026, the configuration-management-tool/cmt authors

package orchestrate

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	remoteexec "github.com/go-remoteexec/transport"

	"github.com/configuration-management-tool/cmt/manifest"
)

// call records one Exec or Put invocation against fakeRegistry, for
// assertions.
type call struct {
	Host  string
	Cmd   string // the full rendered command line (Exec) or "src -> dst" (Put)
	Stdin string
}

// fakeRegistry is an in-memory, concurrency-safe double for everything
// orchestrate needs from package connect: a DialFunc (via dial) and the
// remoteexec.Connection it hands back. It never touches the network.
type fakeRegistry struct {
	mu sync.Mutex

	execs []call
	puts  []call

	dialCount map[string]int
	closes    int

	dialErrHosts map[string]bool
	execErrHosts map[string]bool
	putErrHosts  map[string]bool
	execRC       map[string]int // host -> exit code (default 0)

	concurrent    int
	maxConcurrent int
	execDelay     time.Duration
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{
		dialCount:    map[string]int{},
		dialErrHosts: map[string]bool{},
		execErrHosts: map[string]bool{},
		putErrHosts:  map[string]bool{},
		execRC:       map[string]int{},
	}
}

func (r *fakeRegistry) dial(_ context.Context, hostSpec string, _ manifest.HostsGroup) (remoteexec.Connection, error) {
	r.mu.Lock()
	r.dialCount[hostSpec]++
	err := r.dialErrHosts[hostSpec]
	r.mu.Unlock()
	if err {
		return nil, fmt.Errorf("fake dial error for %s", hostSpec)
	}
	return &fakeConn{host: hostSpec, reg: r}, nil
}

func (r *fakeRegistry) execsFor(host string) []call {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []call
	for _, c := range r.execs {
		if c.Host == host {
			out = append(out, c)
		}
	}
	return out
}

type fakeConn struct {
	host string
	reg  *fakeRegistry
}

func (c *fakeConn) Exec(_ context.Context, cmd string, stdin io.Reader) (remoteexec.Result, error) {
	var sb strings.Builder
	if stdin != nil {
		io.Copy(&sb, stdin)
	}

	r := c.reg
	r.mu.Lock()
	r.concurrent++
	if r.concurrent > r.maxConcurrent {
		r.maxConcurrent = r.concurrent
	}
	delay := r.execDelay
	r.mu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}

	r.mu.Lock()
	r.concurrent--
	r.execs = append(r.execs, call{Host: c.host, Cmd: cmd, Stdin: sb.String()})
	execErr := r.execErrHosts[c.host]
	rc := r.execRC[c.host]
	r.mu.Unlock()

	if execErr {
		return remoteexec.Result{}, fmt.Errorf("fake exec error on %s", c.host)
	}
	return remoteexec.Result{RC: rc, Stdout: "out:" + c.host + "\n", Stderr: ""}, nil
}

func (c *fakeConn) Put(_ context.Context, src, dst string, _ remoteexec.PutOptions) error {
	r := c.reg
	r.mu.Lock()
	r.puts = append(r.puts, call{Host: c.host, Cmd: src + " -> " + dst})
	err := r.putErrHosts[c.host]
	r.mu.Unlock()
	if err {
		return fmt.Errorf("fake put error on %s", c.host)
	}
	return nil
}

func (c *fakeConn) Fetch(context.Context, string, string) error { return nil }
func (c *fakeConn) Remove(context.Context, string) error        { return nil }
func (c *fakeConn) TempPath(base string) string                 { return "/tmp/" + base }
func (c *fakeConn) Close() error {
	c.reg.mu.Lock()
	c.reg.closes++
	c.reg.mu.Unlock()
	return nil
}

func testManifest(t *testing.T, extra string) *manifest.Manifest {
	t.Helper()
	src := `
env {
  GLOBAL = "g"
}

hosts_group "g" {
  hosts = ["h1", "h2", "h3"]
  env = { GROUP = "gg" }
}

hosts_group "one" {
  hosts = ["only-host"]
}

command "run1" {
  run = "do-it"
}
` + extra
	m, err := manifest.Parse([]byte(src), "test.hcl")
	if err != nil {
		t.Fatalf("manifest.Parse: %v", err)
	}
	return m
}

func TestRunBasic(t *testing.T) {
	m := testManifest(t, "")
	reg := newFakeRegistry()
	r := &Runner{Manifest: m, Dial: reg.dial}

	var stdout bytes.Buffer
	results, err := r.Run(context.Background(), "g", []string{"run1"}, Options{Stdout: &stdout})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("len(results) = %d, want 3", len(results))
	}
	for _, h := range []string{"h1", "h2", "h3"} {
		if len(reg.execsFor(h)) != 1 {
			t.Errorf("host %s: exec count = %d, want 1", h, len(reg.execsFor(h)))
		}
	}
	if reg.closes != 3 {
		t.Errorf("closes = %d, want 3", reg.closes)
	}
	out := stdout.String()
	for _, h := range []string{"h1", "h2", "h3"} {
		if !strings.Contains(out, fmt.Sprintf("[%s] out:%s", h, h)) {
			t.Errorf("stdout missing prefixed output for %s: %q", h, out)
		}
	}
}

func TestRunUnknownHostsGroup(t *testing.T) {
	m := testManifest(t, "")
	reg := newFakeRegistry()
	r := &Runner{Manifest: m, Dial: reg.dial}
	if _, err := r.Run(context.Background(), "nope", []string{"run1"}, Options{}); err == nil {
		t.Fatal("expected an error for an unknown hosts_group")
	}
}

func TestRunNilDial(t *testing.T) {
	m := testManifest(t, "")
	r := &Runner{Manifest: m}
	if _, err := r.Run(context.Background(), "g", []string{"run1"}, Options{}); err == nil {
		t.Fatal("expected an error when Dial is nil")
	}
}

func TestRunUnknownCommand(t *testing.T) {
	m := testManifest(t, "")
	reg := newFakeRegistry()
	r := &Runner{Manifest: m, Dial: reg.dial}
	if _, err := r.Run(context.Background(), "g", []string{"nope"}, Options{}); err == nil {
		t.Fatal("expected an error for an unknown command/target")
	}
}

func TestRunTargetExpansion(t *testing.T) {
	m := testManifest(t, `
command "run2" {
  run = "do-it-2"
}
target "t" {
  commands = ["run1", "run2"]
}
`)
	reg := newFakeRegistry()
	r := &Runner{Manifest: m, Dial: reg.dial}
	if _, err := r.Run(context.Background(), "g", []string{"t"}, Options{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, h := range []string{"h1", "h2", "h3"} {
		calls := reg.execsFor(h)
		if len(calls) != 2 {
			t.Fatalf("host %s: exec count = %d, want 2", h, len(calls))
		}
		if !strings.HasSuffix(calls[0].Cmd, "do-it") || !strings.HasSuffix(calls[1].Cmd, "do-it-2") {
			t.Errorf("host %s: unexpected command order: %#v", h, calls)
		}
	}
}

func TestRunOnce(t *testing.T) {
	m := testManifest(t, `
command "once1" {
  run  = "do-it"
  once = true
}
`)
	reg := newFakeRegistry()
	r := &Runner{Manifest: m, Dial: reg.dial}
	results, err := r.Run(context.Background(), "g", []string{"once1"}, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if results[0].Host != "h1" {
		t.Errorf("once should run on the first resolved host, got %q", results[0].Host)
	}
	if len(reg.execsFor("h2")) != 0 || len(reg.execsFor("h3")) != 0 {
		t.Errorf("once should not run on h2/h3")
	}
}

func TestRunSerialBounded(t *testing.T) {
	m := testManifest(t, `
command "serial2" {
  run    = "do-it"
  serial = 2
}
`)
	reg := newFakeRegistry()
	reg.execDelay = 20 * time.Millisecond
	r := &Runner{Manifest: m, Dial: reg.dial}
	if _, err := r.Run(context.Background(), "g", []string{"serial2"}, Options{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	reg.mu.Lock()
	max := reg.maxConcurrent
	reg.mu.Unlock()
	if max > 2 {
		t.Errorf("maxConcurrent = %d, want <= 2", max)
	}
	if max < 2 {
		t.Errorf("maxConcurrent = %d, want == 2 (serial should allow 2 in flight)", max)
	}
}

func TestRunUnlimitedConcurrency(t *testing.T) {
	m := testManifest(t, "")
	reg := newFakeRegistry()
	reg.execDelay = 20 * time.Millisecond
	r := &Runner{Manifest: m, Dial: reg.dial}
	if _, err := r.Run(context.Background(), "g", []string{"run1"}, Options{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	reg.mu.Lock()
	max := reg.maxConcurrent
	reg.mu.Unlock()
	if max != 3 {
		t.Errorf("maxConcurrent = %d, want 3 (unlimited fan-out over 3 hosts)", max)
	}
}

func TestRunTargetFailFast(t *testing.T) {
	m := testManifest(t, `
command "run2" {
  run = "do-it-2"
}
target "t" {
  commands = ["run1", "run2"]
}
`)
	reg := newFakeRegistry()
	reg.execRC["h2"] = 1 // run1 fails on h2
	r := &Runner{Manifest: m, Dial: reg.dial}
	_, err := r.Run(context.Background(), "g", []string{"t"}, Options{})
	if err == nil {
		t.Fatal("expected an error from the failing host")
	}
	if !strings.Contains(err.Error(), `command "run1" failed on host "h2"`) {
		t.Errorf("error = %q", err.Error())
	}
	for _, h := range []string{"h1", "h2", "h3"} {
		for _, c := range reg.execsFor(h) {
			if strings.Contains(c.Cmd, "do-it-2") {
				t.Errorf("run2 should not have run after run1 failed, but host %s ran %q", h, c.Cmd)
			}
		}
	}
}

func TestRunExecTransportError(t *testing.T) {
	m := testManifest(t, "")
	reg := newFakeRegistry()
	reg.execErrHosts["h2"] = true
	r := &Runner{Manifest: m, Dial: reg.dial}
	_, err := r.Run(context.Background(), "g", []string{"run1"}, Options{})
	if err == nil || !strings.Contains(err.Error(), "fake exec error on h2") {
		t.Errorf("error = %v, want to mention the transport error", err)
	}
}

func TestRunDialError(t *testing.T) {
	m := testManifest(t, "")
	reg := newFakeRegistry()
	reg.dialErrHosts["h1"] = true
	r := &Runner{Manifest: m, Dial: reg.dial}
	_, err := r.Run(context.Background(), "g", []string{"run1"}, Options{})
	if err == nil || !strings.Contains(err.Error(), "fake dial error for h1") {
		t.Errorf("error = %v", err)
	}
}

func TestRunUploadCommand(t *testing.T) {
	m := testManifest(t, `
command "up" {
  upload {
    src        = "./dist"
    dst        = "/tmp/dist"
    executable = true
  }
}
`)
	reg := newFakeRegistry()
	var stdout bytes.Buffer
	r := &Runner{Manifest: m, Dial: reg.dial}
	results, err := r.Run(context.Background(), "g", []string{"up"}, Options{Stdout: &stdout})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("len(results) = %d, want 3", len(results))
	}
	for _, r := range results {
		if r.Kind != "upload" {
			t.Errorf("Kind = %q, want upload", r.Kind)
		}
	}
	for _, h := range []string{"h1", "h2", "h3"} {
		calls := reg.puts
		found := false
		for _, c := range calls {
			if c.Host == h && c.Cmd == "./dist -> /tmp/dist" {
				found = true
			}
		}
		if !found {
			t.Errorf("missing Put call for host %s", h)
		}
	}
	if !strings.Contains(stdout.String(), "uploaded ./dist to /tmp/dist") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestRunUploadFailure(t *testing.T) {
	m := testManifest(t, `
command "up" {
  upload {
    src = "./dist"
    dst = "/tmp/dist"
  }
}
`)
	reg := newFakeRegistry()
	reg.putErrHosts["h1"] = true
	r := &Runner{Manifest: m, Dial: reg.dial}
	_, err := r.Run(context.Background(), "g", []string{"up"}, Options{})
	if err == nil || !strings.Contains(err.Error(), "fake put error on h1") {
		t.Errorf("error = %v", err)
	}
}

func TestRunUploadDialError(t *testing.T) {
	m := testManifest(t, `
command "up" {
  upload {
    src = "./dist"
    dst = "/tmp/dist"
  }
}
`)
	reg := newFakeRegistry()
	reg.dialErrHosts["h1"] = true
	r := &Runner{Manifest: m, Dial: reg.dial}
	_, err := r.Run(context.Background(), "g", []string{"up"}, Options{})
	if err == nil || !strings.Contains(err.Error(), "fake dial error for h1") {
		t.Errorf("error = %v", err)
	}
}

func TestRunLocalCommand(t *testing.T) {
	m := testManifest(t, `
command "loc" {
  local = "npm run build"
}
`)
	reg := newFakeRegistry()
	r := &Runner{Manifest: m, Dial: reg.dial}
	results, err := r.Run(context.Background(), "g", []string{"loc"}, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1 (local runs once)", len(results))
	}
	if results[0].Host != LocalHost || results[0].Kind != "local" {
		t.Errorf("unexpected local result: %#v", results[0])
	}
	calls := reg.execsFor(LocalHost)
	if len(calls) != 1 || !strings.HasSuffix(calls[0].Cmd, "npm run build") {
		t.Errorf("unexpected local exec calls: %#v", calls)
	}
	if strings.Count(calls[0].Cmd, "CMT_HOST=") == 0 {
		t.Errorf("expected builtin env vars in local command line: %q", calls[0].Cmd)
	}
}

func TestRunEnvLayering(t *testing.T) {
	m := testManifest(t, "")
	reg := newFakeRegistry()
	r := &Runner{Manifest: m, Dial: reg.dial}
	opts := Options{EnvOverrides: map[string]string{"GROUP": "overridden", "EXTRA": "e"}}
	if _, err := r.Run(context.Background(), "g", []string{"run1"}, opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	calls := reg.execsFor("h1")
	if len(calls) != 1 {
		t.Fatalf("expected 1 call")
	}
	cmd := calls[0].Cmd
	for _, want := range []string{
		"GLOBAL='g'",
		"GROUP='overridden'", // opts override wins over the group's own env
		"EXTRA='e'",
		"CMT_HOSTS_GROUP='g'",
		"CMT_HOST='h1'",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("rendered command %q missing %q", cmd, want)
		}
	}
	if !strings.HasSuffix(cmd, "do-it") {
		t.Errorf("rendered command %q should end with the raw command", cmd)
	}
}

func TestRunHostUser(t *testing.T) {
	m, err := manifest.Parse([]byte(`
hosts_group "g" {
  hosts = ["deploy@h1"]
}
command "run1" {
  run = "do-it"
}
`), "test.hcl")
	if err != nil {
		t.Fatalf("manifest.Parse: %v", err)
	}
	reg := newFakeRegistry()
	r := &Runner{Manifest: m, Dial: reg.dial}
	if _, err := r.Run(context.Background(), "g", []string{"run1"}, Options{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	calls := reg.execsFor("deploy@h1")
	if len(calls) != 1 || !strings.Contains(calls[0].Cmd, "CMT_USER='deploy'") {
		t.Errorf("expected CMT_USER=deploy in %#v", calls)
	}
}

func TestRunHostFiltering(t *testing.T) {
	m := testManifest(t, "")
	reg := newFakeRegistry()
	r := &Runner{Manifest: m, Dial: reg.dial}

	results, err := r.Run(context.Background(), "g", []string{"run1"}, Options{Only: regexp.MustCompile(`^h[12]$`)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("Only: len(results) = %d, want 2", len(results))
	}
	if len(reg.execsFor("h3")) != 0 {
		t.Errorf("Only should have excluded h3")
	}

	reg2 := newFakeRegistry()
	r2 := &Runner{Manifest: m, Dial: reg2.dial}
	results, err = r2.Run(context.Background(), "g", []string{"run1"}, Options{Except: regexp.MustCompile(`^h1$`)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("Except: len(results) = %d, want 2", len(results))
	}
	if len(reg2.execsFor("h1")) != 0 {
		t.Errorf("Except should have excluded h1")
	}
}

func TestRunAllHostsFilteredOut(t *testing.T) {
	m := testManifest(t, "")
	reg := newFakeRegistry()
	r := &Runner{Manifest: m, Dial: reg.dial}
	results, err := r.Run(context.Background(), "g", []string{"run1"}, Options{Only: regexp.MustCompile(`^nomatch$`)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("len(results) = %d, want 0", len(results))
	}
}

func TestRunStdinPiping(t *testing.T) {
	m := testManifest(t, "")
	reg := newFakeRegistry()
	r := &Runner{Manifest: m, Dial: reg.dial}
	opts := Options{StdinData: []byte("piped-input")}
	if _, err := r.Run(context.Background(), "g", []string{"run1"}, opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, h := range []string{"h1", "h2", "h3"} {
		calls := reg.execsFor(h)
		if len(calls) != 1 || calls[0].Stdin != "piped-input" {
			t.Errorf("host %s: expected fresh stdin copy, got %#v", h, calls)
		}
	}
}

func TestRunDisablePrefix(t *testing.T) {
	m, err := manifest.Parse([]byte(`
hosts_group "g" { hosts = ["h1"] }
command "run1" { run = "do-it" }
`), "test.hcl")
	if err != nil {
		t.Fatalf("manifest.Parse: %v", err)
	}
	reg := newFakeRegistry()
	r := &Runner{Manifest: m, Dial: reg.dial}
	var stdout bytes.Buffer
	if _, err := r.Run(context.Background(), "g", []string{"run1"}, Options{Stdout: &stdout, DisablePrefix: true}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(stdout.String(), "[h1]") {
		t.Errorf("expected no prefix, got %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "out:h1") {
		t.Errorf("expected raw output, got %q", stdout.String())
	}
}

func TestRunDynamicInventory(t *testing.T) {
	m, err := manifest.Parse([]byte(`
hosts_group "dyn" {
  inventory = "list-hosts"
}
command "run1" { run = "do-it" }
`), "test.hcl")
	if err != nil {
		t.Fatalf("manifest.Parse: %v", err)
	}
	reg := newFakeRegistry()
	r := &Runner{
		Manifest: m,
		Dial:     reg.dial,
		Inventory: func(_ context.Context, cmd string) ([]string, error) {
			if cmd != "list-hosts" {
				t.Errorf("Inventory called with %q", cmd)
			}
			return []string{"dyn1", "dyn2"}, nil
		},
	}
	results, err := r.Run(context.Background(), "dyn", []string{"run1"}, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	if len(reg.execsFor("dyn1")) != 1 || len(reg.execsFor("dyn2")) != 1 {
		t.Errorf("expected execs on dyn1/dyn2: %#v", reg.execs)
	}
}

func TestRunDynamicInventoryError(t *testing.T) {
	m, err := manifest.Parse([]byte(`
hosts_group "dyn" {
  inventory = "list-hosts"
}
command "run1" { run = "do-it" }
`), "test.hcl")
	if err != nil {
		t.Fatalf("manifest.Parse: %v", err)
	}
	reg := newFakeRegistry()
	r := &Runner{
		Manifest:  m,
		Dial:      reg.dial,
		Inventory: func(context.Context, string) ([]string, error) { return nil, fmt.Errorf("inventory boom") },
	}
	if _, err := r.Run(context.Background(), "dyn", []string{"run1"}, Options{}); err == nil || !strings.Contains(err.Error(), "inventory boom") {
		t.Errorf("error = %v", err)
	}
}

// TestRunLocalInventoryDefault is the one real, non-mocked integration
// test for the default inventory path: it exercises RunLocalInventory
// end to end through transport.NewLocal() (no network, no fake).
func TestRunLocalInventoryDefault(t *testing.T) {
	hosts, err := RunLocalInventory(context.Background(), "printf 'a\\n\\nb\\n'")
	if err != nil {
		t.Fatalf("RunLocalInventory: %v", err)
	}
	if len(hosts) != 2 || hosts[0] != "a" || hosts[1] != "b" {
		t.Errorf("hosts = %#v, want [a b] (blank lines skipped)", hosts)
	}
}

// TestRunLocalInventoryTransportError substitutes localInventoryExec to
// force the transport-error branch (Err != nil, as opposed to a
// nonzero-exit Result) — not reachable through a real local shell
// portably.
func TestRunLocalInventoryTransportError(t *testing.T) {
	orig := localInventoryExec
	t.Cleanup(func() { localInventoryExec = orig })
	localInventoryExec = func(context.Context, string) (remoteexec.Result, error) {
		return remoteexec.Result{}, fmt.Errorf("boom")
	}
	if _, err := RunLocalInventory(context.Background(), "irrelevant"); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("error = %v", err)
	}
}

func TestRunLocalInventoryNonZeroExit(t *testing.T) {
	if _, err := RunLocalInventory(context.Background(), "exit 3"); err == nil {
		t.Fatal("expected an error for a non-zero exit inventory command")
	}
}

func TestRunUsesDefaultInventoryWhenUnset(t *testing.T) {
	m, err := manifest.Parse([]byte(`
hosts_group "dyn" {
  inventory = "printf 'localonly\n'"
}
command "run1" { run = "do-it" }
`), "test.hcl")
	if err != nil {
		t.Fatalf("manifest.Parse: %v", err)
	}
	reg := newFakeRegistry()
	r := &Runner{Manifest: m, Dial: reg.dial} // Inventory left nil: exercises RunLocalInventory as default
	if _, err := r.Run(context.Background(), "dyn", []string{"run1"}, Options{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(reg.execsFor("localonly")) != 1 {
		t.Errorf("expected the default local inventory to resolve to 'localonly': %#v", reg.execs)
	}
}

func TestHostResultFailed(t *testing.T) {
	tests := []struct {
		name string
		r    HostResult
		want bool
	}{
		{"transport error", HostResult{Err: fmt.Errorf("boom")}, true},
		{"nonzero rc run", HostResult{Kind: "run", Out: remoteexec.Result{RC: 1}}, true},
		{"zero rc run", HostResult{Kind: "run", Out: remoteexec.Result{RC: 0}}, false},
		{"nonzero rc upload ignored", HostResult{Kind: "upload", Out: remoteexec.Result{RC: 1}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.r.Failed(); got != tt.want {
				t.Errorf("Failed() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSelectHosts(t *testing.T) {
	hosts := []string{"a", "b", "c"}
	if got := selectHosts(hosts, false); len(got) != 3 {
		t.Errorf("selectHosts(false) = %#v", got)
	}
	if got := selectHosts(hosts, true); len(got) != 1 || got[0] != "a" {
		t.Errorf("selectHosts(true) = %#v", got)
	}
	if got := selectHosts([]string{"a"}, true); len(got) != 1 {
		t.Errorf("selectHosts(true) on a single host = %#v", got)
	}
	if got := selectHosts(nil, true); len(got) != 0 {
		t.Errorf("selectHosts(true) on no hosts = %#v", got)
	}
}

func TestMergeEnv(t *testing.T) {
	got := mergeEnv(map[string]string{"A": "1", "B": "1"}, map[string]string{"B": "2"}, map[string]string{"C": "3"})
	want := map[string]string{"A": "1", "B": "2", "C": "3"}
	if len(got) != len(want) {
		t.Fatalf("mergeEnv = %#v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("mergeEnv[%q] = %q, want %q", k, got[k], v)
		}
	}
}

func TestRenderEnv(t *testing.T) {
	if got := renderEnv(nil); got != "" {
		t.Errorf("renderEnv(nil) = %q, want empty", got)
	}
	got := renderEnv(map[string]string{"B": "b", "A": "a's"})
	want := `A='a'\''s' B='b' `
	if got != want {
		t.Errorf("renderEnv = %q, want %q", got, want)
	}
}

func TestParseHostUser(t *testing.T) {
	if got := parseHostUser("user@host"); got != "user" {
		t.Errorf("parseHostUser = %q, want user", got)
	}
	if got := parseHostUser("host"); got != "" {
		t.Errorf("parseHostUser = %q, want empty", got)
	}
}

func TestWritePrefixed(t *testing.T) {
	var buf bytes.Buffer
	writePrefixed(&buf, "h", "", false)
	if buf.Len() != 0 {
		t.Errorf("expected no output for empty text, got %q", buf.String())
	}

	buf.Reset()
	writePrefixed(&buf, "h", "line1\nline2\n", false)
	if buf.String() != "[h] line1\n[h] line2\n" {
		t.Errorf("got %q", buf.String())
	}

	buf.Reset()
	writePrefixed(&buf, "h", "raw\n", true)
	if buf.String() != "raw\n" {
		t.Errorf("got %q", buf.String())
	}
}

func TestFailureMessage(t *testing.T) {
	if got := failureMessage(HostResult{Err: fmt.Errorf("boom")}); got != "boom" {
		t.Errorf("failureMessage = %q", got)
	}
	if got := failureMessage(HostResult{Out: remoteexec.Result{RC: 7}}); got != "exit code 7" {
		t.Errorf("failureMessage = %q", got)
	}
}
