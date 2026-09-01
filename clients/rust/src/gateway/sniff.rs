//! The SSE usage sniffers: `gateway_asgi._UsageSniffer` for the Anthropic
//! wire and `gateway_openai.OpenAIUsageSniffer`. Each reads the stream line
//! by line as it passes, never holding it back.

use super::pyops::{is_py_int, iterate, loads_text, py_int, Item, JsonErr};
use crate::gate::py::text;
use crate::gate::pyjson::{Object, Value};

/// Either wire's sniffer.
pub struct Sniffer {
    openai: bool,
    buf: String,
    /// The usage dict it builds: keys in first-set order.
    pub usage: Object,
    pub text: String,
    /// message_start's message.model, None when none was a str.
    pub model: Option<String>,
    /// Set when json.loads raised something the sniffer does not catch (a
    /// RecursionError, an integer past the digit limit): the oracle's
    /// stream then ends there with an error.
    pub raised: bool,
}

impl Sniffer {
    pub fn new(openai: bool) -> Sniffer {
        Sniffer { openai, buf: String::new(), usage: Object::new(), text: String::new(), model: None, raised: false }
    }

    /// Reads one chunk: decoded on its own with replacement characters, as
    /// the oracle decodes each chunk.
    pub fn feed(&mut self, chunk: &[u8]) {
        if self.raised {
            return;
        }
        self.buf.push_str(&String::from_utf8_lossy(chunk));
        while let Some(i) = self.buf.find('\n') {
            let line = text::strip(&self.buf[..i]).to_string();
            self.buf.drain(..=i);
            let Some(rest) = line.strip_prefix("data:") else { continue };
            let payload = text::strip(rest);
            if payload.is_empty() || payload == "[DONE]" {
                continue;
            }
            let obj = match loads_text(payload) {
                Ok(v) => v,
                Err(JsonErr::Decode) => continue,
                Err(_) => {
                    self.raised = true;
                    return;
                }
            };
            if self.openai {
                self.openai_event(&obj);
            } else {
                self.anthropic_event(&obj);
            }
        }
    }

    fn anthropic_event(&mut self, obj: &Value) {
        let Value::Obj(o) = obj else { return };
        let typ = o.get("type");
        let mut u: Option<&Value> = None;
        if super::pyops::eq_str(typ, "message_start") {
            let empty = Value::Obj(Object::new());
            let message = o.get("message").unwrap_or(&empty);
            let Value::Obj(m) = message else { return };
            if let Some(Value::Obj(mu)) = m.get("usage") {
                self.add_ints(mu);
            }
            if let Some(Value::Str(s)) = m.get("model") {
                self.model = Some(s.clone());
            }
            return;
        } else if super::pyops::eq_str(typ, "message_delta") {
            u = o.get("usage");
        } else if super::pyops::eq_str(typ, "content_block_delta") {
            if let Some(Value::Obj(delta)) = o.get("delta") {
                if super::pyops::eq_str(delta.get("type"), "text_delta") {
                    if let Some(Value::Str(t)) = delta.get("text") {
                        self.text.push_str(t);
                    }
                }
            }
        }
        if let Some(Value::Obj(uo)) = u {
            let uo = uo.clone();
            self.add_ints(&uo);
        }
    }

    fn add_ints(&mut self, u: &Object) {
        for (k, v) in u.iter() {
            if is_py_int(v) {
                self.usage.set(k, v.clone());
            }
        }
    }

    fn openai_event(&mut self, obj: &Value) {
        let Value::Obj(o) = obj else { return };
        let empty = Value::List(vec![]);
        let choices = match o.get("choices") {
            Some(c) if c.truthy() => c,
            _ => &empty,
        };
        let Ok(items) = iterate(choices) else { return };
        for c in items {
            let Item::V(Value::Obj(co)) = c else { return };
            if let Some(Value::Obj(delta)) = co.get("delta") {
                if let Some(Value::Str(t)) = delta.get("content") {
                    // A raise at a later choice keeps this text: the oracle
                    // appends as it goes.
                    self.text.push_str(t);
                }
            }
        }
        let n = normalize_openai_usage(o.get("usage"));
        for (k, v) in n.iter() {
            self.usage.set(k, v.clone());
        }
    }
}

/// `normalize_openai_usage`: OpenAI usage on the meter's Anthropic-named
/// buckets.
pub fn normalize_openai_usage(usage: Option<&Value>) -> Object {
    let mut out = Object::new();
    let Some(Value::Obj(u)) = usage else { return out };
    let zero = Value::Int("0".into());
    let mut cached = zero.clone();
    if let Some(Value::Obj(d)) = u.get("prompt_tokens_details") {
        cached = d.get("cached_tokens").cloned().unwrap_or_else(|| zero.clone());
    }
    if !is_py_int(&cached) {
        cached = zero.clone();
    }
    if let Some(prompt) = u.get("prompt_tokens").filter(|p| is_py_int(p)) {
        let p = py_int(prompt).unwrap_or_default();
        let c = py_int(&cached).unwrap_or_default();
        let d = (p - c).max(num_bigint::BigInt::from(0));
        out.set("input_tokens", Value::Int(d.to_string()));
        out.set("cache_read_input_tokens", cached.clone());
        out.set("cache_creation_input_tokens", zero.clone());
    }
    if let Some(completion) = u.get("completion_tokens").filter(|p| is_py_int(p)) {
        out.set("output_tokens", completion.clone());
    }
    out
}
