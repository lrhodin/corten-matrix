// corten-matrix - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

// Package imconfig holds the iMessage network config template as the single
// source of truth shared by the bridge connector (pkg/connector) and the
// Beeper registration tool (cmd/bbctl).
//
// It deliberately has NO dependency on pkg/connector or the Rust FFI
// (pkg/rustpushgo), so cmd/bbctl can import it without linking the Rust
// library into the lightweight bbctl binary. Keeping a single embedded copy
// here is what guarantees a self-hosted config and a Beeper-generated config
// carry the identical set of network keys — previously bbctl shipped a
// hand-maintained 6-key subset that drifted out of sync with this file,
// leaving Beeper/Linux installs missing keys that the install scripts then
// had to splice back in with fragile sed/python surgery.
package imconfig

import (
	_ "embed"
	"strings"
)

// NetworkExampleConfig is the documented default for the bridge's `network`
// config section, with keys at column 0 (i.e. NOT yet nested under a
// top-level `network:` key). pkg/connector embeds this verbatim as its
// example config; the bridgev2 framework nests it under `network:` itself.
//
//go:embed example-config.yaml
var NetworkExampleConfig string

// WrapNetwork returns NetworkExampleConfig nested under a top-level `network:`
// key. It is byte-for-byte identical to the wrapping the bridgev2 framework
// applies in its own makeFullExampleConfig — a "# Network-specific config
// options" header, then `network:`, then every line (blank lines included)
// indented four spaces, then a trailing blank line. cmd/bbctl prepends this to
// the Beeper base config so a freshly registered Beeper (Linux) bridge and a
// self-hosted (Mac) bridge carry an identical network block, not merely the
// same keys. Keep this loop in lockstep with mxmain.makeFullExampleConfig.
func WrapNetwork() string {
	var buf strings.Builder
	buf.WriteString("# Network-specific config options\n")
	buf.WriteString("network:\n")
	for _, line := range strings.Split(NetworkExampleConfig, "\n") {
		buf.WriteString("    ")
		buf.WriteString(line)
		buf.WriteByte('\n')
	}
	buf.WriteByte('\n')
	return buf.String()
}
