//go:build speech

package main

/*
#cgo pkg-config: sherpa-onnx
#include <stdlib.h>
#include <string.h>
#include "sherpa-onnx/c-api/c-api.h"
*/
import "C"

import (
	"fmt"
	"os"
	"path/filepath"
	"unsafe"
)

// engineName names the runtime and its version for the hello.
func engineName() string {
	return "sherpa-onnx " + C.GoString(C.SherpaOnnxGetVersionStr())
}

// recognizer is one loaded sherpa-onnx offline recognizer: a NeMo transducer such as Parakeet
// TDT, on the CPU.
type recognizer struct {
	r *C.SherpaOnnxOfflineRecognizer
}

// newRecognizer loads the model in dir: encoder.int8.onnx, decoder.int8.onnx, joiner.int8.onnx
// and tokens.txt.
func newRecognizer(dir string, threads int) (*recognizer, error) {
	files := map[string]string{}
	for _, f := range []string{"encoder.int8.onnx", "decoder.int8.onnx", "joiner.int8.onnx", "tokens.txt"} {
		p := filepath.Join(dir, f)
		if _, err := os.Stat(p); err != nil {
			return nil, fmt.Errorf("the model folder has no %s", f)
		}
		files[f] = p
	}
	var strs []*C.char
	cs := func(s string) *C.char {
		p := C.CString(s)
		strs = append(strs, p)
		return p
	}
	defer func() {
		for _, p := range strs {
			C.free(unsafe.Pointer(p))
		}
	}()
	var cfg C.SherpaOnnxOfflineRecognizerConfig
	C.memset(unsafe.Pointer(&cfg), 0, C.sizeof_SherpaOnnxOfflineRecognizerConfig)
	cfg.feat_config.sample_rate = 16000
	cfg.feat_config.feature_dim = 80
	cfg.model_config.transducer.encoder = cs(files["encoder.int8.onnx"])
	cfg.model_config.transducer.decoder = cs(files["decoder.int8.onnx"])
	cfg.model_config.transducer.joiner = cs(files["joiner.int8.onnx"])
	cfg.model_config.tokens = cs(files["tokens.txt"])
	cfg.model_config.num_threads = C.int32_t(max(threads, 1))
	cfg.model_config.provider = cs("cpu")
	cfg.model_config.model_type = cs("nemo_transducer")
	cfg.decoding_method = cs("greedy_search")
	r := C.SherpaOnnxCreateOfflineRecognizer(&cfg)
	if r == nil {
		return nil, fmt.Errorf("sherpa-onnx did not load the model in %s", dir)
	}
	return &recognizer{r: r}, nil
}

// recognize returns the text of samples at rate (sherpa-onnx resamples to the model's rate).
func (r *recognizer) recognize(samples []float32, rate int) string {
	if len(samples) == 0 {
		return ""
	}
	s := C.SherpaOnnxCreateOfflineStream(r.r)
	defer C.SherpaOnnxDestroyOfflineStream(s)
	C.SherpaOnnxAcceptWaveformOffline(s, C.int32_t(rate), (*C.float)(unsafe.Pointer(&samples[0])), C.int32_t(len(samples)))
	C.SherpaOnnxDecodeOfflineStream(r.r, s)
	res := C.SherpaOnnxGetOfflineStreamResult(s)
	if res == nil {
		return ""
	}
	defer C.SherpaOnnxDestroyOfflineRecognizerResult(res)
	return C.GoString(res.text)
}

func (r *recognizer) close() {
	C.SherpaOnnxDestroyOfflineRecognizer(r.r)
}
