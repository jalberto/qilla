// Package decide runs the deciders' TF-IDF + logistic-regression models in
// Go, so a gather classifies without spawning Python.
//
// Training stays in scikit-learn (~/Projects/mio/qilla-deciders, weekly);
// `scripts/export_model.py` flattens the fitted pipeline into
// <state>/deciders/models/<task>.model.json.gz and this package reproduces
// sklearn's arithmetic exactly — the golden test asserts agreement to 1e-6.
//
// The recipe, mirroring sklearn step for step:
//
//	preprocess : lowercase, then strip_accents_unicode (ASCII passes through
//	             untouched; otherwise NFKD and drop combining marks)
//	word       : token_pattern over the normalised text, 1-2 grams joined " "
//	char_wb    : each whitespace-split word padded with one space on each side,
//	             3-5 grams slid over the padded word
//	weights    : sublinear tf (1+ln c) * idf, L2-normalised PER VECTORIZER
//	             (each TfidfVectorizer normalises before the union concatenates)
//	classifier : x.coef + intercept; two classes -> sigmoid of the single row,
//	             which is P(classes[1]); more -> softmax over the rows.
package decide

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Vectorizer is one exported TfidfVectorizer.
type Vectorizer struct {
	NGram        []int          `json:"ngram"`
	TokenPattern string         `json:"token_pattern,omitempty"`
	Vocab        map[string]int `json:"vocab"`
	IDF          []float64      `json:"idf"`
}

// Model is the exported decider: see scripts/export_model.py for the format.
type Model struct {
	Format    int      `json:"format"`
	Task      string   `json:"task"`
	Classes   []string `json:"classes"`
	Threshold float64  `json:"threshold"`
	Text      struct {
		Lowercase    bool   `json:"lowercase"`
		StripAccents string `json:"strip_accents"`
	} `json:"text"`
	Word        Vectorizer  `json:"word"`
	Char        Vectorizer  `json:"char"`
	Coef        [][]float64 `json:"coef"`
	Intercept   []float64   `json:"intercept"`
	SublinearTF bool        `json:"sublinear_tf"`
	Norm        string      `json:"norm"`

	tokenRe *regexp.Regexp
}

// Prediction is one row's answer. Unknown means the model declined: the
// caller must escalate it (the escape rule), never treat Label as decided.
type Prediction struct {
	Label   string             `json:"label"`
	P       float64            `json:"p"`
	Conf    float64            `json:"conf"`
	Unknown bool               `json:"unknown"`
	Dist    map[string]float64 `json:"dist"`
}

// ModelPath is where a task's exported model lives under the deciders state
// directory (<state_dir>/deciders).
func ModelPath(dir, task string) string {
	return filepath.Join(dir, "models", task+".model.json.gz")
}

// LoadModel reads an exported model; the file may be gzipped (.gz) or plain.
func LoadModel(path string) (*Model, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var r io.Reader = bufio.NewReaderSize(f, 1<<20)
	if strings.HasSuffix(path, ".gz") {
		zr, err := gzip.NewReader(r)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		defer zr.Close()
		// A model is tens of MB of JSON: decompressing on its own goroutine
		// overlaps gunzip with the parse, which halves the load.
		pr, pw := io.Pipe()
		go func() {
			_, err := io.Copy(pw, zr)
			pw.CloseWithError(err)
		}()
		defer pr.Close()
		r = pr
	}
	var m Model
	dec := json.NewDecoder(bufio.NewReaderSize(r, 1<<20))
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if m.Format != 1 {
		return nil, fmt.Errorf("%s: unsupported format %d", path, m.Format)
	}
	if len(m.Classes) < 2 || len(m.Coef) == 0 {
		return nil, fmt.Errorf("%s: model has no classes or coefficients", path)
	}
	if want := len(m.Word.IDF) + len(m.Char.IDF); len(m.Coef[0]) != want {
		return nil, fmt.Errorf("%s: coef width %d, expected %d", path, len(m.Coef[0]), want)
	}
	m.tokenRe, err = compileTokenPattern(m.Word.TokenPattern)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &m, nil
}

// goTokenPattern is Go's equivalent of Python's default `(?u)\b\w\w+\b`:
// every maximal run of two or more unicode word characters. Go's \b and \w
// are ASCII-only, so the pattern cannot be used verbatim; a run of 2+ word
// characters is exactly what the Python pattern finds.
const goTokenPattern = `[\p{L}\p{N}_]{2,}`

const sklearnTokenPattern = `(?u)\b\w\w+\b`

func compileTokenPattern(p string) (*regexp.Regexp, error) {
	if p == "" || p == sklearnTokenPattern {
		return regexp.MustCompile(goTokenPattern), nil
	}
	// Any other pattern is taken as-is; (?u) is implicit in Go and not a
	// legal flag, so it is dropped.
	return regexp.Compile(strings.TrimPrefix(p, "(?u)"))
}

// Preprocess is sklearn's preprocessor: lowercase, then strip accents.
func Preprocess(s string) string {
	return StripAccents(strings.ToLower(s))
}

// StripAccents mirrors sklearn.feature_extraction.text.strip_accents_unicode:
// pure-ASCII input is returned unchanged, anything else is NFKD-normalised
// with the combining characters dropped.
func StripAccents(s string) string {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range norm.NFKD.String(s) {
		if norm.NFC.PropertiesString(string(r)).CCC() != 0 {
			continue // unicodedata.combining(c) != 0
		}
		b.WriteRune(r)
	}
	return b.String()
}

// WordNGrams is sklearn's word analyzer over already-normalised text.
func (m *Model) WordNGrams(norm string) []string {
	tokens := m.tokenRe.FindAllString(norm, -1)
	return joinNGrams(tokens, m.Word.NGram)
}

func joinNGrams(tokens []string, ngram []int) []string {
	minN, maxN := ngramRange(ngram, 1, 1)
	out := make([]string, 0, len(tokens)*(maxN-minN+1))
	if minN == 1 {
		out = append(out, tokens...)
		minN = 2
	}
	for n := minN; n <= maxN && n <= len(tokens); n++ {
		for i := 0; i+n <= len(tokens); i++ {
			out = append(out, strings.Join(tokens[i:i+n], " "))
		}
	}
	return out
}

func ngramRange(ngram []int, dmin, dmax int) (int, int) {
	if len(ngram) == 2 {
		return ngram[0], ngram[1]
	}
	return dmin, dmax
}

// CharWBNGrams mirrors sklearn's _char_wb_ngrams: each whitespace-separated
// word is padded with one space on both sides and the windows slide over the
// padded word; a word shorter than n contributes its padded form exactly once.
func (m *Model) CharWBNGrams(text string) []string {
	minN, maxN := ngramRange(m.Char.NGram, 3, 5)
	var out []string
	for _, word := range strings.FieldsFunc(text, unicode.IsSpace) {
		w := []rune(" " + word + " ")
		wLen := len(w)
		for n := minN; n <= maxN; n++ {
			offset := 0
			out = append(out, string(w[offset:min(offset+n, wLen)]))
			for offset+n < wLen {
				offset++
				out = append(out, string(w[offset:min(offset+n, wLen)]))
			}
			if offset == 0 {
				break // the word is shorter than n: longer n add nothing new
			}
		}
	}
	return out
}

// feature is one non-zero of the sparse row.
type feature struct {
	i int
	v float64
}

// features builds the sparse, per-vectorizer L2-normalised row for one text,
// sorted by index. Indices are global: the char block starts at
// len(word.IDF). Sorted order makes the dot product below deterministic —
// float addition is not associative, so map order would make the output
// differ run to run — and matches the order scipy sums in.
func (m *Model) features(text string) []feature {
	norm := text
	if m.Text.Lowercase {
		norm = strings.ToLower(norm)
	}
	if m.Text.StripAccents == "unicode" {
		norm = StripAccents(norm)
	}
	out := make([]feature, 0, 512)
	out = block(out, m.WordNGrams(norm), m.Word, 0, m.SublinearTF)
	out = block(out, m.CharWBNGrams(norm), m.Char, len(m.Word.IDF), m.SublinearTF)
	return out // already sorted: the word block's indices all precede the char block's
}

// block counts the terms of one vectorizer, weights them and L2-normalises
// that vectorizer's own vector before appending it to out. The non-zeros are
// sorted by index first so both the norm and the later dot product sum in
// scipy's order: float addition is not associative and map order is random,
// which would make the output differ from run to run.
func block(out []feature, terms []string, v Vectorizer, offset int, sublinear bool) []feature {
	counts := make(map[int]float64, len(terms))
	for _, t := range terms {
		if i, ok := v.Vocab[t]; ok {
			counts[i]++
		}
	}
	if len(counts) == 0 {
		return out
	}
	idx := make([]int, 0, len(counts))
	for i := range counts {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	vals := make([]float64, len(idx))
	sum := 0.0
	for k, i := range idx {
		w := counts[i]
		if sublinear {
			w = 1 + math.Log(w)
		}
		w *= v.IDF[i]
		vals[k] = w
		sum += w * w
	}
	if sum == 0 {
		return out
	}
	n := math.Sqrt(sum)
	for k, i := range idx {
		out = append(out, feature{offset + i, vals[k] / n})
	}
	return out
}

// Predict classifies each text. The order of the result matches texts.
func (m *Model) Predict(texts []string) []Prediction {
	preds := make([]Prediction, len(texts))
	for i, t := range texts {
		preds[i] = m.predictOne(t)
	}
	return preds
}

func (m *Model) predictOne(text string) Prediction {
	x := m.features(text)
	scores := make([]float64, len(m.Coef))
	for c, row := range m.Coef {
		s := m.Intercept[c]
		for _, f := range x {
			s += row[f.i] * f.v
		}
		scores[c] = s
	}
	var proba []float64
	if len(m.Coef) == 1 {
		// Binary: sklearn keeps one row, scoring classes[1].
		p1 := 1 / (1 + math.Exp(-scores[0]))
		proba = []float64{1 - p1, p1}
	} else {
		proba = softmax(scores)
	}
	p := Prediction{Dist: make(map[string]float64, len(proba))}
	best := 0
	for i, v := range proba {
		if i < len(m.Classes) {
			p.Dist[m.Classes[i]] = v
		}
		if v > proba[best] {
			best = i
		}
	}
	p.Label = m.Classes[best]
	p.Conf = proba[best]
	p.P = proba[best]
	if len(m.Classes) == 2 {
		p.P = proba[1] // the positive class, so a caller can threshold it
	}
	p.Unknown = p.Conf < m.Threshold
	return p
}

func softmax(s []float64) []float64 {
	maxS := s[0]
	for _, v := range s {
		if v > maxS {
			maxS = v
		}
	}
	out := make([]float64, len(s))
	sum := 0.0
	for i, v := range s {
		e := math.Exp(v - maxS)
		out[i] = e
		sum += e
	}
	for i := range out {
		out[i] /= sum
	}
	return out
}
