#[test] fn b_ok() {}

#[test]
fn b_env() {
    for k in ["SPLIT_SCRIPT", "SPLIT_CONFIG", "SPLIT_REL", "SPLIT_FORCED", "OUT_DIR"] {
        println!("{k}={:?}", std::env::var(k));
    }
    println!("gen={}", std::fs::read_to_string(concat!(env!("OUT_DIR"), "/gen.txt")).unwrap());
}
