fn main() {
    // Gate on the TARGET os, not the host: a build script is compiled for the
    // host, so #[cfg(target_os)] would be macOS even when cross-building to Linux.
    // CARGO_CFG_TARGET_OS is the real target.
    let target_os = std::env::var("CARGO_CFG_TARGET_OS").unwrap_or_default();
    if target_os == "macos" {
        // Compile the Objective-C hardware info reader (macOS only).
        cc::Build::new()
            .file("src/hardware_info.m")
            .flag("-fobjc-arc")
            .flag("-framework")
            .flag("Foundation")
            .flag("-framework")
            .flag("IOKit")
            .compile("hardware_info");

        println!("cargo:rustc-link-lib=framework=Foundation");
        println!("cargo:rustc-link-lib=framework=IOKit");
    }
}
