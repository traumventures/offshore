//! A tiny derive macro without dependencies: the e2e test checks that its
//! proc-macro library built remotely is loadable locally through the output
//! FUSE (rust-analyzer lsp-remote mode).
use proc_macro::{TokenStream, TokenTree};

/// Adds `const NAME: &str` with the type's name.
#[proc_macro_derive(Named)]
pub fn derive_named(input: TokenStream) -> TokenStream {
    let mut tokens = input.into_iter();
    let mut name = None;
    while let Some(t) = tokens.next() {
        if matches!(&t, TokenTree::Ident(i) if i.to_string() == "struct") {
            if let Some(TokenTree::Ident(n)) = tokens.next() {
                name = Some(n.to_string());
            }
            break;
        }
    }
    let name = name.expect("#[derive(Named)] supports structs only");
    format!("impl {name} {{ pub const NAME: &'static str = \"{name}\"; }}")
        .parse()
        .unwrap()
}
