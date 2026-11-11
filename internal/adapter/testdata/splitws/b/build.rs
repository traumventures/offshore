fn main() {
    let out = std::env::var("OUT_DIR").unwrap();
    std::fs::write(format!("{out}/gen.txt"), "generated").unwrap();
    println!("cargo:rustc-env=SPLIT_SCRIPT=from-build-script");
}
