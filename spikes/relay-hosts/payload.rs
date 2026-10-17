//! Deterministic high-entropy transport fixture; not cryptography.
pub const FRAME_BYTES: usize = 65_536;
pub const PAYLOAD_FORMAT: &str = "xorshift32-v1";

pub fn fill_frame(bytes: &mut [u8], index: u64) {
    assert_eq!(bytes.len(), FRAME_BYTES);
    bytes[..8].copy_from_slice(&index.to_be_bytes());
    let mut state = 0x9e37_79b9u32 ^ (index as u32).wrapping_add(1);
    for chunk in bytes[8..].as_chunks_mut::<4>().0 {
        state ^= state << 13;
        state ^= state >> 17;
        state ^= state << 5;
        chunk.copy_from_slice(&state.to_le_bytes());
    }
}
