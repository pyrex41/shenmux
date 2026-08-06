wit_bindgen::generate!({ path: "wit" });

use exports::shenmux::workspace::runtime::{Guest, Response};
use shenmux::workspace::filesystem;

const MAX_READ: u64 = 16 * 1024 * 1024;

struct Workspace;

fn normalize(path: &str, base: &str) -> String {
    let source = if path.starts_with('/') { path.to_owned() } else { format!("{base}/{path}") };
    let mut parts = Vec::new();
    for part in source.split('/') {
        match part {
            "" | "." => {}
            ".." => { parts.pop(); }
            value => parts.push(value),
        }
    }
    if parts.is_empty() { "/".into() } else { format!("/{}", parts.join("/")) }
}

fn error_message(error: filesystem::Error) -> String {
    match error {
        filesystem::Error::NotFound => "not found".into(),
        filesystem::Error::PermissionDenied => "permission denied".into(),
        filesystem::Error::Conflict => "conflict".into(),
        filesystem::Error::Io(message) => message,
    }
}

fn response(cwd: String, output: String, error: Option<String>) -> Response {
    Response { cwd, output, error, entries: Vec::new() }
}

impl Guest for Workspace {
    fn run(cwd: String, command: String) -> Response {
        let mut cwd = normalize(&cwd, "/");
        let mut args = command.split_whitespace();
        let name = args.next().unwrap_or("");
        let rest: Vec<&str> = args.collect();

        match name {
            "" => response(cwd, String::new(), None),
            "help" => response(
                cwd,
                "help  ls [path]  pwd  cd <path>  cat <path>  touch <path>  mkdir <path>  write <path> <text>  clear".into(),
                None,
            ),
            "clear" => response(cwd, String::new(), None),
            "pwd" => response(cwd.clone(), cwd, None),
            "ls" => {
                let path = normalize(rest.first().copied().unwrap_or("."), &cwd);
                match filesystem::list_entries(&path) {
                    Ok(entries) => Response { cwd, output: String::new(), error: None, entries },
                    Err(error) => response(cwd, String::new(), Some(format!("ls {}: {}", path, error_message(error)))),
                }
            }
            "cd" => {
                let path = normalize(rest.first().copied().unwrap_or("/"), &cwd);
                match filesystem::list_entries(&path) {
                    Ok(_) => { cwd = path; response(cwd.clone(), cwd, None) }
                    Err(error) => response(cwd, String::new(), Some(format!("cd {}: {}", path, error_message(error)))),
                }
            }
            "cat" => {
                let path = normalize(rest.first().copied().unwrap_or(""), &cwd);
                match filesystem::read(&path, 0, MAX_READ) {
                    Ok(bytes) => response(cwd, String::from_utf8_lossy(&bytes).into_owned(), None),
                    Err(error) => response(cwd, String::new(), Some(format!("cat {}: {}", path, error_message(error)))),
                }
            }
            "touch" => {
                let path = normalize(rest.first().copied().unwrap_or(""), &cwd);
                match filesystem::write(&path, &[], false) {
                    Ok(()) => response(cwd, format!("created {}", path.trim_start_matches('/')), None),
                    Err(error) => response(cwd, String::new(), Some(format!("touch {}: {}", path, error_message(error)))),
                }
            }
            "mkdir" => {
                let path = normalize(rest.first().copied().unwrap_or(""), &cwd);
                match filesystem::mkdir(&path) {
                    Ok(()) => response(cwd, format!("created {}", path.trim_start_matches('/')), None),
                    Err(error) => response(cwd, String::new(), Some(format!("mkdir {}: {}", path, error_message(error)))),
                }
            }
            "write" => {
                let path = normalize(rest.first().copied().unwrap_or(""), &cwd);
                let split_at = command.find(char::is_whitespace).unwrap_or(command.len());
                let payload = command[split_at..].trim_start().split_once(' ').map(|(_, text)| text).unwrap_or("");
                match filesystem::write(&path, payload.as_bytes(), true) {
                    Ok(()) => response(cwd, format!("saved {}", path.trim_start_matches('/')), None),
                    Err(error) => response(cwd, String::new(), Some(format!("write {}: {}", path, error_message(error)))),
                }
            }
            "sync" => response(cwd, "host filesystem sync is live; remote state is adapter-owned".into(), None),
            other => response(cwd, String::new(), Some(format!("command not found: {other}"))),
        }
    }
}

export!(Workspace);
