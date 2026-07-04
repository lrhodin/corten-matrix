# Open Absinthe

Native NAC (Network Attestation Check) validation for the corten-matrix bridge.

This crate generates Apple validation data using Apple's own `AAAbsintheContext`
framework (macOS 13+), via the `nac-validation` crate, when built with the
`native-nac` feature. There is no emulator and no relay: the bridge does NAC by
running on a real Mac.
