/* moonshine-cli: transcribe 16 kHz mono WAVs with a Moonshine model.
 *
 *   moonshine-cli -m MODEL_DIR -a ARCH -f CLIP.wav
 *   moonshine-cli -m MODEL_DIR -a ARCH --resident
 *
 * MODEL_DIR holds a Moonshine streaming model (encoder.ort, tokenizer.bin
 * and the rest). ARCH is tiny, small or medium: the streaming English
 * architectures of the Moonshine C API. A clip is a PCM WAV, 16 kHz, one
 * channel, 16-bit. Other WAVs are refused; the caller resamples first.
 *
 * With -f, stdout gets the text of each transcript line, one line each,
 * and nothing else. stderr gets the library's log, one timing line
 * (load_ms and decode_ms), and the reason for a failure. Exit codes: 0
 * done, 2 usage, 3 the model did not load, 4 the clip is not a WAV this
 * program reads, 5 the transcription failed.
 *
 * With --resident it loads the model once and answers clip after clip on
 * stdin and stdout: the daisugi-voice-1 protocol of
 * clients/native/common/daisugi-native.h, the transcript lines joined by
 * a space. The voice server runs it so.
 *
 * This is the smallest program that puts the Moonshine C API behind a
 * process boundary, so the Python, Go and Rust daisugi run it the same way.
 */
#include "daisugi-native.h"
#include "moonshine-c-api.h"

static int usage(void) {
  fprintf(stderr, "usage: moonshine-cli -m MODEL_DIR -a tiny|small|medium (-f CLIP.wav | --resident)\n");
  return 2;
}

/* The transcript lines that hold text, joined by one space. */
static char *join_lines(const struct transcript_t *t) {
  size_t len = 1;
  for (uint64_t i = 0; t != NULL && i < t->line_count; i++)
    if (t->lines[i].text != NULL) len += strlen(t->lines[i].text) + 1;
  char *out = malloc(len);
  if (out == NULL) return NULL;
  out[0] = '\0';
  for (uint64_t i = 0; t != NULL && i < t->line_count; i++) {
    const char *text = t->lines[i].text;
    if (text == NULL || text[0] == '\0') continue;
    if (out[0] != '\0') strcat(out, " ");
    strcat(out, text);
  }
  return out;
}

/* One second of silence. Moonshine keeps its voice detector's state from
 * one call to the next (one Silero VAD per process, never reset), so a
 * resident child would hear each clip through the end of the one before.
 * Running this silence first brings that state back to rest. */
static float rest[16000];

static char *transcribe(void *ctx, const float *samples, uint64_t n, const char **why) {
  int32_t handle = *(int32_t *)ctx;
  struct transcript_t *transcript = NULL;
  int32_t err = moonshine_transcribe_without_streaming(handle, rest, 16000, 16000, 0, &transcript);
  if (err != MOONSHINE_ERROR_NONE) {
    *why = moonshine_error_to_string(err);
    return NULL;
  }
  transcript = NULL;
  err = moonshine_transcribe_without_streaming(handle, (float *)samples, n, 16000, 0, &transcript);
  if (err != MOONSHINE_ERROR_NONE) {
    *why = moonshine_error_to_string(err);
    return NULL;
  }
  char *text = join_lines(transcript);
  if (text == NULL) *why = "out of memory";
  return text;
}

int main(int argc, char **argv) {
  const char *model = NULL, *arch_name = NULL, *clip = NULL;
  int resident = 0;
  for (int i = 1; i < argc; i++) {
    if (i + 1 < argc && strcmp(argv[i], "-m") == 0) {
      model = argv[++i];
    } else if (i + 1 < argc && strcmp(argv[i], "-a") == 0) {
      arch_name = argv[++i];
    } else if (i + 1 < argc && strcmp(argv[i], "-f") == 0) {
      clip = argv[++i];
    } else if (strcmp(argv[i], "--resident") == 0) {
      resident = 1;
    } else {
      return usage();
    }
  }
  if (model == NULL || arch_name == NULL || (clip == NULL) == (resident == 0)) return usage();
  uint32_t arch;
  if (strcmp(arch_name, "tiny") == 0) {
    arch = MOONSHINE_MODEL_ARCH_TINY_STREAMING;
  } else if (strcmp(arch_name, "small") == 0) {
    arch = MOONSHINE_MODEL_ARCH_SMALL_STREAMING;
  } else if (strcmp(arch_name, "medium") == 0) {
    arch = MOONSHINE_MODEL_ARCH_MEDIUM_STREAMING;
  } else {
    fprintf(stderr, "moonshine-cli: unknown arch %s; use tiny, small or medium\n", arch_name);
    return 2;
  }

  if (resident) {
    dn_die_with_parent();
    dn_take_stdout();
    double t0 = dn_now_ms();
    int32_t handle = moonshine_load_transcriber_from_files(model, arch, NULL, 0, MOONSHINE_HEADER_VERSION);
    if (handle < 0) {
      char why[512];
      snprintf(why, sizeof why, "the model in %s did not load: %s", model, moonshine_error_to_string(handle));
      dn_load_failed(why);
    }
    int code = dn_serve(&handle, transcribe, dn_now_ms() - t0);
    moonshine_free_transcriber(handle);
    return code;
  }

  size_t len = 0;
  unsigned char *bytes = dn_read_file(clip, &len);
  if (bytes == NULL) {
    fprintf(stderr, "moonshine-cli: cannot read %s\n", clip);
    return 4;
  }
  float *samples = NULL;
  uint64_t count = 0;
  const char *bad = dn_wav_samples(bytes, len, &samples, &count);
  free(bytes);
  if (bad != NULL) {
    fprintf(stderr, "moonshine-cli: %s: %s\n", clip, bad);
    return 4;
  }

  double t0 = dn_now_ms();
  int32_t handle = moonshine_load_transcriber_from_files(model, arch, NULL, 0, MOONSHINE_HEADER_VERSION);
  double t1 = dn_now_ms();
  if (handle < 0) {
    fprintf(stderr, "moonshine-cli: the model in %s did not load: %s\n", model, moonshine_error_to_string(handle));
    free(samples);
    return 3;
  }
  struct transcript_t *transcript = NULL;
  int32_t err = MOONSHINE_ERROR_NONE;
  if (count > 0) {
    err = moonshine_transcribe_without_streaming(handle, samples, count, 16000, 0, &transcript);
  }
  double t2 = dn_now_ms();
  free(samples);
  if (err != MOONSHINE_ERROR_NONE) {
    fprintf(stderr, "moonshine-cli: transcription failed: %s\n", moonshine_error_to_string(err));
    moonshine_free_transcriber(handle);
    return 5;
  }
  if (transcript != NULL) {
    for (uint64_t i = 0; i < transcript->line_count; i++) {
      const char *text = transcript->lines[i].text;
      if (text != NULL && text[0] != '\0') printf("%s\n", text);
    }
  }
  fflush(stdout);
  fprintf(stderr, "moonshine-cli: load_ms=%.0f decode_ms=%.0f\n", t1 - t0, t2 - t1);
  moonshine_free_transcriber(handle);
  return 0;
}
