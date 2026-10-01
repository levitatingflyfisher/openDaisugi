/* daisugi-native.h: what moonshine-cli and parakeet-cli share.
 *
 * The WAV reader, a clock, and the child side of the resident protocol
 * (daisugi-voice-1). The protocol is defined in one place,
 * src/opendaisugi/voice/resident.py; this file follows it:
 *
 *   ready line   {"ready":"daisugi-voice-1","load_ms":N}
 *   load error   {"error":"WHY"} and exit 3
 *   request      a 4-byte big-endian length N (1 .. DN_MAX_FRAME), then a
 *                16 kHz mono 16-bit PCM WAV of N bytes
 *   reply        {"text":"..."} or {"error":"WHY"}, one line
 *   end          end of file at a frame boundary: exit 0; a length of 0
 *                or over DN_MAX_FRAME, or end of file inside a frame: exit 2
 *
 * The child asks the kernel for SIGTERM when its parent dies, so it never
 * outlives the voice server. In resident mode fd 1 points at stderr and the
 * protocol has its own stream, so library output never mixes with it.
 */
#ifndef DAISUGI_NATIVE_H
#define DAISUGI_NATIVE_H

#include <signal.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/prctl.h>
#include <time.h>
#include <unistd.h>

#define DN_PROTOCOL "daisugi-voice-1"
#define DN_MAX_FRAME (64u * 1024u * 1024u)

static double dn_now_ms(void) {
  struct timespec ts;
  clock_gettime(CLOCK_MONOTONIC, &ts);
  return (double)ts.tv_sec * 1000.0 + (double)ts.tv_nsec / 1e6;
}

static uint32_t dn_le32(const unsigned char *p) {
  return (uint32_t)p[0] | ((uint32_t)p[1] << 8) | ((uint32_t)p[2] << 16) | ((uint32_t)p[3] << 24);
}

static uint16_t dn_le16(const unsigned char *p) { return (uint16_t)(p[0] | (p[1] << 8)); }

/* dn_read_file reads the whole file. It returns NULL on any error. */
static unsigned char *dn_read_file(const char *path, size_t *len) {
  FILE *f = fopen(path, "rb");
  if (f == NULL) return NULL;
  size_t cap = 1 << 16, n = 0;
  unsigned char *buf = malloc(cap);
  while (buf != NULL) {
    if (n == cap) {
      unsigned char *bigger = realloc(buf, cap * 2);
      if (bigger == NULL) {
        free(buf);
        buf = NULL;
        break;
      }
      buf = bigger;
      cap *= 2;
    }
    size_t got = fread(buf + n, 1, cap - n, f);
    n += got;
    if (got == 0) break;
  }
  if (buf != NULL && ferror(f)) {
    free(buf);
    buf = NULL;
  }
  fclose(f);
  *len = n;
  return buf;
}

/* dn_wav_samples finds the fmt and data chunks of a RIFF/WAVE file and
 * returns the samples as floats in [-1, 1]. A data size past the end of
 * the file (as a streamed WAV writes it) reads to the end. It returns a
 * reason on failure and NULL on success. */
static const char *dn_wav_samples(const unsigned char *b, size_t len, float **out, uint64_t *count) {
  if (len < 12 || memcmp(b, "RIFF", 4) != 0 || memcmp(b + 8, "WAVE", 4) != 0) return "not a RIFF/WAVE file";
  size_t pos = 12;
  int have_fmt = 0;
  while (pos + 8 <= len) {
    const unsigned char *id = b + pos;
    uint32_t size = dn_le32(b + pos + 4);
    size_t body = pos + 8;
    if (memcmp(id, "fmt ", 4) == 0) {
      if (size < 16 || body + 16 > len) return "short fmt chunk";
      if (dn_le16(b + body) != 1) return "not PCM";
      if (dn_le16(b + body + 2) != 1) return "not mono";
      if (dn_le32(b + body + 4) != 16000) return "not 16 kHz";
      if (dn_le16(b + body + 14) != 16) return "not 16-bit";
      have_fmt = 1;
    } else if (memcmp(id, "data", 4) == 0) {
      if (!have_fmt) return "data before fmt";
      size_t avail = len - body;
      size_t n = size > avail ? avail : size;
      uint64_t frames = n / 2;
      float *s = malloc((frames ? frames : 1) * sizeof(float));
      if (s == NULL) return "out of memory";
      for (uint64_t i = 0; i < frames; i++) {
        int16_t v = (int16_t)dn_le16(b + body + 2 * i);
        s[i] = (float)v / 32768.0f;
      }
      *out = s;
      *count = frames;
      return NULL;
    }
    if (size > len - body) break;
    pos = body + size + (size & 1);
  }
  return "no data chunk";
}

/* dn_put_json_string writes s as a JSON string. Control characters are
 * escaped, and a byte that does not start or continue valid UTF-8 becomes
 * U+FFFD, so the line is always JSON a parent can read. */
static void dn_put_json_string(FILE *f, const char *s) {
  const unsigned char *p = (const unsigned char *)s;
  fputc('"', f);
  while (*p) {
    unsigned char c = *p;
    if (c == '"' || c == '\\') {
      fputc('\\', f);
      fputc(c, f);
      p++;
    } else if (c < 0x20) {
      fprintf(f, "\\u%04x", c);
      p++;
    } else if (c < 0x80) {
      fputc(c, f);
      p++;
    } else {
      int n = (c & 0xE0) == 0xC0 ? 2 : (c & 0xF0) == 0xE0 ? 3 : (c & 0xF8) == 0xF0 ? 4 : 0;
      int ok = n > 0 && !(n == 2 && c < 0xC2) && !(n == 4 && c > 0xF4);
      for (int i = 1; ok && i < n; i++) ok = (p[i] & 0xC0) == 0x80;
      if (ok && n == 3 && ((c == 0xE0 && p[1] < 0xA0) || (c == 0xED && p[1] > 0x9F))) ok = 0;
      if (ok && n == 4 && ((c == 0xF0 && p[1] < 0x90) || (c == 0xF4 && p[1] > 0x8F))) ok = 0;
      if (ok) {
        fwrite(p, 1, (size_t)n, f);
        p += n;
      } else {
        fputs("\\ufffd", f);
        p++;
      }
    }
  }
  fputc('"', f);
}

/* The protocol's own stdout. dn_take_stdout points fd 1 at stderr, so a
 * library that prints to stdout cannot break a protocol line. */
static FILE *dn_out = NULL;

static void dn_take_stdout(void) {
  int fd = dup(1);
  if (fd >= 0 && dup2(2, 1) >= 0) dn_out = fdopen(fd, "w");
  if (dn_out == NULL) dn_out = stdout;
}

static void dn_reply(const char *key, const char *value) {
  if (dn_out == NULL) dn_out = stdout;
  fprintf(dn_out, "{\"%s\":", key);
  dn_put_json_string(dn_out, value);
  fputs("}\n", dn_out);
  fflush(dn_out);
}

/* dn_die_with_parent asks for SIGTERM when the parent ends. Call it first. */
static void dn_die_with_parent(void) {
  prctl(PR_SET_PDEATHSIG, SIGTERM);
  if (getppid() == 1) exit(0);
}

/* dn_load_failed writes the load error line and exits 3. */
static void dn_load_failed(const char *why) {
  dn_reply("error", why);
  exit(3);
}

static int dn_read_exact(unsigned char *buf, size_t n) {
  size_t got = 0;
  while (got < n) {
    size_t r = fread(buf + got, 1, n - got, stdin);
    if (r == 0) break;
    got += r;
  }
  return got == n ? 0 : (got == 0 ? -1 : -2);
}

/* A transcribe function: the text of n samples (malloc'd, the caller frees
 * it), or NULL with *why set to a reason. */
typedef char *(*dn_transcribe_fn)(void *ctx, const float *samples, uint64_t n, const char **why);

/* dn_serve writes the ready line, then answers frames until stdin ends.
 * It returns the exit code. */
static int dn_serve(void *ctx, dn_transcribe_fn transcribe, double load_ms) {
  if (dn_out == NULL) dn_out = stdout;
  fprintf(dn_out, "{\"ready\":\"" DN_PROTOCOL "\",\"load_ms\":%.0f}\n", load_ms);
  fflush(dn_out);
  for (;;) {
    unsigned char head[4];
    int r = dn_read_exact(head, 4);
    if (r == -1) return 0;
    if (r != 0) return 2;
    uint32_t n = ((uint32_t)head[0] << 24) | ((uint32_t)head[1] << 16) | ((uint32_t)head[2] << 8) | head[3];
    if (n == 0 || n > DN_MAX_FRAME) return 2;
    unsigned char *wav = malloc(n);
    if (wav == NULL) return 2;
    if (dn_read_exact(wav, n) != 0) {
      free(wav);
      return 2;
    }
    float *samples = NULL;
    uint64_t count = 0;
    const char *bad = dn_wav_samples(wav, n, &samples, &count);
    free(wav);
    if (bad != NULL) {
      char why[128];
      snprintf(why, sizeof why, "the clip is not a 16 kHz mono 16-bit WAV: %s", bad);
      dn_reply("error", why);
      continue;
    }
    double t0 = dn_now_ms();
    const char *why = "transcription failed";
    char *text = count > 0 ? transcribe(ctx, samples, count, &why) : strdup("");
    free(samples);
    if (text == NULL) {
      dn_reply("error", why);
      continue;
    }
    fputs("{\"text\":", dn_out);
    dn_put_json_string(dn_out, text);
    fprintf(dn_out, ",\"decode_ms\":%.0f}\n", dn_now_ms() - t0);
    fflush(dn_out);
    free(text);
  }
}

#endif
