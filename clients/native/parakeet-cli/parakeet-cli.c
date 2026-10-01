/* parakeet-cli: transcribe 16 kHz mono WAVs with a Parakeet-TDT GGUF, on
 * CrispASR's Parakeet code and the ggml CPU backend.
 *
 *   parakeet-cli -m MODEL.gguf -f CLIP.wav [-t THREADS]
 *   parakeet-cli -m MODEL.gguf --resident [-t THREADS]
 *
 * With -f it transcribes one clip: the text on stdout, one timing line
 * (load_ms and decode_ms) and any reason for a failure on stderr. Exit
 * codes: 0 done, 2 usage, 3 the model did not load, 4 the clip is not a
 * WAV this program reads, 5 the transcription failed.
 *
 * With --resident it loads the model once and answers clip after clip on
 * stdin and stdout: the daisugi-voice-1 protocol of
 * clients/native/common/daisugi-native.h. The voice server runs it so.
 *
 * THREADS defaults to the CPUs online, at most 8. Decoding is greedy, on
 * the CPU, with flash attention in the encoder.
 */
#include "daisugi-native.h"
#include "parakeet.h"

static int usage(void) {
  fprintf(stderr, "usage: parakeet-cli -m MODEL.gguf (-f CLIP.wav | --resident) [-t THREADS]\n");
  return 2;
}

static char *transcribe(void *ctx, const float *samples, uint64_t n, const char **why) {
  if (n > 0x7fffffff) {
    *why = "the clip is too long";
    return NULL;
  }
  char *text = parakeet_transcribe((struct parakeet_context *)ctx, samples, (int)n);
  if (text == NULL) *why = "transcription failed";
  return text;
}

static struct parakeet_context *load(const char *model, int threads) {
  struct parakeet_context_params p = parakeet_context_default_params();
  p.n_threads = threads;
  p.verbosity = 0;
  p.use_gpu = false;
  return parakeet_init_from_file(model, p);
}

int main(int argc, char **argv) {
  const char *model = NULL, *clip = NULL;
  int resident = 0;
  long cpus = sysconf(_SC_NPROCESSORS_ONLN);
  int threads = cpus < 1 ? 1 : cpus > 8 ? 8 : (int)cpus;
  for (int i = 1; i < argc; i++) {
    if (i + 1 < argc && strcmp(argv[i], "-m") == 0) {
      model = argv[++i];
    } else if (i + 1 < argc && strcmp(argv[i], "-f") == 0) {
      clip = argv[++i];
    } else if (i + 1 < argc && strcmp(argv[i], "-t") == 0) {
      threads = atoi(argv[++i]);
    } else if (strcmp(argv[i], "--resident") == 0) {
      resident = 1;
    } else {
      return usage();
    }
  }
  if (model == NULL || threads < 1 || (clip == NULL) == (resident == 0)) return usage();

  if (resident) {
    dn_die_with_parent();
    dn_take_stdout();
    double t0 = dn_now_ms();
    struct parakeet_context *ctx = load(model, threads);
    if (ctx == NULL) {
      char why[512];
      snprintf(why, sizeof why, "the model %s did not load", model);
      dn_load_failed(why);
    }
    int code = dn_serve(ctx, transcribe, dn_now_ms() - t0);
    parakeet_free(ctx);
    return code;
  }

  size_t len = 0;
  unsigned char *bytes = dn_read_file(clip, &len);
  if (bytes == NULL) {
    fprintf(stderr, "parakeet-cli: cannot read %s\n", clip);
    return 4;
  }
  float *samples = NULL;
  uint64_t count = 0;
  const char *bad = dn_wav_samples(bytes, len, &samples, &count);
  free(bytes);
  if (bad != NULL) {
    fprintf(stderr, "parakeet-cli: %s: %s\n", clip, bad);
    return 4;
  }
  double t0 = dn_now_ms();
  struct parakeet_context *ctx = load(model, threads);
  double t1 = dn_now_ms();
  if (ctx == NULL) {
    fprintf(stderr, "parakeet-cli: the model %s did not load\n", model);
    free(samples);
    return 3;
  }
  char *text = NULL;
  if (count > 0) {
    const char *why = NULL;
    text = transcribe(ctx, samples, count, &why);
    if (text == NULL) {
      fprintf(stderr, "parakeet-cli: %s\n", why);
      free(samples);
      parakeet_free(ctx);
      return 5;
    }
  }
  double t2 = dn_now_ms();
  free(samples);
  if (text != NULL && text[0] != '\0') printf("%s\n", text);
  fflush(stdout);
  fprintf(stderr, "parakeet-cli: load_ms=%.0f decode_ms=%.0f\n", t1 - t0, t2 - t1);
  free(text);
  parakeet_free(ctx);
  return 0;
}
