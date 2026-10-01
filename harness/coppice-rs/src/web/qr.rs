//! A QR code drawn with half blocks, as Go's WriteQR draws it: rsc.io/qr
//! v0.2.0 (BSD-3-Clause) picks the mode, the version and the bits, always
//! with mask 0, and qrterminal v3.2.1 (MIT) draws it. A port of both, so
//! the same text gives the same code, character for character.

// The loops keep Go's shape, index for index, so the port reads against
// the original line by line.
#![allow(clippy::needless_range_loop, clippy::manual_div_ceil)]

/// The drawn width a QR must stay inside.
#[allow(dead_code)]
pub const MAX_QR_COLUMNS: usize = 120;

struct Field {
    log: [u8; 256],
    exp: [u8; 510],
}

impl Field {
    fn new() -> Field {
        let poly = 0x11d;
        let mut f = Field {
            log: [0; 256],
            exp: [0; 510],
        };
        let mut x: i32 = 1;
        for i in 0..255 {
            f.exp[i] = x as u8;
            f.exp[i + 255] = x as u8;
            f.log[x as usize] = i as u8;
            x = mul(x, 2, poly);
        }
        f.log[0] = 255;
        f
    }

    fn exp(&self, e: usize) -> u8 {
        self.exp[e % 255]
    }

    fn mul(&self, x: u8, y: u8) -> u8 {
        if x == 0 || y == 0 {
            return 0;
        }
        self.exp[self.log[x as usize] as usize + self.log[y as usize] as usize]
    }

    /// The generator polynomial of degree e and its logs.
    fn lgen(&self, e: usize) -> Vec<u8> {
        let mut p = vec![0u8; e + 1];
        p[e] = 1;
        for i in 0..e {
            let c = self.exp(i);
            for j in 0..e {
                p[j] = self.mul(p[j], c) ^ p[j + 1];
            }
            p[e] = self.mul(p[e], c);
        }
        p.iter()
            .map(|&c| if c == 0 { 255 } else { self.log[c as usize] })
            .collect()
    }

    fn ecc(&self, data: &[u8], c: usize) -> Vec<u8> {
        let lgen = self.lgen(c);
        let lgen = &lgen[1..];
        let mut p = data.to_vec();
        p.resize(data.len() + c, 0);
        for i in 0..data.len() {
            let ch = p[i];
            if ch == 0 {
                continue;
            }
            let base = self.log[ch as usize] as usize;
            for (j, &lg) in lgen.iter().enumerate() {
                if lg != 255 {
                    p[i + 1 + j] ^= self.exp[base + lg as usize];
                }
            }
        }
        p[data.len()..].to_vec()
    }
}

fn mul(mut x: i32, mut y: i32, poly: i32) -> i32 {
    let mut z = 0;
    while x > 0 {
        if x & 1 != 0 {
            z ^= y;
        }
        x >>= 1;
        y <<= 1;
        if y & 0x100 != 0 {
            y ^= poly;
        }
    }
    z
}

#[derive(Default)]
struct Bits {
    b: Vec<u8>,
    nbit: usize,
}

impl Bits {
    fn write(&mut self, mut v: u32, mut nbit: usize) {
        while nbit > 0 {
            let mut n = nbit.min(8);
            if self.nbit.is_multiple_of(8) {
                self.b.push(0);
            } else {
                let m = (8 - self.nbit % 8) % 8;
                n = n.min(m);
            }
            self.nbit += n;
            let sh = nbit - n;
            let last = self.b.len() - 1;
            let shift_left = (8 - self.nbit % 8) % 8;
            self.b[last] |= ((v >> sh) << shift_left) as u8;
            v -= (v >> sh) << sh;
            nbit -= n;
        }
    }

    fn pad(&mut self, n: usize) {
        if n <= 4 {
            self.write(0, n);
            return;
        }
        let mut n = n;
        self.write(0, 4);
        n -= 4;
        let fill = (8 - self.nbit % 8) % 8;
        n -= fill;
        self.write(0, fill);
        let pad = n / 8;
        let mut i = 0;
        while i < pad {
            self.write(0xec, 8);
            if i + 1 >= pad {
                break;
            }
            self.write(0x11, 8);
            i += 2;
        }
    }
}

/// apos, astride, bytes, pattern, and (nblock, check) per level L M Q H.
type V = (usize, usize, usize, u32, [(usize, usize); 4]);

const VTAB: [V; 41] = [
    (0, 0, 0, 0, [(0, 0); 4]),
    (100, 100, 26, 0x0, [(1, 7), (1, 10), (1, 13), (1, 17)]),
    (16, 100, 44, 0x0, [(1, 10), (1, 16), (1, 22), (1, 28)]),
    (20, 100, 70, 0x0, [(1, 15), (1, 26), (2, 18), (2, 22)]),
    (24, 100, 100, 0x0, [(1, 20), (2, 18), (2, 26), (4, 16)]),
    (28, 100, 134, 0x0, [(1, 26), (2, 24), (4, 18), (4, 22)]),
    (32, 100, 172, 0x0, [(2, 18), (4, 16), (4, 24), (4, 28)]),
    (20, 16, 196, 0x7c94, [(2, 20), (4, 18), (6, 18), (5, 26)]),
    (22, 18, 242, 0x85bc, [(2, 24), (4, 22), (6, 22), (6, 26)]),
    (24, 20, 292, 0x9a99, [(2, 30), (5, 22), (8, 20), (8, 24)]),
    (26, 22, 346, 0xa4d3, [(4, 18), (5, 26), (8, 24), (8, 28)]),
    (28, 24, 404, 0xbbf6, [(4, 20), (5, 30), (8, 28), (11, 24)]),
    (30, 26, 466, 0xc762, [(4, 24), (8, 22), (10, 26), (11, 28)]),
    (32, 28, 532, 0xd847, [(4, 26), (9, 22), (12, 24), (16, 22)]),
    (24, 20, 581, 0xe60d, [(4, 30), (9, 24), (16, 20), (16, 24)]),
    (24, 22, 655, 0xf928, [(6, 22), (10, 24), (12, 30), (18, 24)]),
    (
        24,
        24,
        733,
        0x10b78,
        [(6, 24), (10, 28), (17, 24), (16, 30)],
    ),
    (
        28,
        24,
        815,
        0x1145d,
        [(6, 28), (11, 28), (16, 28), (19, 28)],
    ),
    (
        28,
        26,
        901,
        0x12a17,
        [(6, 30), (13, 26), (18, 28), (21, 28)],
    ),
    (
        28,
        28,
        991,
        0x13532,
        [(7, 28), (14, 26), (21, 26), (25, 26)],
    ),
    (
        32,
        28,
        1085,
        0x149a6,
        [(8, 28), (16, 26), (20, 30), (25, 28)],
    ),
    (
        26,
        22,
        1156,
        0x15683,
        [(8, 28), (17, 26), (23, 28), (25, 30)],
    ),
    (
        24,
        24,
        1258,
        0x168c9,
        [(9, 28), (17, 28), (23, 30), (34, 24)],
    ),
    (
        28,
        24,
        1364,
        0x177ec,
        [(9, 30), (18, 28), (25, 30), (30, 30)],
    ),
    (
        26,
        26,
        1474,
        0x18ec4,
        [(10, 30), (20, 28), (27, 30), (32, 30)],
    ),
    (
        30,
        26,
        1588,
        0x191e1,
        [(12, 26), (21, 28), (29, 30), (35, 30)],
    ),
    (
        28,
        28,
        1706,
        0x1afab,
        [(12, 28), (23, 28), (34, 28), (37, 30)],
    ),
    (
        32,
        28,
        1828,
        0x1b08e,
        [(12, 30), (25, 28), (34, 30), (40, 30)],
    ),
    (
        24,
        24,
        1921,
        0x1cc1a,
        [(13, 30), (26, 28), (35, 30), (42, 30)],
    ),
    (
        28,
        24,
        2051,
        0x1d33f,
        [(14, 30), (28, 28), (38, 30), (45, 30)],
    ),
    (
        24,
        26,
        2185,
        0x1ed75,
        [(15, 30), (29, 28), (40, 30), (48, 30)],
    ),
    (
        28,
        26,
        2323,
        0x1f250,
        [(16, 30), (31, 28), (43, 30), (51, 30)],
    ),
    (
        32,
        26,
        2465,
        0x209d5,
        [(17, 30), (33, 28), (45, 30), (54, 30)],
    ),
    (
        28,
        28,
        2611,
        0x216f0,
        [(18, 30), (35, 28), (48, 30), (57, 30)],
    ),
    (
        32,
        28,
        2761,
        0x228ba,
        [(19, 30), (37, 28), (51, 30), (60, 30)],
    ),
    (
        28,
        24,
        2876,
        0x2379f,
        [(19, 30), (38, 28), (53, 30), (63, 30)],
    ),
    (
        22,
        26,
        3034,
        0x24b0b,
        [(20, 30), (40, 28), (56, 30), (66, 30)],
    ),
    (
        26,
        26,
        3196,
        0x2542e,
        [(21, 30), (43, 28), (59, 30), (70, 30)],
    ),
    (
        30,
        26,
        3362,
        0x26a64,
        [(22, 30), (45, 28), (62, 30), (74, 30)],
    ),
    (
        24,
        28,
        3532,
        0x27541,
        [(24, 30), (47, 28), (65, 30), (77, 30)],
    ),
    (
        28,
        28,
        3706,
        0x28c69,
        [(25, 30), (49, 28), (68, 30), (81, 30)],
    ),
];

/// The level the web uses: L, the lowest.
const LEVEL_L: usize = 0;

fn data_bytes(v: usize, l: usize) -> usize {
    let (_, _, bytes, _, lev) = VTAB[v];
    bytes - lev[l].0 * lev[l].1
}

fn size_class(v: usize) -> usize {
    if v <= 9 {
        0
    } else if v <= 26 {
        1
    } else {
        2
    }
}

const ALPHABET: &str = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ $%*+-./:";

enum Enc<'a> {
    Num(&'a str),
    Alpha(&'a str),
    Str(&'a str),
}

impl Enc<'_> {
    fn bits(&self, v: usize) -> usize {
        match self {
            Enc::Num(s) => 4 + [10, 12, 14][size_class(v)] + (10 * s.len() + 2) / 3,
            Enc::Alpha(s) => 4 + [9, 11, 13][size_class(v)] + (11 * s.len() + 1) / 2,
            Enc::Str(s) => 4 + [8, 16, 16][size_class(v)] + 8 * s.len(),
        }
    }

    fn encode(&self, b: &mut Bits, v: usize) {
        match self {
            Enc::Num(s) => {
                let s = s.as_bytes();
                b.write(1, 4);
                b.write(s.len() as u32, [10, 12, 14][size_class(v)]);
                let d = |c: u8| (c - b'0') as u32;
                let mut i = 0;
                while i + 3 <= s.len() {
                    b.write(d(s[i]) * 100 + d(s[i + 1]) * 10 + d(s[i + 2]), 10);
                    i += 3;
                }
                match s.len() - i {
                    1 => b.write(d(s[i]), 4),
                    2 => b.write(d(s[i]) * 10 + d(s[i + 1]), 7),
                    _ => {}
                }
            }
            Enc::Alpha(s) => {
                let s = s.as_bytes();
                b.write(2, 4);
                b.write(s.len() as u32, [9, 11, 13][size_class(v)]);
                let idx = |c: u8| ALPHABET.find(c as char).unwrap_or(0) as u32;
                let mut i = 0;
                while i + 2 <= s.len() {
                    b.write(idx(s[i]) * 45 + idx(s[i + 1]), 11);
                    i += 2;
                }
                if i < s.len() {
                    b.write(idx(s[i]), 6);
                }
            }
            Enc::Str(s) => {
                b.write(4, 4);
                b.write(s.len() as u32, [8, 16, 16][size_class(v)]);
                for &c in s.as_bytes() {
                    b.write(c as u32, 8);
                }
            }
        }
    }
}

// Pixel bits: 1 black, 2 invert, roles at bits 2..6, offset from bit 6.
const BLACK: u32 = 1;
const INVERT: u32 = 2;
const POSITION: u32 = 1;
const ALIGNMENT: u32 = 2;
const TIMING: u32 = 3;
const FORMAT: u32 = 4;
const PVERSION: u32 = 5;
const UNUSED: u32 = 6;
const DATA: u32 = 7;
const CHECK: u32 = 8;
const EXTRA: u32 = 9;

fn role_px(r: u32) -> u32 {
    r << 2
}

fn role_of(p: u32) -> u32 {
    (p >> 2) & 15
}

fn offset_px(o: u32) -> u32 {
    o << 6
}

fn pos_box(m: &mut [Vec<u32>], x: usize, y: usize) {
    let pos = role_px(POSITION);
    for dy in 0..7 {
        for dx in 0..7 {
            let mut p = pos;
            if dx == 0
                || dx == 6
                || dy == 0
                || dy == 6
                || (2..=4).contains(&dx) && (2..=4).contains(&dy)
            {
                p |= BLACK;
            }
            m[y + dy][x + dx] = p;
        }
    }
    let n = m.len() as i64;
    let (x, y) = (x as i64, y as i64);
    for dy in -1..8i64 {
        if 0 <= y + dy && y + dy < n {
            if x > 0 {
                m[(y + dy) as usize][(x - 1) as usize] = pos;
            }
            if x + 7 < n {
                m[(y + dy) as usize][(x + 7) as usize] = pos;
            }
        }
    }
    for dx in -1..8i64 {
        if 0 <= x + dx && x + dx < n {
            if y > 0 {
                m[(y - 1) as usize][(x + dx) as usize] = pos;
            }
            if y + 7 < n {
                m[(y + 7) as usize][(x + dx) as usize] = pos;
            }
        }
    }
}

fn align_box(m: &mut [Vec<u32>], x: usize, y: usize) {
    let align = role_px(ALIGNMENT);
    for dy in 0..5 {
        for dx in 0..5 {
            let mut p = align;
            if dx == 0 || dx == 4 || dy == 0 || dy == 4 || dx == 2 && dy == 2 {
                p |= BLACK;
            }
            m[y + dy][x + dx] = p;
        }
    }
}

/// The pixel plan of version v at level l with mask 0.
fn plan(v: usize, l: usize) -> Vec<Vec<u32>> {
    let siz = 17 + v * 4;
    let mut m = vec![vec![0u32; siz]; siz];
    for i in 0..siz {
        let mut p = role_px(TIMING);
        if i & 1 == 0 {
            p |= BLACK;
        }
        m[i][6] = p;
        m[6][i] = p;
    }
    pos_box(&mut m, 0, 0);
    pos_box(&mut m, siz - 7, 0);
    pos_box(&mut m, 0, siz - 7);
    let (apos, astride, bytes, pattern, levels) = VTAB[v];
    let mut x = 4;
    while x + 5 < siz {
        let mut y = 4;
        while y + 5 < siz {
            let skip =
                (x < 7 && y < 7) || (x < 7 && y + 5 >= siz - 7) || (x + 5 >= siz - 7 && y < 7);
            if !skip {
                align_box(&mut m, x, y);
            }
            if y == 4 {
                y = apos;
            } else {
                y += astride;
            }
        }
        if x == 4 {
            x = apos;
        } else {
            x += astride;
        }
    }
    if pattern != 0 {
        let mut pv = pattern;
        for x in 0..6 {
            for y in 0..3 {
                let mut p = role_px(PVERSION);
                if pv & 1 != 0 {
                    p |= BLACK;
                }
                m[siz - 11 + y][x] = p;
                m[x][siz - 11 + y] = p;
                pv >>= 1;
            }
        }
    }
    m[siz - 8][8] = role_px(UNUSED) | BLACK;

    // The format bits, mask 0.
    let mut fb: u32 = ((l as u32) ^ 1) << 13;
    let format_poly: u32 = 0x537;
    let mut rem = fb;
    for i in (10..=14).rev() {
        if rem & (1 << i) != 0 {
            rem ^= format_poly << (i - 10);
        }
    }
    fb |= rem;
    let invert: u32 = 0x5412;
    for i in 0..15u32 {
        let mut pix = role_px(FORMAT) + offset_px(i);
        if (fb >> i) & 1 == 1 {
            pix |= BLACK;
        }
        if (invert >> i) & 1 == 1 {
            pix ^= INVERT | BLACK;
        }
        let iu = i as usize;
        match iu {
            0..=5 => m[iu][8] = pix,
            6..=7 => m[iu + 1][8] = pix,
            8 => m[8][7] = pix,
            _ => m[8][14 - iu] = pix,
        }
        if iu < 8 {
            m[8][siz - 1 - iu] = pix;
        } else {
            m[siz - 1 - (14 - iu)][8] = pix;
        }
    }

    // The data and check bits, interleaved by block.
    let (nblock, ne) = levels[l];
    let nde = (bytes - ne * nblock) / nblock;
    let extra = (bytes - ne * nblock) % nblock;
    let data_bits = (nde * nblock + extra) * 8;
    let check_bits = ne * nblock * 8;
    let data: Vec<u32> = (0..data_bits)
        .map(|i| role_px(DATA) | offset_px(i as u32))
        .collect();
    let check: Vec<u32> = (0..check_bits)
        .map(|i| role_px(CHECK) | offset_px((i + data_bits) as u32))
        .collect();
    let mut data_list = Vec::new();
    let mut check_list = Vec::new();
    let (mut d0, mut c0) = (0, 0);
    for i in 0..nblock {
        let mut nd = nde;
        if i >= nblock - extra {
            nd += 1;
        }
        data_list.push(&data[d0..d0 + nd * 8]);
        check_list.push(&check[c0..c0 + ne * 8]);
        d0 += nd * 8;
        c0 += ne * 8;
    }
    let mut bits: Vec<u32> = Vec::with_capacity(data_bits + check_bits + 7);
    for i in 0..nde + 1 {
        for b in &data_list {
            if i * 8 < b.len() {
                bits.extend_from_slice(&b[i * 8..(i + 1) * 8]);
            }
        }
    }
    for i in 0..ne {
        for b in &check_list {
            if i * 8 < b.len() {
                bits.extend_from_slice(&b[i * 8..(i + 1) * 8]);
            }
        }
    }
    bits.extend(std::iter::repeat_n(role_px(EXTRA), 7));
    let mut src = bits.into_iter();
    let mut x = siz;
    while x > 0 {
        for y in (0..siz).rev() {
            if role_of(m[y][x - 1]) == 0 {
                m[y][x - 1] = src.next().unwrap_or(0);
            }
            if role_of(m[y][x - 2]) == 0 {
                m[y][x - 2] = src.next().unwrap_or(0);
            }
        }
        x -= 2;
        if x == 7 {
            x -= 1;
        }
        for y in 0..siz {
            if role_of(m[y][x - 1]) == 0 {
                m[y][x - 1] = src.next().unwrap_or(0);
            }
            if role_of(m[y][x - 2]) == 0 {
                m[y][x - 2] = src.next().unwrap_or(0);
            }
        }
        x -= 2;
    }

    // Mask 0: (i+j)%2 == 0 inverts data, check and extra pixels.
    for (y, row) in m.iter_mut().enumerate() {
        for (x, pix) in row.iter_mut().enumerate() {
            let r = role_of(*pix);
            if (r == DATA || r == CHECK || r == EXTRA) && (y + x) % 2 == 0 {
                *pix ^= BLACK | INVERT;
            }
        }
    }
    m
}

/// The black and white cells of the code for text at level L, or None
/// when the text is too long for any version.
pub fn encode(text: &str) -> Option<Vec<Vec<bool>>> {
    let enc = if text.bytes().all(|c| c.is_ascii_digit()) {
        Enc::Num(text)
    } else if text.chars().all(|c| ALPHABET.contains(c)) {
        Enc::Alpha(text)
    } else {
        Enc::Str(text)
    };
    let l = LEVEL_L;
    let mut v = 1;
    loop {
        if v > 40 {
            return None;
        }
        if enc.bits(v) <= data_bytes(v, l) * 8 {
            break;
        }
        v += 1;
    }
    let pix = plan(v, l);
    let mut b = Bits::default();
    enc.encode(&mut b, v);
    let nd = data_bytes(v, l);
    if b.nbit < nd * 8 {
        b.pad(nd * 8 - b.nbit);
    }
    let field = Field::new();
    let (nblock, check) = VTAB[v].4[l];
    let mut db = nd / nblock;
    let extra = nd % nblock;
    let data = b.b.clone();
    let mut all = data.clone();
    let mut at = 0;
    for i in 0..nblock {
        if i == nblock - extra {
            db += 1;
        }
        all.extend(field.ecc(&data[at..at + db], check));
        at += db;
    }
    Some(
        pix.iter()
            .map(|row| {
                row.iter()
                    .map(|&p| {
                        let mut p = p;
                        let r = role_of(p);
                        if r == DATA || r == CHECK {
                            let o = (p >> 6) as usize;
                            if all[o / 8] & (1 << (7 - o % 8)) != 0 {
                                p ^= BLACK;
                            }
                        }
                        p & BLACK != 0
                    })
                    .collect()
            })
            .collect(),
    )
}

/// The code for text drawn with half blocks and a quiet zone of 2, as
/// Go's WriteQR writes it. Text too long for a code draws nothing.
pub fn render(text: &str) -> String {
    let Some(code) = encode(text) else {
        return String::new();
    };
    let size = code.len();
    let black = |x: usize, y: usize| x < size && y < size && code[y][x];
    let quiet = 2;
    let (ww, bb, wb, bw) = ("█", " ", "▀", "▄");
    let mut out = String::new();
    let width = size + quiet * 2;
    for _ in 0..quiet / 2 {
        out.push_str(&ww.repeat(width));
        out.push('\n');
    }
    let mut i = 0;
    while i <= size {
        out.push_str(&ww.repeat(quiet));
        for j in 0..=size {
            let next_black = i + 1 < size && black(j, i + 1);
            let curr_black = black(j, i);
            out.push_str(match (curr_black, next_black) {
                (true, true) => bb,
                (true, false) => bw,
                (false, false) => ww,
                (false, true) => wb,
            });
        }
        out.push_str(&ww.repeat(quiet - 1));
        out.push('\n');
        i += 2;
    }
    for _ in 0..(quiet / 2).saturating_sub(1) {
        out.push_str(&ww.repeat(width));
        out.push('\n');
    }
    out.push_str(&wb.repeat(width));
    out.push('\n');
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_url_fits_the_terminal() {
        let s = render("http://coppice.test:9000/#t=pinned-web-token-0001");
        let widest = s.lines().map(|l| l.chars().count()).max().unwrap();
        assert!(widest <= MAX_QR_COLUMNS, "{widest}");
    }

    #[test]
    fn version_one_digits_start_with_a_finder() {
        let code = encode("01234567").unwrap();
        assert_eq!(code.len(), 21);
        assert!(code[0][..7].iter().all(|&b| b));
        assert!(!code[1][1]);
    }
}
