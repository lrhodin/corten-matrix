// corten-matrix - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/lrhodin/corten-matrix/pkg/imconfig"
)

// configPathFromArgs mirrors mxmain's -c/--config flag (default "config.yaml")
// so the config can be located before PreInit parses flags.
func configPathFromArgs() string {
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-c" || args[i] == "--config":
			if i+1 < len(args) {
				return args[i+1]
			}
		case strings.HasPrefix(args[i], "-c="):
			return strings.TrimPrefix(args[i], "-c=")
		case strings.HasPrefix(args[i], "--config="):
			return strings.TrimPrefix(args[i], "--config=")
		}
	}
	return "config.yaml"
}

// ensureNetworkConfigKeys appends any network keys the current build knows
// about but that are missing from the on-disk config, copying their documented
// defaults and comments from the embedded example. It runs before the bridge
// loads its config, so a `git pull` + rebuild + restart always lands a
// complete network block — independent of whether the framework's own config
// upgrade writes back (e.g. when the bridge is launched with --no-update).
//
// It is intentionally conservative — NO breaking changes:
//   - Keys that already exist are NEVER touched. The existing file is never
//     re-serialized; only the new key lines are spliced in, so every existing
//     value, comment and byte of formatting is preserved exactly.
//   - A config that does not parse is left completely untouched — a
//     structurally broken file is for manual repair; we must not make it worse.
//   - It writes only when something is actually missing, and writes atomically
//     (temp file + rename, mode 0600).
//   - It is idempotent: once the keys exist, every later run is a no-op.
func ensureNetworkConfigKeys(configPath string) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return
	}

	// Parse ONLY to detect which keys are present and to locate the end of the
	// network block. We never marshal this tree back out.
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return // unparseable: leave for manual repair
	}
	if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return
	}
	network := mappingValue(root.Content[0], "network")
	if network == nil || network.Kind != yaml.MappingNode || len(network.Content) == 0 {
		return
	}

	present := make(map[string]bool, len(network.Content)/2)
	endLine := network.Line
	for i := 0; i+1 < len(network.Content); i += 2 {
		present[network.Content[i].Value] = true
		if l := maxNodeLine(network.Content[i+1]); l > endLine {
			endLine = l
		}
		if network.Content[i].Line > endLine {
			endLine = network.Content[i].Line
		}
	}

	// Build the text to splice in: each missing key's comment block + key line
	// (+ any nested lines), taken verbatim from the example and indented one
	// level to sit under `network:`.
	var additions []string
	added := 0
	for _, blk := range exampleNetworkBlocks() {
		if present[blk.key] {
			continue
		}
		additions = append(additions, "")
		for _, line := range blk.lines {
			if line == "" {
				additions = append(additions, "")
			} else {
				additions = append(additions, "    "+line)
			}
		}
		added++
	}
	if added == 0 {
		return
	}

	lines := strings.Split(string(data), "\n")
	if endLine < 1 || endLine > len(lines) {
		return // line numbers out of range — bail rather than risk corruption
	}
	out := make([]string, 0, len(lines)+len(additions))
	out = append(out, lines[:endLine]...) // up to and including the last network line
	out = append(out, additions...)
	out = append(out, lines[endLine:]...) // the rest of the file, untouched

	if err := atomicWriteConfig(configPath, []byte(strings.Join(out, "\n"))); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not add missing config keys to %s: %v\n", configPath, err)
		return
	}
	fmt.Fprintf(os.Stderr, "Added %d missing network config key(s) to %s\n", added, configPath)
}

// mappingValue returns the value node for key in a YAML mapping node, or nil.
func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

// maxNodeLine returns the largest source line number anywhere in the subtree,
// so the end of a (possibly nested) value block can be located.
func maxNodeLine(n *yaml.Node) int {
	max := n.Line
	for _, c := range n.Content {
		if l := maxNodeLine(c); l > max {
			max = l
		}
	}
	return max
}

type exampleBlock struct {
	key   string
	lines []string // leading comment block + key line (+ nested lines), at column 0
}

// exampleNetworkBlocks splits the embedded example into per-top-level-key text
// blocks (each with its leading comment block and any nested lines), in file
// order. Working from the example text keeps additions clean and documented
// and keeps the example the single source of truth.
func exampleNetworkBlocks() []exampleBlock {
	var blocks []exampleBlock
	var pending []string
	cur := -1
	for _, line := range strings.Split(strings.TrimRight(imconfig.NetworkExampleConfig, "\n"), "\n") {
		isTopKey := len(line) > 0 && line[0] != ' ' && line[0] != '\t' && line[0] != '#' && strings.Contains(line, ":")
		isIndented := len(line) > 0 && (line[0] == ' ' || line[0] == '\t')
		switch {
		case isTopKey:
			blk := exampleBlock{key: strings.SplitN(strings.TrimSpace(line), ":", 2)[0]}
			blk.lines = append(blk.lines, trailingCommentBlock(pending)...)
			blk.lines = append(blk.lines, line)
			blocks = append(blocks, blk)
			cur = len(blocks) - 1
			pending = nil
		case isIndented && cur >= 0:
			blocks[cur].lines = append(blocks[cur].lines, line)
		default:
			pending = append(pending, line)
			cur = -1
		}
	}
	return blocks
}

// trailingCommentBlock returns the run of lines at the end of pending that
// immediately precede a key (i.e. everything after the last blank separator),
// so a key's own documentation comments travel with it.
func trailingCommentBlock(pending []string) []string {
	start := 0
	for i, line := range pending {
		if strings.TrimSpace(line) == "" {
			start = i + 1
		}
	}
	return pending[start:]
}

// atomicWriteConfig writes data to path via a temp file + rename so a partial
// write can never leave a truncated config behind.
func atomicWriteConfig(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "imessage-config-*.yaml")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}
