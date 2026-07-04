//! LocalMacOSConfig — reads hardware info from IOKit and produces a
//! `rustpush::macos::MacOSConfig` via `into_macos_config()`.
//!
//! `MacOSConfig` already implements `OSConfig` (inside the rustpush crate where
//! `ActivationInfo` is visible), so we don't need to re-implement `OSConfig` here.
//! Our overlaid `rustpush/open-absinthe/src/nac.rs` provides the native NAC path
//! (`AAAbsintheContext` via `nac-validation`) when the `native-nac` feature is on,
//! so `MacOSConfig::generate_validation_data` automatically uses it.
//!
//! _enc fields are left empty — the native NAC path reads hardware identifiers
//! directly from IOKit and does not use them.

use std::ffi::CStr;

use rustpush::macos::{HardwareConfig, MacOSConfig};

// FFI for hardware_info.m
#[repr(C)]
struct CHardwareInfo {
    product_name: *mut std::os::raw::c_char,
    serial_number: *mut std::os::raw::c_char,
    platform_uuid: *mut std::os::raw::c_char,
    board_id: *mut std::os::raw::c_char,
    os_build_num: *mut std::os::raw::c_char,
    os_version: *mut std::os::raw::c_char,
    rom: *mut u8,
    rom_len: usize,
    mlb: *mut std::os::raw::c_char,
    mac_address: *mut u8,
    mac_address_len: usize,
    root_disk_uuid: *mut std::os::raw::c_char,
    darwin_version: *mut std::os::raw::c_char,
    error: *mut std::os::raw::c_char,
}

extern "C" {
    fn hw_info_read() -> CHardwareInfo;
    fn hw_info_free(info: *mut CHardwareInfo);
}

fn c_str_to_string(ptr: *mut std::os::raw::c_char) -> Option<String> {
    if ptr.is_null() {
        return None;
    }
    Some(unsafe { CStr::from_ptr(ptr) }.to_string_lossy().into_owned())
}

fn c_data_to_vec(ptr: *mut u8, len: usize) -> Vec<u8> {
    if ptr.is_null() || len == 0 {
        return vec![];
    }
    unsafe { std::slice::from_raw_parts(ptr, len) }.to_vec()
}

/// Hardware info read from IOKit.
#[derive(Debug, Clone)]
pub struct HardwareInfo {
    pub product_name: String,
    pub serial_number: String,
    pub platform_uuid: String,
    pub board_id: String,
    pub os_build_num: String,
    pub os_version: String,
    pub rom: Vec<u8>,
    pub mlb: String,
    pub mac_address: [u8; 6],
    pub root_disk_uuid: String,
    pub darwin_version: String,
}

impl HardwareInfo {
    pub fn read() -> Result<Self, String> {
        let mut raw = unsafe { hw_info_read() };

        if !raw.error.is_null() {
            let err = c_str_to_string(raw.error).unwrap_or_default();
            unsafe { hw_info_free(&mut raw) };
            return Err(err);
        }

        let mac_vec = c_data_to_vec(raw.mac_address, raw.mac_address_len);
        let mac_address: [u8; 6] = if mac_vec.len() == 6 {
            mac_vec.try_into().unwrap()
        } else {
            [0; 6]
        };

        let info = HardwareInfo {
            product_name: c_str_to_string(raw.product_name).unwrap_or_else(|| "Mac".to_string()),
            serial_number: c_str_to_string(raw.serial_number).unwrap_or_default(),
            platform_uuid: c_str_to_string(raw.platform_uuid).unwrap_or_else(|| uuid::Uuid::new_v4().to_string()),
            board_id: c_str_to_string(raw.board_id).unwrap_or_default(),
            os_build_num: c_str_to_string(raw.os_build_num).unwrap_or_else(|| "25B78".to_string()),
            os_version: c_str_to_string(raw.os_version).unwrap_or_else(|| "26.1".to_string()),
            rom: c_data_to_vec(raw.rom, raw.rom_len),
            mlb: c_str_to_string(raw.mlb).unwrap_or_default(),
            mac_address,
            root_disk_uuid: c_str_to_string(raw.root_disk_uuid).unwrap_or_default(),
            darwin_version: c_str_to_string(raw.darwin_version).unwrap_or_else(|| "24.0.0".to_string()),
        };

        unsafe { hw_info_free(&mut raw) };
        Ok(info)
    }
}

/// Local macOS configuration builder.
/// Call `into_macos_config()` to get the `MacOSConfig` that implements `OSConfig`.
#[derive(Clone)]
pub struct LocalMacOSConfig {
    pub hw: HardwareInfo,
    pub device_id: String,
    pub protocol_version: u32,
    pub icloud_ua: String,
    pub aoskit_version: String,
}

impl LocalMacOSConfig {
    pub fn new() -> Result<Self, String> {
        let hw = HardwareInfo::read()?;
        // Use the real hardware UUID — AAAbsintheContext embeds it in
        // validation data, so a random UUID would cause Apple to reject
        // the registration (error 6001).
        let device_id = hw.platform_uuid.to_uppercase();

        let darwin = &hw.darwin_version;
        let icloud_ua = format!(
            "com.apple.iCloudHelper/282 CFNetwork/1568.100.1 Darwin/{}",
            darwin
        );
        let aoskit_version = "com.apple.AOSKit/282 (com.apple.accountsd/113)".to_string();

        Ok(Self {
            hw,
            device_id,
            protocol_version: 1660,
            icloud_ua,
            aoskit_version,
        })
    }

    pub fn with_device_id(self, id: String) -> Self {
        // For LocalMacOSConfig, the device ID must always be the hardware
        // UUID because AAAbsintheContext embeds it in the validation data.
        // Ignore any persisted device ID — it may be a stale random UUID
        // from before this fix.
        if id != self.device_id {
            log::warn!(
                "Ignoring persisted device ID {} — LocalMacOSConfig must use hardware UUID {}",
                id, self.device_id
            );
        }
        self
    }

    /// Convert to the `MacOSConfig` that implements `OSConfig`.
    ///
    /// `_enc` fields are empty — the native NAC path (our overlaid
    /// `rustpush/open-absinthe/src/nac.rs`) reads hardware identifiers from
    /// IOKit directly and does not use these fields.
    pub fn into_macos_config(self) -> MacOSConfig {
        MacOSConfig {
            inner: HardwareConfig {
                product_name: self.hw.product_name,
                io_mac_address: self.hw.mac_address,
                platform_serial_number: self.hw.serial_number,
                platform_uuid: self.hw.platform_uuid,
                root_disk_uuid: self.hw.root_disk_uuid,
                board_id: self.hw.board_id,
                os_build_num: self.hw.os_build_num,
                // _enc fields empty — native NAC reads from IOKit internally
                platform_serial_number_enc: vec![],
                platform_uuid_enc: vec![],
                root_disk_uuid_enc: vec![],
                rom: self.hw.rom,
                rom_enc: vec![],
                mlb: self.hw.mlb,
                mlb_enc: vec![],
            },
            version: self.hw.os_version,
            protocol_version: self.protocol_version,
            device_id: self.device_id.clone(),
            icloud_ua: self.icloud_ua,
            aoskit_version: self.aoskit_version,
            udid: Some(self.device_id),
        }
    }
}
