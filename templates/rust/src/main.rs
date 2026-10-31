use std::io::{BufRead, BufReader, Write};
use std::net::TcpListener;

fn main() -> std::io::Result<()> {
    let port = std::env::var("PORT").unwrap_or_else(|_| "8080".into());
    let slot = std::env::var("COOLIX_SLOT").unwrap_or_default();
    let listener = TcpListener::bind(format!("127.0.0.1:{port}"))?;
    println!("listening on 127.0.0.1:{port}");
    for stream in listener.incoming() {
        let mut stream = stream?;
        let mut request_line = String::new();
        BufReader::new(&stream).read_line(&mut request_line)?;
        let path = request_line.split_whitespace().nth(1).unwrap_or("/");
        let (status, body) = match path {
            "/healthz" => ("200 OK", "ok\n".to_string()),
            "/" => ("200 OK", format!("Hello from coolix! (slot {slot})\n")),
            _ => ("404 Not Found", "not found\n".to_string()),
        };
        write!(stream, "HTTP/1.1 {status}\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}", body.len())?;
    }
    Ok(())
}
