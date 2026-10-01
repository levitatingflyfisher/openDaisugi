package voice

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Which is shutil.which(name): the first PATH entry that holds name as an
// executable file that is not a directory, joined as Python joins it, or
// "".
func Which(name string) string {
	path, ok := os.LookupEnv("PATH")
	if !ok {
		path = "/bin:/usr/bin"
	}
	if strings.Contains(name, "/") {
		if isExec(name) {
			return name
		}
		return ""
	}
	seen := map[string]bool{}
	for _, dir := range strings.Split(path, ":") {
		if seen[dir] {
			continue
		}
		seen[dir] = true
		p := name
		switch {
		case dir == "":
		case strings.HasSuffix(dir, "/"):
			p = dir + name
		default:
			p = dir + "/" + name
		}
		if isExec(p) {
			return p
		}
	}
	return ""
}

func isExec(p string) bool {
	st, err := os.Stat(p)
	if err != nil || st.IsDir() {
		return false
	}
	return syscall.Access(p, 1) == nil
}

// ffmpegArgs is the command line audio._ffmpeg_to_wav_16k_mono runs.
var ffmpegArgs = []string{"-hide_banner", "-loglevel", "error", "-i", "pipe:0",
	"-ar", "16000", "-ac", "1", "-f", "wav", "pipe:1"}

// ffmpegTimeout is the oracle's subprocess timeout for ffmpeg.
const ffmpegTimeout = 30 * time.Second

func ffmpegToWav(raw []byte) ([]byte, *AudioError) {
	ctx, cancel := context.WithTimeout(context.Background(), ffmpegTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg", ffmpegArgs...)
	cmd.Path = Which("ffmpeg")
	cmd.Stdin = bytes.NewReader(raw)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, audioErr("timeout", "ffmpeg timed out")
	}
	if err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			return nil, audioErr("oserror", err.Error())
		}
		return nil, audioErr("unsupported", "ffmpeg could not decode this clip")
	}
	rate, samples, aerr := readMonoSamples(out.Bytes())
	if aerr != nil {
		return nil, aerr
	}
	return writeWavMono(samples, rate)
}

// ToWav16kMono is audio.to_wav_16k_mono. A WAV at 16 kHz is repacked as
// mono; a WAV at another rate goes through ffmpeg when it is on PATH, and
// through the linear resample when it is not. Any other clip needs
// ffmpeg: the oracle's last resort, the av package, is a Python library
// this binary does not carry (ruling VO-4).
func ToWav16kMono(raw []byte) ([]byte, *AudioError) {
	if isWav(raw) {
		rate, samples, err := readMonoSamples(raw)
		if err != nil {
			return nil, err
		}
		if rate == 16000 {
			return writeWavMono(samples, 16000)
		}
		if Which("ffmpeg") != "" {
			return ffmpegToWav(raw)
		}
		out, err := resampleLinear(samples, rate, 16000)
		if err != nil {
			return nil, err
		}
		return writeWavMono(out, 16000)
	}
	if Which("ffmpeg") != "" {
		return ffmpegToWav(raw)
	}
	return nil, audioErr("unsupported", "cannot decode this audio format. The ffmpeg binary is not on PATH. Install ffmpeg.")
}
