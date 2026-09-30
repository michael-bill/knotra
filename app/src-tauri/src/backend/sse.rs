use super::{Error, Result};
#[derive(Debug, PartialEq)]
pub struct Frame {
    pub id: String,
    pub data: String,
}
/// Incremental UTF-8 SSE parsing; arbitrary network boundaries and CR/LF are allowed.
#[derive(Default)]
pub struct Parser {
    bytes: Vec<u8>,
    line: Vec<u8>,
    id: String,
    data: Vec<String>,
    cr: bool,
    size: usize,
}
impl Parser {
    pub fn push(&mut self, bytes: &[u8]) -> Result<Vec<Frame>> {
        let mut frames = Vec::new();
        self.bytes.extend_from_slice(bytes);
        for byte in std::mem::take(&mut self.bytes) {
            if byte == b'\n' && self.cr {
                self.cr = false;
                continue;
            }
            self.cr = byte == b'\r';
            if byte == b'\r' || byte == b'\n' {
                let line = String::from_utf8(std::mem::take(&mut self.line))
                    .map_err(|_| Error::new("protocol", "Invalid UTF-8 event."))?;
                if line.is_empty() {
                    if !self.data.is_empty() {
                        if self.id.is_empty() {
                            return Err(Error::new(
                                "protocol",
                                "Engine event is missing a durable ID.",
                            ));
                        }
                        frames.push(Frame {
                            id: std::mem::take(&mut self.id),
                            data: self.data.join("\n"),
                        });
                    }
                    self.id.clear();
                    self.data.clear();
                    self.size = 0;
                } else if !line.starts_with(':') {
                    let (field, value) = line.split_once(':').unwrap_or((&line, ""));
                    let value = value.strip_prefix(' ').unwrap_or(value);
                    if field == "id" {
                        if value.len() > 256 || value.chars().any(|c| c.is_control()) {
                            return Err(Error::new("protocol", "Invalid event ID."));
                        }
                        self.id = value.into();
                    }
                    if field == "data" {
                        self.data.push(value.into());
                    }
                }
            } else {
                self.line.push(byte);
            }
            self.size += 1;
            if self.size > 256 * 1024 {
                return Err(Error::new("limit", "Engine event exceeds 256 KiB."));
            }
        }
        Ok(frames)
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn fragmented_unicode_multiline_and_heartbeats() {
        let payload = ": ping\r\nid: e1\r\ndata: {\"text\":\"Привет\",\r\ndata: \"n\":1}\r\n\r\n";
        let mut parser = Parser::default();
        let mut frames = Vec::new();
        for byte in payload.as_bytes() {
            frames.extend(parser.push(&[*byte]).unwrap());
        }
        assert_eq!(
            frames,
            vec![Frame {
                id: "e1".into(),
                data: "{\"text\":\"Привет\",\n\"n\":1}".into()
            }]
        );
    }
    #[test]
    fn rejects_unresumable_and_oversized_events() {
        assert!(Parser::default().push(b"data: {}\n\n").is_err());
        assert!(Parser::default().push(&vec![b'x'; 256 * 1024 + 1]).is_err());
    }
}
