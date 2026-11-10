#[derive(hello_macros::Named)]
struct Greeter;

fn main() {
    let where_ = if std::env::var("CLOUDSOURCED_SESSION").is_ok() { "remote" } else { "local" };
    println!("greeting=hello ran={where_}");
    let _len = Greeter::NAME.len();
}
