// SPDX-License-Identifier: BSD-3-Clause
//
// Copyright (c) 2026, the configuration-management-tool/cmt authors

// Command cmt runs named shell commands, from an HCL2 manifest, against
// groups of hosts in parallel — a pure-Go reimplementation of
// pressly/sup ("Stack Up") using go-remoteexec/transport for SSH/WinRM/
// local execution.
package main

import (
	"os"

	"github.com/configuration-management-tool/cmt/connect"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Stdin, connect.Dial))
}
