// A SpeakerDiarizer with no diarization in it, linked in place of the
// upstream speaker-diarizer.cpp.
//
// The upstream one runs cpp-annote, which compiles kaldi-native-fbank
// (Xiaomi Corporation) into the library. The voice bridge never asks for
// speaker identification, so this build leaves that code out: the
// transcriber only builds a SpeakerDiarizer when the identify_speakers
// option is on, and moonshine-cli passes no options. A caller that turns it
// on gets an exception here, which the C API turns into an error code.
#include "speaker-diarizer.h"

#include <stdexcept>

struct SpeakerDiarizer::Impl {};

namespace {
[[noreturn]] void not_built() {
  throw std::runtime_error("speaker identification is not built into this Moonshine library");
}
}  // namespace

SpeakerDiarizer::SpeakerDiarizer(const SpeakerDiarizerOptions &) { not_built(); }
SpeakerDiarizer::~SpeakerDiarizer() = default;
int32_t SpeakerDiarizer::create_stream() { not_built(); }
void SpeakerDiarizer::free_stream(int32_t) { not_built(); }
void SpeakerDiarizer::start_stream(int32_t) { not_built(); }
void SpeakerDiarizer::add_audio_to_stream(int32_t, const float *, uint64_t, int32_t) { not_built(); }
std::vector<SpeakerTurn> SpeakerDiarizer::get_turns(int32_t) { not_built(); }
std::vector<SpeakerTurn> SpeakerDiarizer::finish_stream(int32_t) { not_built(); }
std::vector<SpeakerTurn> SpeakerDiarizer::diarize(const float *, uint64_t, int32_t) { not_built(); }
