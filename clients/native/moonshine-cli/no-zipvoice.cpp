// A ZipVoice text-to-speech engine with nothing in it, linked in place of
// the upstream zipvoice-*.cpp files.
//
// ZipVoice is a text-to-speech model, and zipvoice-voices-data.cpp embeds
// its reference voice clips. The voice bridge only transcribes, so this
// build leaves all of it out, which keeps the library small and leaves
// less code to audit. Moonshine's C API still names the engine, so
// the symbols exist: there are no built-in voices, and building the engine
// throws, which the C API turns into an error code.
#include <stdexcept>

#include "zipvoice-tts.h"
#include "zipvoice-voices.h"

namespace moonshine_tts {

namespace {
[[noreturn]] void not_built() {
  throw std::runtime_error("ZipVoice is not built into this Moonshine library");
}
}  // namespace

struct ZipVoiceTTS::Impl {};

ZipVoiceTTS::ZipVoiceTTS(const ZipVoiceTTSOptions &) { not_built(); }
ZipVoiceTTS::ZipVoiceTTS(ZipVoiceTTS &&) noexcept = default;
ZipVoiceTTS &ZipVoiceTTS::operator=(ZipVoiceTTS &&) noexcept = default;
ZipVoiceTTS::~ZipVoiceTTS() = default;
void ZipVoiceTTS::set_speed(double) { not_built(); }
double ZipVoiceTTS::speed() const { not_built(); }
bool ZipVoiceTTS::normalize_audio() const { not_built(); }
void ZipVoiceTTS::set_normalize_audio(bool) { not_built(); }
float ZipVoiceTTS::output_volume() const { not_built(); }
void ZipVoiceTTS::set_output_volume(float) { not_built(); }
std::vector<float> ZipVoiceTTS::synthesize(std::string_view) { not_built(); }
std::vector<float> ZipVoiceTTS::synthesize_from_ipa(std::string_view) { not_built(); }

std::vector<float> zipvoice_compress_long_pauses(const std::vector<float> &wav, int, float, float,
                                                 float) {
  return wav;
}

const ZipVoiceBuiltinVoice *zipvoice_builtin_voices(size_t *count) {
  if (count != nullptr) *count = 0;
  return nullptr;
}

const ZipVoiceBuiltinVoice *zipvoice_find_builtin_voice(std::string_view) { return nullptr; }

std::vector<float> zipvoice_builtin_voice_pcm_to_float(const ZipVoiceBuiltinVoice &) { return {}; }

}  // namespace moonshine_tts
