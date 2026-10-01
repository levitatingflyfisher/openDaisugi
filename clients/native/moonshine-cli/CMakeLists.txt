# Builds moonshine-cli: the Moonshine library from the pinned upstream
# sources, linked statically into one small program, with ONNX Runtime as
# the one shared library beside it.
#
#   cmake -S clients/native/moonshine-cli -B BUILD -DMOONSHINE_CORE=SRC/core
#   cmake --build BUILD --target moonshine-cli
#
# clients/go/scripts/native.sh --moonshine runs this. MOONSHINE_CORE is
# the core/ directory of the pinned Moonshine checkout, whose
# third-party/onnxruntime holds Microsoft's ONNX Runtime release.
#
# The library is the upstream "moonshine" target's sources, with two
# changes that leave out code the program never calls, so the build stays
# small, offline and easy to audit: speaker-diarizer.cpp is replaced by
# no-diarizer.cpp (no cpp-annote, no kaldi-native-fbank), and the ZipVoice
# files by no-zipvoice.cpp.
cmake_minimum_required(VERSION 3.22.1)
project(moonshine-cli C CXX)

if(NOT MOONSHINE_CORE)
  message(FATAL_ERROR "Set MOONSHINE_CORE to the core/ directory of the Moonshine sources.")
endif()

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)
set(CMAKE_POSITION_INDEPENDENT_CODE ON)

add_subdirectory(${MOONSHINE_CORE}/ort-utils ${CMAKE_BINARY_DIR}/ort-utils)
add_subdirectory(${MOONSHINE_CORE}/bin-tokenizer ${CMAKE_BINARY_DIR}/bin-tokenizer)
# The C API also carries text to speech, so its library comes along; this
# program never calls it. Its ZipVoice engine (a model with reference
# clips embedded in a source file) is replaced by no-zipvoice.cpp.
add_subdirectory(${MOONSHINE_CORE}/moonshine-tts ${CMAKE_BINARY_DIR}/moonshine-tts)
get_target_property(tts_sources moonshine_tts_ort SOURCES)
list(FILTER tts_sources EXCLUDE REGEX "zipvoice-")
list(APPEND tts_sources ${CMAKE_CURRENT_LIST_DIR}/no-zipvoice.cpp)
set_target_properties(moonshine_tts_ort PROPERTIES SOURCES "${tts_sources}")

set(core ${MOONSHINE_CORE})
add_library(moonshine-core STATIC
  ${core}/moonshine-c-api.cpp
  ${core}/moonshine-model-catalog.cpp
  ${core}/moonshine-model-file-metadata.generated.cpp
  ${core}/cosine-distance.cpp
  ${core}/moonshine-model.cpp
  ${core}/moonshine-streaming-model.cpp
  ${core}/voice-activity-detector.cpp
  ${core}/silero-vad.cpp
  ${core}/speech-clip.cpp
  ${core}/clone-clip.cpp
  ${core}/resampler.cpp
  ${core}/transcriber.cpp
  ${core}/gemma-embedding-model.cpp
  ${core}/text-embedder.cpp
  ${core}/spelling-fusion.cpp
  ${core}/spelling-fusion-data.cpp
  ${core}/spelling-model.cpp
  ${core}/context-biaser.cpp
  ${core}/context-extractor.cpp
  ${core}/word-alignment.cpp
  ${CMAKE_CURRENT_LIST_DIR}/no-diarizer.cpp
)
target_include_directories(moonshine-core PUBLIC ${core})
target_include_directories(moonshine-core PRIVATE
  ${core}/moonshine-utils
  ${core}/ort-utils
  ${core}/bin-tokenizer
  ${core}/third-party/onnxruntime/include
  ${core}/third-party/utf-8
  ${core}/third-party
  ${core}/moonshine-tts/src
)
target_compile_definitions(moonshine-core PRIVATE MOONSHINE_C_API_MOONSHINE_TTS=1)
target_link_libraries(moonshine-core PUBLIC bin-tokenizer ort-utils moonshine_tts_ort)

add_executable(moonshine-cli ${CMAKE_CURRENT_LIST_DIR}/moonshine-cli.c)
target_include_directories(moonshine-cli PRIVATE ${CMAKE_CURRENT_LIST_DIR}/../common)
target_link_libraries(moonshine-cli PRIVATE moonshine-core)
set_target_properties(moonshine-cli PROPERTIES
  LINKER_LANGUAGE CXX
  INSTALL_RPATH "$ORIGIN/../lib"
  BUILD_WITH_INSTALL_RPATH TRUE
)
