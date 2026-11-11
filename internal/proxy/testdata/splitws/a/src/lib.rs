/// ```
/// assert_eq!(a::two(), 2);
/// ```
pub fn two() -> u32 {
    2
}

#[cfg(test)]
mod t {
    #[test]
    fn lib_env() {
        let args: Vec<String> = std::env::args().skip(1).collect();
        println!("cwd={}", std::env::current_dir().unwrap().display());
        println!("args={args:?}");
        for k in ["CARGO_MANIFEST_DIR", "CARGO_MANIFEST_PATH", "CARGO_PKG_NAME", "CARGO_PKG_VERSION", "CARGO_PKG_VERSION_MAJOR", "CARGO_PKG_DESCRIPTION"] {
            println!("{k}={:?}", std::env::var(k));
        }
    }

    #[test]
    fn maybe_fail() {
        assert!(std::env::var("SPLIT_FAIL").is_err());
    }
}
