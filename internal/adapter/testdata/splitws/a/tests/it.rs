#[test]
fn bin_runs() {
    let out = std::process::Command::new(env!("CARGO_BIN_EXE_a")).output().unwrap();
    assert_eq!(out.stdout, b"2\n");
    assert_eq!(std::env::var("CARGO_BIN_EXE_a").unwrap(), env!("CARGO_BIN_EXE_a"));
}
