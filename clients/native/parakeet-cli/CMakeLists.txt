# Builds parakeet-cli and parakeet-quantize: CrispASR's Parakeet code (five
# source files) and the ggml CPU backend, linked statically, with no other
# CrispASR code.
#
#   cmake -S clients/native/parakeet-cli -B BUILD -DCRISPASR_SRC=SRC -DGGML_SRC=GGML
#   cmake --build BUILD --target parakeet-cli parakeet-quantize
#
# clients/go/scripts/native.sh --parakeet runs this. CRISPASR_SRC is a
# sparse checkout of the pinned CrispASR commit that holds only the files
# crispasr-files.sha256 lists; GGML_SRC is the pinned ggml commit with only
# its CPU backend. So no other CrispASR code (the ports of the other
# models, the model download code) is on disk to compile, which keeps the
# build small, offline and auditable.
# parakeet-quantize is CrispASR's crispasr-quantize, which turns the
# fetched F16 model into the Q4_K one on the user's box.
cmake_minimum_required(VERSION 3.22.1)
project(parakeet-cli C CXX)

if(NOT CRISPASR_SRC OR NOT GGML_SRC)
  message(FATAL_ERROR "Set CRISPASR_SRC to the CrispASR checkout and GGML_SRC to the ggml checkout.")
endif()

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_POSITION_INDEPENDENT_CODE ON)
set(BUILD_SHARED_LIBS OFF CACHE BOOL "" FORCE)
set(GGML_NATIVE OFF CACHE BOOL "" FORCE)
set(GGML_OPENMP OFF CACHE BOOL "" FORCE)
set(GGML_BLAS OFF CACHE BOOL "" FORCE)
set(GGML_CCACHE OFF CACHE BOOL "" FORCE)
set(GGML_BUILD_TESTS OFF CACHE BOOL "" FORCE)
set(GGML_BUILD_EXAMPLES OFF CACHE BOOL "" FORCE)
add_subdirectory(${GGML_SRC} ${CMAKE_BINARY_DIR}/ggml)

set(s ${CRISPASR_SRC}/src)
add_library(parakeet STATIC
  ${s}/parakeet.cpp
  ${s}/parakeet_orchestrate.cpp
  ${s}/core/mel.cpp
  ${s}/core/gguf_loader.cpp
  ${s}/crispasr_imatrix.cpp)
target_include_directories(parakeet PUBLIC ${s} PRIVATE ${GGML_SRC}/src)
target_link_libraries(parakeet PUBLIC ggml ggml-base ggml-cpu)

add_executable(parakeet-cli ${CMAKE_CURRENT_LIST_DIR}/parakeet-cli.c)
target_include_directories(parakeet-cli PRIVATE ${s} ${CMAKE_CURRENT_LIST_DIR}/../common)
target_link_libraries(parakeet-cli PRIVATE parakeet)
set_target_properties(parakeet-cli PROPERTIES LINKER_LANGUAGE CXX)

set(e ${CRISPASR_SRC}/examples)
add_executable(parakeet-quantize ${e}/crispasr-quantize/main.cpp ${e}/common-ggml.cpp ${e}/common.cpp)
target_include_directories(parakeet-quantize PRIVATE ${e} ${s})
target_link_libraries(parakeet-quantize PRIVATE ggml ggml-base ggml-cpu)
