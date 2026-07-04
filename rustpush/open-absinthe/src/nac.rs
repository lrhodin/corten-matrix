// Public (FOSS) NAC validation for the corten-matrix bridge.
//
// This is the open-source open-absinthe NAC layer. It ships exactly ONE path:
// Apple's native `AAAbsintheContext` framework via the `nac-validation` crate
// (macOS 13+, `native-nac` feature). In other words: the bridge generates Apple
// validation data by running on a real Mac, using Apple's own code.
//
// The historical local x86-64 NAC *emulator* AND the Mac-to-Linux NAC *relay*
// have both been removed from the public source — the emulator because keeping
// reverse-engineered NAC internals around is a liability, and the relay because
// relaying from a Mac to a Linux host still requires a Mac, so it added a moving
// part without removing the Mac requirement. If you want to run the bridge,
// run it on a Mac.

use std::cell::UnsafeCell;
use std::fmt;

use log::{debug, info, warn};
use serde::{de, Deserialize, Deserializer, Serialize, Serializer};

use crate::AbsintheError;

/// `_enc` hardware fields were only consumed by the removed local emulator.
/// Native AAAbsintheContext reads real hardware identifiers from IOKit itself,
/// so on the public build this is a no-op.
pub fn enrich_missing_enc_fields(_hw: &mut HardwareConfig) -> Result<(), AbsintheError> {
    Ok(())
}

// ============================================================================
// Serde helpers
// ============================================================================

pub fn bin_serialize<S>(x: &[u8], s: S) -> Result<S::Ok, S::Error>
where
    S: Serializer,
{
    s.serialize_bytes(x)
}

pub fn bin_deserialize_mac<'de, D>(d: D) -> Result<[u8; 6], D::Error>
where
    D: Deserializer<'de>,
{
    let v = bin_deserialize(d)?;
    if v.is_empty() {
        return Ok([0u8; 6]);
    }
    v.try_into().map_err(|v: Vec<u8>| {
        de::Error::custom(format!("expected 6 bytes for MAC address, got {}", v.len()))
    })
}

pub fn bin_deserialize<'de, D>(d: D) -> Result<Vec<u8>, D::Error>
where
    D: Deserializer<'de>,
{
    struct DataVisitor;

    impl<'de> de::Visitor<'de> for DataVisitor {
        type Value = Vec<u8>;

        fn expecting(&self, formatter: &mut fmt::Formatter) -> fmt::Result {
            formatter.write_str("a byte array, sequence of u8, or null")
        }

        fn visit_bytes<E>(self, v: &[u8]) -> Result<Self::Value, E>
        where
            E: de::Error,
        {
            Ok(v.to_owned())
        }

        fn visit_byte_buf<E>(self, v: Vec<u8>) -> Result<Self::Value, E>
        where
            E: de::Error,
        {
            Ok(v)
        }

        // serde_json represents byte arrays as JSON arrays [u8, u8, ...],
        // which calls visit_seq instead of visit_bytes.
        fn visit_seq<A>(self, mut seq: A) -> Result<Self::Value, A::Error>
        where
            A: de::SeqAccess<'de>,
        {
            let mut bytes = Vec::with_capacity(seq.size_hint().unwrap_or(0));
            while let Some(b) = seq.next_element::<u8>()? {
                bytes.push(b);
            }
            Ok(bytes)
        }

        // Handle JSON null → empty Vec (Apple Silicon Macs lack _enc fields)
        fn visit_unit<E>(self) -> Result<Self::Value, E>
        where
            E: de::Error,
        {
            Ok(Vec::new())
        }

        fn visit_none<E>(self) -> Result<Self::Value, E>
        where
            E: de::Error,
        {
            Ok(Vec::new())
        }
    }

    d.deserialize_any(DataVisitor)
}

// ============================================================================
// HardwareConfig
// ============================================================================

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct HardwareConfig {
    pub product_name: String,
    #[serde(serialize_with = "bin_serialize", deserialize_with = "bin_deserialize_mac")]
    pub io_mac_address: [u8; 6],
    pub platform_serial_number: String,
    pub platform_uuid: String,
    pub root_disk_uuid: String,
    pub board_id: String,
    pub os_build_num: String,
    #[serde(serialize_with = "bin_serialize", deserialize_with = "bin_deserialize")]
    pub platform_serial_number_enc: Vec<u8>, // Gq3489ugfi
    #[serde(serialize_with = "bin_serialize", deserialize_with = "bin_deserialize")]
    pub platform_uuid_enc: Vec<u8>, // Fyp98tpgj
    #[serde(serialize_with = "bin_serialize", deserialize_with = "bin_deserialize")]
    pub root_disk_uuid_enc: Vec<u8>, // kbjfrfpoJU
    #[serde(serialize_with = "bin_serialize", deserialize_with = "bin_deserialize")]
    pub rom: Vec<u8>,
    #[serde(serialize_with = "bin_serialize", deserialize_with = "bin_deserialize")]
    pub rom_enc: Vec<u8>, // oycqAZloTNDm
    pub mlb: String,
    #[serde(serialize_with = "bin_serialize", deserialize_with = "bin_deserialize")]
    pub mlb_enc: Vec<u8>, // abKPld1EcMni
}

impl HardwareConfig {
    pub fn from_validation_data(_data: &[u8]) -> Result<HardwareConfig, AbsintheError> {
        Err(AbsintheError::Other(
            "from_validation_data is not supported (local emulator removed)".into(),
        ))
    }
}

// ============================================================================
// ValidationCtx
// ============================================================================

/// Validation context used by rustpush's `MacOSConfig` to generate APNs
/// validation data. The only backend is Apple's native `AAAbsintheContext`
/// (macOS + `native-nac` feature), driven through the `nac-validation` crate's
/// three-step API. Upstream rustpush is unmodified — the same HTTP flow runs,
/// just driven by `AAAbsintheContext`.
///
/// On a build without a native backend (non-macOS, or `native-nac` off) NAC is
/// simply unavailable: `new()` returns an error. Run the bridge on a Mac.
pub struct ValidationCtx {
    inner: ValidationCtxInner,
}

#[allow(dead_code)]
enum ValidationCtxInner {
    #[cfg(all(target_os = "macos", feature = "native-nac"))]
    Native {
        // UnsafeCell so `sign(&self)` can call NacContext's `&mut self`
        // methods. ValidationCtx is only used from a single task — the
        // `unsafe impl Send` below mirrors that invariant.
        ctx: UnsafeCell<nac_validation::NacContext>,
    },
    // Keeps the type inhabited and the methods type-checkable on builds with no
    // native backend; never constructed (new() errors first).
    #[cfg(not(all(target_os = "macos", feature = "native-nac")))]
    Unsupported,
}

unsafe impl Send for ValidationCtx {}

impl ValidationCtx {
    /// Initialise NAC validation.
    ///
    /// * `cert_chain`        — certificate bytes from Apple's validation cert endpoint
    /// * `out_request_bytes` — filled with the session-info-request to send to Apple
    /// * `hw_config`         — hardware identifiers (extracted from a real Mac)
    pub fn new(
        cert_chain: &[u8],
        out_request_bytes: &mut Vec<u8>,
        hw_config: &HardwareConfig,
    ) -> Result<ValidationCtx, AbsintheError> {
        // Native NAC path — macOS only, requires `native-nac`. Delegates to
        // AAAbsintheContext via the `nac-validation` crate. Hardware `_enc`
        // fields are irrelevant — Apple's framework reads real identifiers from
        // IOKit itself.
        #[cfg(all(target_os = "macos", feature = "native-nac"))]
        {
            match nac_validation::NacContext::init(cert_chain) {
                Ok((ctx, request_bytes)) => {
                    info!(
                        "NAC native path: NacContext::init produced {} request bytes",
                        request_bytes.len()
                    );
                    *out_request_bytes = request_bytes;
                    return Ok(ValidationCtx {
                        inner: ValidationCtxInner::Native {
                            ctx: UnsafeCell::new(ctx),
                        },
                    });
                }
                Err(e) => {
                    warn!("NAC native path: NacContext::init failed: {}", e);
                    return Err(AbsintheError::Other(format!(
                        "native NAC (AAAbsintheContext) failed: {e}"
                    )));
                }
            }
        }

        #[cfg(not(all(target_os = "macos", feature = "native-nac")))]
        {
            let _ = (cert_chain, out_request_bytes, hw_config);
            Err(AbsintheError::Other(
                "no NAC backend available — the public build does NAC only via Apple's \
                 native AAAbsintheContext. Build on macOS with the `nac-apple-framework` \
                 feature and run the bridge on that Mac."
                    .into(),
            ))
        }
    }

    /// Process the session-info response from Apple.
    pub fn key_establishment(&mut self, response: &[u8]) -> Result<(), AbsintheError> {
        match &mut self.inner {
            #[cfg(all(target_os = "macos", feature = "native-nac"))]
            ValidationCtxInner::Native { ctx, .. } => {
                debug!(
                    "NAC key_establishment: native path, {} bytes -> AAAbsintheContext",
                    response.len()
                );
                // Safety: single-task use; see ValidationCtx struct doc.
                let ctx = unsafe { &mut *ctx.get() };
                ctx.key_establishment(response)
                    .map_err(|e| AbsintheError::Other(format!("NACKeyEstablishment: {e}")))?;
                Ok(())
            }
            #[cfg(not(all(target_os = "macos", feature = "native-nac")))]
            ValidationCtxInner::Unsupported => {
                let _ = response;
                Err(AbsintheError::Other("no NAC backend available".into()))
            }
        }
    }

    /// Generate signed validation data.
    pub fn sign(&self) -> Result<Vec<u8>, AbsintheError> {
        match &self.inner {
            #[cfg(all(target_os = "macos", feature = "native-nac"))]
            ValidationCtxInner::Native { ctx } => {
                // 3-step NACSign on the context whose key_establishment was
                // driven by the upstream HTTP flow.
                // Safety: single-task use; see ValidationCtx struct doc.
                let ctx = unsafe { &mut *ctx.get() };
                let bytes = ctx
                    .sign()
                    .map_err(|e| AbsintheError::Other(format!("NACSign: {e}")))?;
                debug!("NAC sign: native 3-step path produced {} bytes", bytes.len());
                Ok(bytes)
            }
            #[cfg(not(all(target_os = "macos", feature = "native-nac")))]
            ValidationCtxInner::Unsupported => {
                Err(AbsintheError::Other("no NAC backend available".into()))
            }
        }
    }
}
