package decide

import (
	"fmt"
	"os"
	"strings"
	"sync"
)

// textLimit is deciders/dataset.py TEXT_LIMIT — in characters, not bytes.
const textLimit = 2000

// Record is one row to classify, the shape `qilla decide predict` reads from
// stdin and a gather passes to the decide() builtin.
type Record struct {
	ID       string `json:"id"`
	Subject  string `json:"subject"`
	From     string `json:"from"`
	FromName string `json:"from_name"`
	Body     string `json:"body"`
}

// Text is deciders/dataset.py::predict_text / build_text, verbatim: subject,
// then the sender (display name and address), then the body — one per line,
// cut at 2000 characters. Change it here only together with the Python side,
// or the models see text they were not trained on.
func (r Record) Text() string {
	who := strings.TrimSpace(strings.Join(nonEmpty(strings.TrimSpace(r.FromName), strings.TrimSpace(r.From)), " "))
	parts := []string{strings.TrimSpace(r.Subject), who, strings.TrimSpace(r.Body)}
	s := strings.Join(parts, "\n")
	if runes := []rune(s); len(runes) > textLimit {
		s = string(runes[:textLimit])
	}
	return s
}

func nonEmpty(ss ...string) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Result is one (row, task) answer, the JSONL line the CLI prints and the
// dict the Starlark builtin returns.
type Result struct {
	ID      string  `json:"id"`
	Task    string  `json:"task"`
	Label   string  `json:"label"`
	P       float64 `json:"p"`
	Conf    float64 `json:"conf"`
	Unknown bool    `json:"unknown"`
}

// Set is the models loaded for a list of tasks, in the order asked.
type Set struct {
	Tasks  []string
	Models map[string]*Model
}

// LoadSet loads every task that has an exported model under dir. A task
// without one is skipped with a note on w (nil = silent) — a missing model is
// not an error: the model keeps deciding that bucket.
func LoadSet(dir string, tasks []string, w *os.File) (*Set, error) {
	s := &Set{Models: make(map[string]*Model, len(tasks))}
	// Loading is the whole cost (tens of MB of JSON per task), so the tasks
	// load in parallel; the order of s.Tasks stays the order asked.
	loaded := make([]*Model, len(tasks))
	errs := make([]error, len(tasks))
	var wg sync.WaitGroup
	for i, task := range tasks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			loaded[i], errs[i] = LoadModel(ModelPath(dir, task))
		}()
	}
	wg.Wait()
	for i, task := range tasks {
		if errs[i] != nil {
			if w != nil {
				fmt.Fprintf(w, "qilla decide: skipping %s: %v\n", task, errs[i])
			}
			continue
		}
		s.Tasks = append(s.Tasks, task)
		s.Models[task] = loaded[i]
	}
	return s, nil
}

// Classify returns one Result per record per loaded task, records outer.
func (s *Set) Classify(records []Record) []Result {
	texts := make([]string, len(records))
	for i, r := range records {
		texts[i] = r.Text()
	}
	byTask := make(map[string][]Prediction, len(s.Tasks))
	for _, task := range s.Tasks {
		byTask[task] = s.Models[task].Predict(texts)
	}
	out := make([]Result, 0, len(records)*len(s.Tasks))
	for i, r := range records {
		for _, task := range s.Tasks {
			p := byTask[task][i]
			out = append(out, Result{
				ID: r.ID, Task: task, Label: p.Label,
				P: p.P, Conf: p.Conf, Unknown: p.Unknown,
			})
		}
	}
	return out
}
