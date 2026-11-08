// Command duetty-eval measures duetty's language pick and noise rejection against
// the real API: clips go through Silero + trim and then stt.Resolver, once per
// language pair. Transcripts are printed to stdout only, never stored.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nlm/duetty/internal/app"
	"github.com/nlm/duetty/internal/audioio"
	"github.com/nlm/duetty/internal/config"
	"github.com/nlm/duetty/internal/denoise"
	"github.com/nlm/duetty/internal/langid"
	"github.com/nlm/duetty/internal/silerovad"
	"github.com/nlm/duetty/internal/stt"
	"github.com/nlm/duetty/internal/trim"
)

type clip struct {
	Lang string `json:"lang"`
	WAV  string `json:"wav"`
	Ref  string `json:"ref"`
}

type prepared struct {
	id      string
	lang    string
	ref     string
	wav16k  []byte // nil when dropped before Whisper
	dropped string // reason when wav16k == nil
}

// cache memoises Transcribe per (clip, language): the auto-detect pass is
// pair-independent, so it is paid once per clip.
type cache struct {
	mu   sync.Mutex
	next stt.Transcriber
	m    map[string]*entry
}

type entry struct {
	once sync.Once
	res  stt.Result
	err  error
}

func (c *cache) get(id string, wav []byte, lang string) (stt.Result, error) {
	k := id + "\x00" + lang
	c.mu.Lock()
	e := c.m[k]
	if e == nil {
		e = &entry{}
		c.m[k] = e
	}
	c.mu.Unlock()
	e.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		e.res, e.err = c.next.Transcribe(ctx, wav, lang)
	})
	return e.res, e.err
}

type clipT struct {
	c   *cache
	id  string
	wav []byte
}

func (t clipT) Transcribe(_ context.Context, _ []byte, lang string) (stt.Result, error) {
	return t.c.get(t.id, t.wav, lang)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "duetty-eval:", err)
		os.Exit(1)
	}
}

func run() error {
	fs := flag.NewFlagSet("duetty-eval", flag.ExitOnError)
	clips := fs.String("clips", "eval/clips/meta.json", "meta.json of sentence clips")
	pairsF := fs.String("pairs", "fr-en,es-en,ja-en,es-fr,ja-fr,es-ja", "comma-separated language pairs")
	words := fs.String("words", "", "meta.json of short real words (must be kept, not dropped as noise)")
	noise := fs.String("noise", "", "directory of noise WAVs (any 48 kHz mono s16le or ffmpeg-convertible via eval script)")
	detail := fs.Bool("detail", false, "print per-clip logprob/compression of every Whisper pass (words and noise)")
	floor := fs.Float64("floor", 0, "noise logprob floor (default: config)")
	maxc := fs.Float64("maxcomp", 0, "noise max compression (default: config)")
	cfgPath := fs.String("config", "", "config file")
	apiURL := fs.String("api-url", "", "OpenAI-compatible API URL (default: config)")
	dn := fs.Bool("denoise", false, "run every clip through the local denoiser first (as -denoise in duetty)")
	fs.Parse(os.Args[1:])

	var cargs []string
	if *cfgPath != "" {
		cargs = []string{"-config", *cfgPath}
	}
	if *apiURL != "" {
		cargs = append(cargs, "-api-url", *apiURL)
	}
	cfg, err := config.Load(cargs, os.Getenv)
	if err != nil {
		return err
	}
	if cfg.APIKey == "" && cfg.NeedsKey() {
		return errors.New("no API key: set DUETTY_API_KEY (or OPENAI_API_KEY)")
	}
	if *floor == 0 {
		*floor = cfg.NoiseMinLogprob
	}
	if *maxc == 0 {
		*maxc = cfg.NoiseMaxCompression
	}
	cfg.Denoise = cfg.Denoise || *dn
	cfg, warns, err := app.ResolveAudioPaths(cfg)
	_ = warns // Smart Turn is irrelevant here; a missing denoise model is checked below
	if err != nil {
		return err
	}
	if cfg.Denoise {
		if cfg.DenoiseModel == "" {
			return errors.New("-denoise: no denoise model next to duetty-eval (run scripts/setup-denoise.sh) or set denoise_model")
		}
		d, err := denoise.New(cfg.ONNXRuntimeLib, cfg.DenoiseModel)
		if err != nil {
			return fmt.Errorf("denoise: %w", err)
		}
		d.Close() // fail fast; each clip gets a fresh denoiser
		fmt.Printf("denoise: on (%s)\n", cfg.DenoiseModel)
	}
	vad, err := silerovad.NewVAD(cfg.ONNXRuntimeLib, cfg.VADModel, "", cfg.VADThreshold, cfg.VADEndSilenceMS, cfg.VADOnsetMS, audioio.SampleRate)
	if err != nil {
		return fmt.Errorf("silero: %w", err)
	}
	defer vad.Close()

	prep := func(id string, pcm []byte) (prepared, error) {
		if cfg.Denoise {
			var err error
			if pcm, err = denoiseClip(cfg.ONNXRuntimeLib, cfg.DenoiseModel, pcm); err != nil {
				return prepared{}, fmt.Errorf("%s: denoise: %w", id, err)
			}
		}
		probs, err := vad.Probs(pcm)
		if err != nil {
			return prepared{}, err
		}
		voiced := 0
		for _, p := range probs {
			if float64(p) >= cfg.VADThreshold {
				voiced++
			}
		}
		out := trim.Trim(pcm, probs, cfg.VADThreshold, time.Duration(cfg.VADTrimMarginMS)*time.Millisecond)
		switch {
		case out == nil:
			return prepared{id: id, dropped: "no voice after trim"}, nil
		case float64(voiced)*32 < cfg.VADMinSpeechMS:
			return prepared{id: id, dropped: fmt.Sprintf("voiced %dms < min speech", voiced*32)}, nil
		}
		w, err := stt.PCM48kToWAV16k(out)
		return prepared{id: id, wav16k: w}, err
	}
	load := func(dir, name string) ([]byte, error) {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		return wavPCM48k(b)
	}
	loadMeta := func(path string) ([]prepared, error) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var cs []clip
		if err := json.Unmarshal(raw, &cs); err != nil {
			return nil, err
		}
		var out []prepared
		for _, c := range cs {
			pcm, err := load(filepath.Dir(path), c.WAV)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", c.WAV, err)
			}
			p, err := prep(c.WAV, pcm)
			if err != nil {
				return nil, err
			}
			p.lang, p.ref = c.Lang, c.Ref
			out = append(out, p)
		}
		return out, nil
	}

	sentences, err := loadMeta(*clips)
	if err != nil {
		return err
	}
	var shortWords []prepared
	if *words != "" {
		if shortWords, err = loadMeta(*words); err != nil {
			return err
		}
	}
	var noises []prepared
	if *noise != "" {
		files, _ := filepath.Glob(filepath.Join(*noise, "*.wav"))
		sort.Strings(files)
		for _, f := range files {
			pcm, err := load(filepath.Dir(f), filepath.Base(f))
			if err != nil {
				return err
			}
			p, err := prep(filepath.Base(f), pcm)
			if err != nil {
				return err
			}
			noises = append(noises, p)
		}
	}

	ca := &cache{next: stt.NewClient(cfg.STTURL(), cfg.APIKey, cfg.STTModel), m: map[string]*entry{}}
	resolve := func(p prepared, pair [2]string) (stt.Resolved, error) {
		r := stt.Resolver{T: clipT{ca, p.id, p.wav16k}, ID: langid.New(pair), Pair: pair, LogprobFloor: *floor, MaxCompression: *maxc}
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		return r.Resolve(ctx, p.wav16k)
	}

	fmt.Printf("thresholds: logprob floor %g, max compression %g\n", *floor, *maxc)
	for _, ps := range strings.Split(*pairsF, ",") {
		pr := strings.Split(strings.TrimSpace(ps), "-")
		if len(pr) != 2 {
			return fmt.Errorf("bad pair %q", ps)
		}
		pair := [2]string{pr[0], pr[1]}
		correct, total := 0, 0
		var misses []string
		for _, c := range sentences {
			if c.lang != pair[0] && c.lang != pair[1] {
				continue
			}
			total++
			if c.wav16k == nil {
				misses = append(misses, fmt.Sprintf("  %s: dropped before Whisper (%s)  ref=%q", c.id, c.dropped, c.ref))
				continue
			}
			got, err := resolve(c, pair)
			if err == nil && got.Lang == c.lang {
				correct++
				continue
			}
			what := "wrong language " + got.Lang
			if err != nil {
				what = "error: " + err.Error()
			}
			misses = append(misses, fmt.Sprintf("  %s: %s  ref=%q got=%q", c.id, what, c.ref, got.Text))
		}
		fmt.Printf("\npair %s: %d/%d correct\n", ps, correct, total)
		for _, m := range misses {
			fmt.Println(m)
		}
	}

	// Short real words: must be kept, in their own language.
	if len(shortWords) > 0 {
		fmt.Println("\nshort real words (must be kept)")
		for _, ps := range strings.Split(*pairsF, ",") {
			pr := strings.Split(strings.TrimSpace(ps), "-")
			pair := [2]string{pr[0], pr[1]}
			kept, total, dropped := 0, 0, 0
			var lost []string
			for _, w := range shortWords {
				if w.lang != pair[0] && w.lang != pair[1] {
					continue
				}
				total++
				if w.wav16k == nil {
					dropped++
					lost = append(lost, fmt.Sprintf("  %s: dropped before Whisper (%s) ref=%q", w.id, w.dropped, w.ref))
					continue
				}
				_, err := resolve(w, pair)
				if err == nil {
					kept++
				} else if errors.Is(err, stt.ErrNoise) {
					lost = append(lost, fmt.Sprintf("  %s: dropped as noise ref=%q", w.id, w.ref))
				} else {
					lost = append(lost, fmt.Sprintf("  %s: error %v", w.id, err))
				}
			}
			fmt.Printf("pair %s: kept %d/%d (dropped before Whisper %d)\n", ps, kept, total, dropped)
			for _, l := range lost {
				fmt.Println(l)
			}
		}
	}

	// Noise: dropped before Whisper vs ErrNoise vs produced a row.
	if len(noises) > 0 {
		fmt.Println("\nnoise clips")
		for _, ps := range strings.Split(*pairsF, ",") {
			pr := strings.Split(strings.TrimSpace(ps), "-")
			pair := [2]string{pr[0], pr[1]}
			pre, errNoise, rows := 0, 0, 0
			var leaked []string
			for _, n := range noises {
				if n.wav16k == nil {
					pre++
					continue
				}
				got, err := resolve(n, pair)
				switch {
				case errors.Is(err, stt.ErrNoise):
					errNoise++
				case err != nil:
					leaked = append(leaked, fmt.Sprintf("  %s: error %v", n.id, err))
				default:
					rows++
					leaked = append(leaked, fmt.Sprintf("  %s: ROW lang=%s text=%q", n.id, got.Lang, got.Text))
				}
			}
			fmt.Printf("pair %s: %d clips: dropped before Whisper %d, ErrNoise %d, produced a row %d\n", ps, len(noises), pre, errNoise, rows)
			for _, l := range leaked {
				fmt.Println(l)
			}
		}
	}

	if *detail {
		fmt.Println("\ndetail: per-pass Whisper stats (lang=\"\" is auto-detect)")
		report := func(kind string, set []prepared) {
			for _, p := range set {
				if p.wav16k == nil {
					fmt.Printf("%-5s %-28s dropped before Whisper (%s)\n", kind, p.id, p.dropped)
					continue
				}
				ca.mu.Lock()
				var keys []string
				for k := range ca.m {
					if strings.HasPrefix(k, p.id+"\x00") {
						keys = append(keys, k)
					}
				}
				ca.mu.Unlock()
				sort.Strings(keys)
				for _, k := range keys {
					e := ca.m[k]
					l := strings.TrimPrefix(k, p.id+"\x00")
					fmt.Printf("%-5s %-28s pass=%-3q lp=%.3f cr=%.2f det=%s text=%q\n", kind, p.id, l, e.res.AvgLogprob, e.res.CompressionRatio, e.res.Language, e.res.Text)
				}
			}
		}
		report("word", shortWords)
		report("noise", noises)
	}
	return nil
}

// denoiseClip runs pcm through a fresh denoiser in mic-sized chunks, as the
// live app does, and removes the denoiser's constant delay.
func denoiseClip(ortLib, model string, pcm []byte) ([]byte, error) {
	d, err := denoise.New(ortLib, model)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	lag := 2 * denoise.LatencySamples
	in := append(append([]byte{}, pcm...), make([]byte, lag)...)
	out := make([]byte, 0, len(in))
	const chunk = 2048
	for i := 0; i < len(in); i += chunk {
		out = append(out, d.Process(in[i:min(i+chunk, len(in))])...)
	}
	return out[lag:], nil
}
