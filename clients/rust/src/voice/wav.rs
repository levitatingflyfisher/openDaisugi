//! WAV reading as Python's `wave` module reads it, the 16-bit mono writer,
//! and the linear resample (`voice/audio.py`).

/// `KSDATAFORMAT_SUBTYPE_PCM`.
const PCM_GUID: &[u8; 16] = b"\x01\x00\x00\x00\x00\x00\x10\x00\x80\x00\x00\xaa\x00\x38\x9b\x71";

/// A clip that cannot be normalized. `kind` is the Python exception the
/// oracle raises: "unsupported" (AudioFormatUnsupported), "wave"
/// (wave.Error), "value", "runtime", "eof", "zerodiv", "struct",
/// "timeout" or "oserror".
#[derive(Debug, Clone)]
pub struct AudioError {
    pub kind: &'static str,
    pub msg: String,
}

impl AudioError {
    pub fn new(kind: &'static str, msg: &str) -> AudioError {
        AudioError { kind, msg: msg.to_string() }
    }

    /// Whether the server answers this 400 bad_audio; any other is a 500.
    pub fn bad_audio(&self) -> bool {
        matches!(self.kind, "unsupported" | "wave" | "value" | "runtime")
    }
}

type AR<T> = Result<T, AudioError>;

/// A stream as `wave` reads one: the bytes, or a chunk inside them.
trait Reader {
    fn read(&mut self, n: i64) -> Vec<u8>;
    fn seek(&mut self, pos: i64, whence: i32) -> AR<()>;
    fn tell(&self) -> i64;
}

struct PyFile<'a> {
    b: &'a [u8],
    pos: i64,
}

impl Reader for PyFile<'_> {
    fn read(&mut self, n: i64) -> Vec<u8> {
        let len = self.b.len() as i64;
        if self.pos >= len {
            return vec![];
        }
        let end = if n >= 0 && self.pos + n < len { self.pos + n } else { len };
        let out = self.b[self.pos as usize..end as usize].to_vec();
        self.pos = end;
        out
    }

    fn seek(&mut self, pos: i64, whence: i32) -> AR<()> {
        self.pos = if whence == 1 { self.pos + pos } else { pos };
        Ok(())
    }

    fn tell(&self) -> i64 {
        self.pos
    }
}

/// `wave._Chunk`, little-endian and word-aligned.
struct Chunk<'r> {
    f: &'r mut dyn Reader,
    name: Vec<u8>,
    size: i64,
    size_read: i64,
    offset: i64,
}

fn eof() -> AudioError {
    AudioError::new("eof", "EOFError")
}

impl<'r> Chunk<'r> {
    fn new(f: &'r mut dyn Reader) -> AR<Chunk<'r>> {
        let name = f.read(4);
        if name.len() < 4 {
            return Err(eof());
        }
        let sz = f.read(4);
        if sz.len() < 4 {
            return Err(eof());
        }
        let size = u32::from_le_bytes([sz[0], sz[1], sz[2], sz[3]]) as i64;
        let offset = f.tell();
        Ok(Chunk { f, name, size, size_read: 0, offset })
    }

    fn skip(&mut self) -> AR<()> {
        let mut n = self.size - self.size_read;
        if self.size & 1 == 1 {
            n += 1;
        }
        self.f.seek(n, 1)?;
        self.size_read += n;
        Ok(())
    }
}

impl Reader for Chunk<'_> {
    fn read(&mut self, n: i64) -> Vec<u8> {
        if self.size_read >= self.size {
            return vec![];
        }
        let n = if n < 0 || n > self.size - self.size_read { self.size - self.size_read } else { n };
        let data = self.f.read(n);
        self.size_read += data.len() as i64;
        if self.size_read == self.size && self.size & 1 == 1 {
            self.size_read += self.f.read(1).len() as i64;
        }
        data
    }

    fn seek(&mut self, pos: i64, whence: i32) -> AR<()> {
        let pos = match whence {
            1 => pos + self.size_read,
            2 => pos + self.size,
            _ => pos,
        };
        if pos < 0 || pos > self.size {
            return Err(AudioError::new("runtime", "RuntimeError"));
        }
        self.f.seek(self.offset + pos, 0)?;
        self.size_read = pos;
        Ok(())
    }

    fn tell(&self) -> i64 {
        self.size_read
    }
}

/// What `wave.open(io.BytesIO(raw))` reads, and the first `readframes`
/// of the whole data.
pub struct WaveRead {
    pub channels: i64,
    pub rate: i64,
    pub sampwidth: i64,
    pub frames: Vec<u8>,
}

fn read_fmt(c: &mut Chunk, w: &mut WaveRead) -> AR<()> {
    let b = c.read(14);
    if b.len() < 14 {
        return Err(eof());
    }
    let tag = u16::from_le_bytes([b[0], b[1]]);
    w.channels = u16::from_le_bytes([b[2], b[3]]) as i64;
    w.rate = u32::from_le_bytes([b[4], b[5], b[6], b[7]]) as i64;
    if tag != 1 && tag != 0xFFFE {
        return Err(AudioError::new("wave", "unknown format"));
    }
    let b = c.read(2);
    if b.len() < 2 {
        return Err(eof());
    }
    let bits = u16::from_le_bytes([b[0], b[1]]) as i64;
    if tag == 0xFFFE {
        if c.read(8).len() < 8 {
            return Err(eof());
        }
        let sub = c.read(16);
        if sub.len() < 16 {
            return Err(eof());
        }
        if sub.as_slice() != PCM_GUID {
            return Err(AudioError::new("wave", "unknown extended format"));
        }
    }
    w.sampwidth = (bits + 7) / 8;
    if w.sampwidth == 0 {
        return Err(AudioError::new("wave", "bad sample width"));
    }
    if w.channels == 0 {
        return Err(AudioError::new("wave", "bad # of channels"));
    }
    Ok(())
}

/// Opens raw as `wave` does and reads all its frames.
pub fn open_wave(raw: &[u8]) -> AR<WaveRead> {
    let mut file = PyFile { b: raw, pos: 0 };
    let mut outer = Chunk::new(&mut file)?;
    if outer.name != b"RIFF" {
        return Err(AudioError::new("wave", "file does not start with RIFF id"));
    }
    if outer.read(4) != b"WAVE" {
        return Err(AudioError::new("wave", "not a WAVE file"));
    }
    let mut w = WaveRead { channels: 0, rate: 0, sampwidth: 0, frames: vec![] };
    let mut fmt_read = false;
    loop {
        let mut sub = match Chunk::new(&mut outer) {
            Ok(c) => c,
            Err(_) => break,
        };
        if sub.name == b"fmt " {
            read_fmt(&mut sub, &mut w)?;
            fmt_read = true;
        } else if sub.name == b"data" {
            if !fmt_read {
                return Err(AudioError::new("wave", "data chunk before fmt chunk"));
            }
            let framesize = w.channels * w.sampwidth;
            let nframes = sub.size / framesize;
            w.frames = if nframes == 0 { vec![] } else { sub.read(nframes * framesize) };
            return Ok(w);
        }
        sub.skip()?;
    }
    if !fmt_read {
        return Err(AudioError::new("wave", "fmt chunk and/or data chunk missing"));
    }
    Err(AudioError::new("wave", "fmt chunk and/or data chunk missing"))
}

/// `audio.wav_duration_s`: the frames actually present, over the rate.
pub fn wav_duration_s(raw: &[u8]) -> AR<f64> {
    let w = open_wave(raw)?;
    let n = w.frames.len() as i64 / (w.sampwidth * w.channels);
    if w.rate == 0 {
        return Err(AudioError::new("zerodiv", "float division by zero"));
    }
    Ok(n as f64 / w.rate as f64)
}

/// `audio._read_wav_mono_samples`: the rate and the 16-bit samples, the
/// channels averaged.
pub fn read_mono_samples(raw: &[u8]) -> AR<(i64, Vec<f64>)> {
    let w = open_wave(raw)?;
    if w.sampwidth != 2 {
        return Err(AudioError::new("unsupported", "unsupported sample width"));
    }
    if w.frames.len() % 2 != 0 {
        return Err(AudioError::new("value", "buffer size must be a multiple of element size"));
    }
    let ints: Vec<i64> = w.frames.chunks(2).map(|c| i16::from_le_bytes([c[0], c[1]]) as i64).collect();
    if w.channels <= 1 {
        return Ok((w.rate, ints.iter().map(|&v| v as f64).collect()));
    }
    if ints.len() as i64 % w.channels != 0 {
        return Err(AudioError::new("value", "cannot reshape array"));
    }
    // The sum of int16 values is exact in an f64 whatever order numpy adds
    // them in; the mean is one rounded division.
    let out = ints.chunks(w.channels as usize).map(|row| row.iter().sum::<i64>() as f64 / w.channels as f64).collect();
    Ok((w.rate, out))
}

/// `audio.write_wav_16k_mono`: clipped, cut to int16, at `rate`.
pub fn write_wav_mono(samples: &[f64], rate: i64) -> AR<Vec<u8>> {
    if rate <= 0 {
        return Err(AudioError::new("wave", "bad frame rate"));
    }
    if rate * 2 > u32::MAX as i64 {
        return Err(AudioError::new("struct", "argument out of range"));
    }
    let mut data = Vec::with_capacity(samples.len() * 2);
    for &s in samples {
        let c = s.clamp(-32768.0, 32767.0).trunc() as i16;
        data.extend_from_slice(&c.to_le_bytes());
    }
    write_wav_pcm(&data, rate)
}

/// A mono 16-bit WAV of these raw frame bytes at `rate`: the header's
/// sizes count every byte written, an odd one too.
pub fn write_wav_pcm(data: &[u8], rate: i64) -> AR<Vec<u8>> {
    if data.len() as i64 + 36 > u32::MAX as i64 {
        return Err(AudioError::new("struct", "argument out of range"));
    }
    let mut h = Vec::with_capacity(44 + data.len());
    h.extend_from_slice(b"RIFF");
    h.extend_from_slice(&(36 + data.len() as u32).to_le_bytes());
    h.extend_from_slice(b"WAVEfmt ");
    h.extend_from_slice(&16u32.to_le_bytes());
    h.extend_from_slice(&1u16.to_le_bytes());
    h.extend_from_slice(&1u16.to_le_bytes());
    h.extend_from_slice(&(rate as u32).to_le_bytes());
    h.extend_from_slice(&((rate * 2) as u32).to_le_bytes());
    h.extend_from_slice(&2u16.to_le_bytes());
    h.extend_from_slice(&16u16.to_le_bytes());
    h.extend_from_slice(b"data");
    h.extend_from_slice(&(data.len() as u32).to_le_bytes());
    h.extend_from_slice(data);
    Ok(h)
}

/// A window is at most this long (see `split_wav`).
pub const WINDOW_SECONDS: f64 = 30.0;
/// A cut falls in the quietest 20 ms frame of this last stretch of a window.
pub const CUT_SEARCH_SECONDS: f64 = 3.0;
const CUT_FRAME_SAMPLES: usize = 320;

/// Cuts a 16 kHz mono WAV into windows of at most `WINDOW_SECONDS`, each cut
/// at the quietest 20 ms frame in the last `CUT_SEARCH_SECONDS` of a window,
/// so a cut lands in a pause and not in a word. The windows do not overlap.
/// A clip that fits in one window comes back whole.
pub fn split_wav(wav: &[u8]) -> AR<Vec<Vec<u8>>> {
    let w = open_wave(wav)?;
    if w.sampwidth != 2 || w.channels != 1 || w.rate <= 0 {
        return Ok(vec![wav.to_vec()]);
    }
    let pcm = &w.frames;
    let total = pcm.len() / 2;
    let window = (WINDOW_SECONDS * w.rate as f64) as usize;
    if total <= window {
        return Ok(vec![wav.to_vec()]);
    }
    let search = (CUT_SEARCH_SECONDS * w.rate as f64) as usize;
    let mut out = Vec::new();
    let mut pos = 0usize;
    while total - pos > window {
        let cut = quietest_cut(pcm, pos + window - search, pos + window);
        out.push(write_wav_pcm(&pcm[2 * pos..2 * cut], w.rate)?);
        pos = cut;
    }
    out.push(write_wav_pcm(&pcm[2 * pos..], w.rate)?);
    Ok(out)
}

/// Keeps the first `max_seconds` of a 16 kHz mono WAV. Returns the clip and
/// the whole seconds cut from its end, rounded up, so the reply can say how
/// much was left out. A clip that fits comes back as it is, with 0 cut.
/// Frames are counted as integers, so every port cuts at one sample.
pub fn truncate_wav(wav: &[u8], max_seconds: f64) -> AR<(Vec<u8>, i64)> {
    let w = open_wave(wav)?;
    if w.sampwidth != 2 || w.channels != 1 || w.rate <= 0 {
        return Ok((wav.to_vec(), 0));
    }
    let total = (w.frames.len() / 2) as i64;
    let keep = (max_seconds * w.rate as f64) as i64;
    if total <= keep {
        return Ok((wav.to_vec(), 0));
    }
    let piece = write_wav_pcm(&w.frames[..2 * keep as usize], w.rate)?;
    Ok((piece, (total - keep + w.rate - 1) / w.rate))
}

/// The sample index in `[lo, hi]` at the middle of the quietest 20 ms frame
/// inside the range. The first of equal frames wins.
fn quietest_cut(pcm: &[u8], lo: usize, hi: usize) -> usize {
    let mut best = u64::MAX;
    let mut best_at = (lo + hi) / 2;
    let mut start = lo;
    while start + CUT_FRAME_SAMPLES <= hi {
        let mut e: u64 = 0;
        for i in start..start + CUT_FRAME_SAMPLES {
            let v = i16::from_le_bytes([pcm[2 * i], pcm[2 * i + 1]]) as i64;
            e += (v * v) as u64;
        }
        if e < best {
            best = e;
            best_at = start + CUT_FRAME_SAMPLES / 2;
        }
        start += CUT_FRAME_SAMPLES / 2;
    }
    best_at
}

/// `audio._resample_linear`: numpy.interp at the target rate's times.
pub fn resample_linear(samples: &[f64], orig: i64, target: i64) -> AR<Vec<f64>> {
    if orig == target {
        return Ok(samples.to_vec());
    }
    if orig == 0 {
        return Err(AudioError::new("zerodiv", "division by zero"));
    }
    let duration = samples.len() as f64 / orig as f64;
    let n_target = ((duration * target as f64).round_ties_even() as i64).max(1);
    let n = samples.len();
    if n == 0 {
        return Err(AudioError::new("value", "array of sample points is empty"));
    }
    let xp: Vec<f64> = (0..n).map(|i| i as f64 / orig as f64).collect();
    if n == 1 {
        return Ok(vec![samples[0]; n_target as usize]);
    }
    let slopes: Vec<f64> = (0..n - 1).map(|i| (samples[i + 1] - samples[i]) / (xp[i + 1] - xp[i])).collect();
    let mut out = Vec::with_capacity(n_target as usize);
    let mut j = 0usize;
    for i in 0..n_target {
        let x = i as f64 / target as f64;
        while j + 1 < n && xp[j + 1] <= x {
            j += 1;
        }
        let v = if x > xp[n - 1] || j == n - 1 || xp[j] == x {
            if x > xp[n - 1] {
                samples[n - 1]
            } else {
                samples[j]
            }
        } else {
            let slope = slopes[j];
            let mut v = slope * (x - xp[j]) + samples[j];
            if v.is_nan() {
                v = slope * (x - xp[j + 1]) + samples[j + 1];
                if v.is_nan() && samples[j] == samples[j + 1] {
                    v = samples[j];
                }
            }
            v
        };
        out.push(v);
    }
    Ok(out)
}

/// `audio._is_wav`.
pub fn is_wav(raw: &[u8]) -> bool {
    raw.len() >= 12 && &raw[..4] == b"RIFF" && &raw[8..12] == b"WAVE"
}
