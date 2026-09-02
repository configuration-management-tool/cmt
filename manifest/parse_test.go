// SPDX-License-Identifier: BSD-3-Clause
//
// Copyright (c) 2026, the configuration-management-tool/cmt authors

package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseFullExample(t *testing.T) {
	src := `
env {
  NAME  = "api"
  IMAGE = "example/api"
}

hosts_group "production" {
  hosts = ["api1.example.com", "api2.example.com"]
  env = { DEPLOY_ENV = "prod" }

  ssh {
    user            = "deploy"
    port            = 2222
    password        = "s3cret"
    private-key     = "/home/deploy/.ssh/id_ed25519"
    passphrase      = "phrase"
    host-key-check  = true
    known-hosts     = "/home/deploy/.ssh/known_hosts"
    tty             = true
    tmpdir          = "/var/tmp"
    connect-timeout = 15
  }

  become {
    method   = "sudo"
    user     = "root"
    password = "rootpw"
  }
}

hosts_group "windows" {
  hosts = ["win1.example.com"]

  winrm {
    user            = "Administrator"
    port            = 5986
    password        = "winpw"
    ssl             = true
    ssl-verify      = true
    ca-cert         = "/etc/ssl/ca.pem"
    transport       = "negotiate"
    client-cert     = "/etc/ssl/client.pem"
    client-key      = "/etc/ssl/client.key"
    connect-timeout = 30
    tmpdir          = "C:\\Windows\\Temp"
    path            = "/wsman"
  }
}

hosts_group "staging" {
  inventory = "curl http://example.com/latest/meta-data/hostname"
}

command "restart" {
  desc   = "Restart example container"
  run    = "sudo docker restart example"
  serial = 2
}

command "build" {
  desc = "Build image"
  run  = "sudo docker build -t image:latest ."
  once = true
}

command "prepare" {
  desc  = "Prepare to upload"
  local = "npm run build"
}

command "upload" {
  desc = "Upload dist files"
  upload {
    src        = "./dist"
    dst        = "/tmp/"
    executable = true
  }
}

target "deploy" {
  commands = ["build", "prepare", "upload", "restart"]
}
`
	m, err := Parse([]byte(src), "test.hcl")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if m.Env["NAME"] != "api" || m.Env["IMAGE"] != "example/api" {
		t.Errorf("Env = %#v", m.Env)
	}

	prod, ok := m.HostsGroups["production"]
	if !ok {
		t.Fatal("missing hosts_group production")
	}
	if len(prod.Hosts) != 2 || prod.Hosts[0] != "api1.example.com" {
		t.Errorf("production.Hosts = %#v", prod.Hosts)
	}
	if prod.Env["DEPLOY_ENV"] != "prod" {
		t.Errorf("production.Env = %#v", prod.Env)
	}
	if prod.SSH == nil {
		t.Fatal("production.SSH is nil")
	}
	if prod.SSH.User != "deploy" || prod.SSH.Port != 2222 || prod.SSH.Password != "s3cret" ||
		prod.SSH.PrivateKey != "/home/deploy/.ssh/id_ed25519" || prod.SSH.PrivateKeyPassphrase != "phrase" ||
		!prod.SSH.HostKeyCheck || prod.SSH.KnownHostsFile != "/home/deploy/.ssh/known_hosts" ||
		!prod.SSH.TTY || prod.SSH.TempDir != "/var/tmp" || prod.SSH.ConnectTimeout != 15 {
		t.Errorf("production.SSH = %#v", prod.SSH)
	}
	if prod.Become == nil || prod.Become.Method != "sudo" || prod.Become.User != "root" || prod.Become.Password != "rootpw" {
		t.Errorf("production.Become = %#v", prod.Become)
	}

	win, ok := m.HostsGroups["windows"]
	if !ok {
		t.Fatal("missing hosts_group windows")
	}
	if win.WinRM == nil {
		t.Fatal("windows.WinRM is nil")
	}
	if win.WinRM.User != "Administrator" || win.WinRM.Port != 5986 || win.WinRM.Password != "winpw" ||
		!win.WinRM.SSL || !win.WinRM.SSLVerify || win.WinRM.CACert != "/etc/ssl/ca.pem" ||
		win.WinRM.Transport != "negotiate" || win.WinRM.ClientCert != "/etc/ssl/client.pem" ||
		win.WinRM.ClientKey != "/etc/ssl/client.key" || win.WinRM.ConnectTimeout != 30 ||
		win.WinRM.TempDir != `C:\Windows\Temp` || win.WinRM.Path != "/wsman" {
		t.Errorf("windows.WinRM = %#v", win.WinRM)
	}

	staging, ok := m.HostsGroups["staging"]
	if !ok {
		t.Fatal("missing hosts_group staging")
	}
	if staging.Inventory != "curl http://example.com/latest/meta-data/hostname" {
		t.Errorf("staging.Inventory = %q", staging.Inventory)
	}

	build, ok := m.Commands["build"]
	if !ok || !build.Once || build.Run == "" {
		t.Errorf("build command = %#v", build)
	}
	restart, ok := m.Commands["restart"]
	if !ok || restart.Serial != 2 {
		t.Errorf("restart command = %#v", restart)
	}
	prepare, ok := m.Commands["prepare"]
	if !ok || prepare.Local != "npm run build" {
		t.Errorf("prepare command = %#v", prepare)
	}
	upload, ok := m.Commands["upload"]
	if !ok || upload.Upload == nil || upload.Upload.Src != "./dist" || upload.Upload.Dst != "/tmp/" || !upload.Upload.Executable {
		t.Errorf("upload command = %#v", upload)
	}

	deploy, ok := m.Targets["deploy"]
	if !ok {
		t.Fatal("missing target deploy")
	}
	want := []string{"build", "prepare", "upload", "restart"}
	if len(deploy.Commands) != len(want) {
		t.Fatalf("deploy.Commands = %#v", deploy.Commands)
	}
	for i, c := range want {
		if deploy.Commands[i] != c {
			t.Errorf("deploy.Commands[%d] = %q, want %q", i, deploy.Commands[i], c)
		}
	}
}

func TestParseExampleFile(t *testing.T) {
	path := filepath.Join("..", "example", "cmt.hcl")
	m, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile(%s): %v", path, err)
	}
	if len(m.HostsGroups) == 0 || len(m.Commands) == 0 || len(m.Targets) == 0 {
		t.Errorf("unexpectedly empty manifest: %#v", m)
	}
}

func TestParseFileMissing(t *testing.T) {
	if _, err := ParseFile(filepath.Join(t.TempDir(), "does-not-exist.hcl")); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

func TestParseFileOnDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cmt.hcl")
	src := `
hosts_group "g" { hosts = ["localhost"] }
command "c" { run = "echo hi" }
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if _, ok := m.HostsGroups["g"]; !ok {
		t.Errorf("missing hosts_group g")
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string // substring expected in the error
	}{
		{
			name: "malformed hcl",
			src:  `hosts_group "g" {`,
			want: "manifest:",
		},
		{
			name: "unknown top-level block",
			src:  `foo "x" {}`,
			want: `unknown top-level block "foo"`,
		},
		{
			name: "env block with label",
			src:  `env "x" {}`,
			want: "env block takes no label",
		},
		{
			name: "env value not a string",
			src: `env {
  FOO = [1]
}`,
			want: `env value "FOO" must be a string`,
		},
		{
			name: "env value null",
			src: `env {
  FOO = null
}`,
			want: `env value "FOO" must not be null`,
		},
		{
			name: "env value undefined variable",
			src: `env {
  FOO = undefinedvar
}`,
			want: "env block:",
		},
		{
			name: "env block unknown nested block",
			src: `env {
  nope {}
}`,
			want: `unexpected block "nope"`,
		},

		{
			name: "hosts_group missing label",
			src:  `hosts_group { hosts = ["h"] }`,
			want: "requires exactly one non-empty label",
		},
		{
			name: "hosts_group empty label",
			src:  `hosts_group "" { hosts = ["h"] }`,
			want: "requires exactly one non-empty label",
		},
		{
			name: "hosts_group neither hosts nor inventory",
			src:  `hosts_group "g" {}`,
			want: `must set either "hosts" or "inventory"`,
		},
		{
			name: "hosts_group both hosts and inventory",
			src: `hosts_group "g" {
  hosts     = ["h"]
  inventory = "echo h"
}`,
			want: `mutually exclusive`,
		},
		{
			name: "hosts_group empty hosts list",
			src:  `hosts_group "g" { hosts = [] }`,
			want: `"hosts" must not be empty`,
		},
		{
			name: "hosts_group hosts undefined var",
			src:  `hosts_group "g" { hosts = undefinedvar }`,
			want: `hosts_group "g":`,
		},
		{
			name: "hosts_group hosts not a list",
			src:  `hosts_group "g" { hosts = "not-a-list" }`,
			want: `"hosts" must be a list of strings`,
		},
		{
			name: "hosts_group hosts element not a string",
			src:  `hosts_group "g" { hosts = [["nested"]] }`,
			want: `"hosts" must be a list of strings`,
		},
		{
			name: "hosts_group hosts null",
			src:  `hosts_group "g" { hosts = null }`,
			want: `"hosts" must not be null`,
		},
		{
			name: "hosts_group env not a map",
			src: `hosts_group "g" {
  hosts = ["h"]
  env   = "nope"
}`,
			want: `"env" must be a map`,
		},
		{
			name: "hosts_group env entry not a string",
			src: `hosts_group "g" {
  hosts = ["h"]
  env   = { K = [1] }
}`,
			want: `"env".K must be a string`,
		},
		{
			name: "hosts_group env undefined variable",
			src: `hosts_group "g" {
  hosts = ["h"]
  env   = undefinedvar
}`,
			want: `hosts_group "g":`,
		},
		{
			name: "hosts_group env null",
			src: `hosts_group "g" {
  hosts = ["h"]
  env   = null
}`,
			want: `"env" must not be null`,
		},
		{
			name: "hosts_group unknown attribute",
			src: `hosts_group "g" {
  hosts = ["h"]
  nope  = "x"
}`,
			want: `unexpected attribute "nope"`,
		},
		{
			name: "hosts_group unknown block",
			src: `hosts_group "g" {
  hosts = ["h"]
  nope {}
}`,
			want: `unexpected block "nope"`,
		},
		{
			name: "hosts_group duplicate ssh block",
			src: `hosts_group "g" {
  hosts = ["h"]
  ssh {}
  ssh {}
}`,
			want: `duplicate "ssh" block`,
		},
		{
			name: "hosts_group duplicate winrm block",
			src: `hosts_group "g" {
  hosts = ["h"]
  winrm {}
  winrm {}
}`,
			want: `duplicate "winrm" block`,
		},
		{
			name: "hosts_group duplicate become block",
			src: `hosts_group "g" {
  hosts = ["h"]
  become {}
  become {}
}`,
			want: `duplicate "become" block`,
		},
		{
			name: "hosts_group ssh block labeled",
			src: `hosts_group "g" {
  hosts = ["h"]
  ssh "x" {}
}`,
			want: `"ssh" block takes no label`,
		},
		{
			name: "hosts_group ssh block bad attribute",
			src: `hosts_group "g" {
  hosts = ["h"]
  ssh { user = undefinedvar }
}`,
			want: "ssh block:",
		},
		{
			name: "hosts_group ssh block unknown attribute",
			src: `hosts_group "g" {
  hosts = ["h"]
  ssh { nope = "x" }
}`,
			want: `unexpected attribute "nope"`,
		},
		{
			name: "hosts_group ssh block unknown nested block",
			src: `hosts_group "g" {
  hosts = ["h"]
  ssh {
    nope {}
  }
}`,
			want: `unexpected block "nope"`,
		},
		{
			name: "hosts_group winrm block bad attribute",
			src: `hosts_group "g" {
  hosts = ["h"]
  winrm { user = undefinedvar }
}`,
			want: "winrm block:",
		},
		{
			name: "hosts_group winrm block unknown attribute",
			src: `hosts_group "g" {
  hosts = ["h"]
  winrm { nope = "x" }
}`,
			want: `unexpected attribute "nope"`,
		},
		{
			name: "hosts_group winrm block unknown nested block",
			src: `hosts_group "g" {
  hosts = ["h"]
  winrm {
    nope {}
  }
}`,
			want: `unexpected block "nope"`,
		},
		{
			name: "hosts_group become block bad attribute",
			src: `hosts_group "g" {
  hosts = ["h"]
  become { user = undefinedvar }
}`,
			want: "become block:",
		},
		{
			name: "hosts_group become block unknown attribute",
			src: `hosts_group "g" {
  hosts = ["h"]
  become { nope = "x" }
}`,
			want: `unexpected attribute "nope"`,
		},
		{
			name: "hosts_group become block unknown nested block",
			src: `hosts_group "g" {
  hosts = ["h"]
  become {
    nope {}
  }
}`,
			want: `unexpected block "nope"`,
		},
		{
			name: "duplicate hosts_group",
			src: `hosts_group "g" { hosts = ["h"] }
hosts_group "g" { hosts = ["h2"] }`,
			want: `duplicate hosts_group "g"`,
		},

		{
			name: "command missing label",
			src:  `command { run = "echo hi" }`,
			want: "requires exactly one non-empty label",
		},
		{
			name: "command none of run/local/upload",
			src:  `command "c" { desc = "nothing" }`,
			want: `exactly one of "run", "local", or an "upload" block is required`,
		},
		{
			name: "command run and local both set",
			src: `command "c" {
  run   = "echo hi"
  local = "echo hi"
}`,
			want: `mutually exclusive`,
		},
		{
			name: "command bad attribute",
			src:  `command "c" { run = undefinedvar }`,
			want: `command "c":`,
		},
		{
			name: "command desc not a string",
			src: `command "c" {
  run  = "echo hi"
  desc = [1]
}`,
			want: `"desc" must be a string`,
		},
		{
			name: "command desc null",
			src: `command "c" {
  run  = "echo hi"
  desc = null
}`,
			want: `"desc" must not be null`,
		},
		{
			name: "command once undefined variable",
			src: `command "c" {
  run  = "echo hi"
  once = undefinedvar
}`,
			want: `command "c":`,
		},
		{
			name: "command serial undefined variable",
			src: `command "c" {
  run    = "echo hi"
  serial = undefinedvar
}`,
			want: `command "c":`,
		},
		{
			name: "command once not a bool",
			src: `command "c" {
  run  = "echo hi"
  once = [1]
}`,
			want: `"once" must be a bool`,
		},
		{
			name: "command once null",
			src: `command "c" {
  run  = "echo hi"
  once = null
}`,
			want: `"once" must not be null`,
		},
		{
			name: "command serial not a number",
			src: `command "c" {
  run    = "echo hi"
  serial = [1]
}`,
			want: `"serial" must be a number`,
		},
		{
			name: "command serial null",
			src: `command "c" {
  run    = "echo hi"
  serial = null
}`,
			want: `"serial" must not be null`,
		},
		{
			name: "command unknown attribute",
			src: `command "c" {
  run  = "echo hi"
  nope = "x"
}`,
			want: `unexpected attribute "nope"`,
		},
		{
			name: "command unknown block",
			src: `command "c" {
  run = "echo hi"
  nope {}
}`,
			want: `unexpected block "nope"`,
		},
		{
			name: "command local with serial",
			src: `command "c" {
  local  = "echo hi"
  serial = 2
}`,
			want: `"serial"/"once" apply only to "run"/"upload" commands, not "local"`,
		},
		{
			name: "command local with once",
			src: `command "c" {
  local = "echo hi"
  once  = true
}`,
			want: `"serial"/"once" apply only to "run"/"upload" commands, not "local"`,
		},
		{
			name: "command duplicate upload block",
			src: `command "c" {
  upload {
    src = "a"
    dst = "b"
  }
  upload {
    src = "a"
    dst = "b"
  }
}`,
			want: `duplicate "upload" block`,
		},
		{
			name: "command upload missing src",
			src: `command "c" {
  upload { dst = "b" }
}`,
			want: `"src" is required`,
		},
		{
			name: "command upload missing dst",
			src: `command "c" {
  upload { src = "a" }
}`,
			want: `"dst" is required`,
		},
		{
			name: "command upload bad attribute",
			src: `command "c" {
  upload {
    src = undefinedvar
    dst = "b"
  }
}`,
			want: "upload block:",
		},
		{
			name: "command upload unknown attribute",
			src: `command "c" {
  upload {
    src  = "a"
    dst  = "b"
    nope = "x"
  }
}`,
			want: `unexpected attribute "nope"`,
		},
		{
			name: "command upload unknown nested block",
			src: `command "c" {
  upload {
    src = "a"
    dst = "b"
    nope {}
  }
}`,
			want: `unexpected block "nope"`,
		},
		{
			name: "command upload labeled",
			src: `command "c" {
  upload "x" {
    src = "a"
    dst = "b"
  }
}`,
			want: `"upload" block takes no label`,
		},
		{
			name: "duplicate command",
			src: `command "c" { run = "echo hi" }
command "c" { run = "echo bye" }`,
			want: `duplicate command "c"`,
		},

		{
			name: "target missing label",
			src:  `target { commands = ["c"] }`,
			want: "requires exactly one non-empty label",
		},
		{
			name: "target commands undefined variable",
			src: `target "t" {
  commands = undefinedvar
}`,
			want: `target "t":`,
		},
		{
			name: "target missing commands",
			src:  `target "t" {}`,
			want: `"commands" is required`,
		},
		{
			name: "target empty commands",
			src:  `target "t" { commands = [] }`,
			want: `"commands" must not be empty`,
		},
		{
			name: "target unknown attribute",
			src: `target "t" {
  commands = ["c"]
  nope     = "x"
}`,
			want: `unexpected attribute "nope"`,
		},
		{
			name: "target unknown block",
			src: `target "t" {
  commands = ["c"]
  nope {}
}`,
			want: `unexpected block "nope"`,
		},
		{
			name: "duplicate target",
			src: `target "t" { commands = ["c"] }
target "t" { commands = ["c"] }`,
			want: `duplicate target "t"`,
		},
		{
			name: "target references unknown command",
			src: `command "c" { run = "echo hi" }
target "t" { commands = ["nope"] }`,
			want: `target "t" references unknown command "nope"`,
		},
		{
			name: "target references a target, not a command",
			src: `command "c" { run = "echo hi" }
target "t1" { commands = ["c"] }
target "t2" { commands = ["t1"] }`,
			want: `target "t2" references "t1", which is a target, not a command`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.src), "test.hcl")
			if err == nil {
				t.Fatalf("expected an error, got nil")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want substring %q", err.Error(), tt.want)
			}
		})
	}
}

func TestExpandCommands(t *testing.T) {
	src := `
command "build" { run = "echo build" }
command "deploy" { run = "echo deploy" }
target "release" { commands = ["build", "deploy"] }
`
	m, err := Parse([]byte(src), "test.hcl")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	got, err := m.ExpandCommands([]string{"release"})
	if err != nil {
		t.Fatalf("ExpandCommands(target): %v", err)
	}
	want := []string{"build", "deploy"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("ExpandCommands(target) = %#v, want %#v", got, want)
	}

	got, err = m.ExpandCommands([]string{"build"})
	if err != nil {
		t.Fatalf("ExpandCommands(command): %v", err)
	}
	if len(got) != 1 || got[0] != "build" {
		t.Errorf("ExpandCommands(command) = %#v", got)
	}

	if _, err := m.ExpandCommands([]string{"nope"}); err == nil {
		t.Fatal("expected an error for an unknown name")
	} else if !strings.Contains(err.Error(), `"nope" is not a known command or target`) {
		t.Errorf("error = %q", err)
	}
}
