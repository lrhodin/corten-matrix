//go:build !darwin || ios

package main

func isSetupMode() bool {
	return false
}

func runSetupPermissions() {}
