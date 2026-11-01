//! The zero-model matcher of ADR-0019. The code lives in the pando crate
//! (coppice-rs needs the same matcher); this module keeps the oracle tests.

pub use pando::lexical::*;

#[cfg(test)]
mod tests {
    use super::*;
    use crate::warden::pyjson::{loads, Value};

    fn units() -> Value {
        let p = concat!(env!("CARGO_MANIFEST_DIR"), "/../fixtures/pathways/units.json");
        loads(&std::fs::read_to_string(p).unwrap()).unwrap()
    }

    #[test]
    fn digests_match_the_oracle() {
        let u = units();
        let Value::List(pairs) = u.as_obj().unwrap().value("blake2b") else { panic!() };
        assert!(pairs.len() > 50);
        for p in pairs {
            let Value::List(p) = p else { panic!() };
            let (t, want) = (p[0].as_str().unwrap(), p[1].as_str().unwrap());
            assert_eq!(format!("{:016x}", hash(t)), want, "{t:?}");
        }
    }

    #[test]
    fn vectors_match_the_oracle() {
        let u = units();
        let Value::List(cases) = u.as_obj().unwrap().value("lexical") else { panic!() };
        for c in cases {
            let o = c.as_obj().unwrap();
            let text = o.value("text").as_str().unwrap();
            let Value::List(toks) = o.value("tokens") else { panic!() };
            let want: Vec<&str> = toks.iter().map(|t| t.as_str().unwrap()).collect();
            assert_eq!(tokens(text), want, "{text:?}");
            let v = encode(text);
            let Value::List(nz) = o.value("nz") else { panic!() };
            let mut got = vec![];
            for (i, x) in v.iter().enumerate() {
                if *x != 0.0 {
                    got.push((i, *x));
                }
            }
            assert_eq!(got.len(), nz.len(), "{text:?}");
            for (g, w) in got.iter().zip(nz) {
                let Value::List(w) = w else { panic!() };
                let wi: usize = match &w[0] {
                    Value::Int(t) => t.parse().unwrap(),
                    _ => panic!(),
                };
                let wx = match &w[1] {
                    Value::Float(f) => *f,
                    _ => panic!(),
                };
                assert_eq!(g.0, wi, "{text:?}");
                assert!((g.1 - wx).abs() < 1e-12, "{text:?}: {} vs {wx}", g.1);
            }
        }
    }
}
