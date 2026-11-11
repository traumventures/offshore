#[derive(hello_macros::Named)]
struct Greeter;

fn main() {
    let where_ = if std::env::var("OFFSHORE_SESSION").is_ok() { "remote" } else { "local" };
    println!("greeting=hello ran={where_}");
    let _len = Greeter::NAME.len();
}

#[test]
fn where_tests_run() {
    let where_ = if std::env::var("OFFSHORE_SESSION").is_ok() { "remote" } else { "local" };
    println!("test ran={where_} manifest_dir={}", env!("CARGO_MANIFEST_DIR"));
    assert!(std::env::var("E2E_TEST_FAIL").is_err(), "E2E_TEST_FAIL is set");
}
