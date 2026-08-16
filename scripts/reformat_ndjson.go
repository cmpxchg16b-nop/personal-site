package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"

	"personal-site/pkg/models/question"
)

// rawOpt is a single answer option in the legacy NDJSON format.
type rawOpt struct {
	IsCorrect bool   `json:"isCorrect"`
	Text      string `json:"text"`
}

// rawOptNew is a single answer option in the new NDJSON format produced by
// scripts/parse_to_nd_json.py:
//
//	{"id":"1","section":"...","answer":["4"],"question":"...","options":[{"option_id":"1","option":"..."}]}
type rawOptNew struct {
	OptionID string `json:"option_id"`
	Option   string `json:"option"`
}

// rawQuestion is one entry in the NDJSON question bank. It supports both
// the legacy format (title/isMulti/opts) and the new format
// (id/section/answer/question/options) for backward compatibility.
// Images is optional in the legacy data; it stays nil when omitted.
type rawQuestion struct {
	// New format (parse_to_nd_json.py)
	ID       string     `json:"id"`
	Section  string     `json:"section"`
	Answer   []string   `json:"answer"`
	Question string     `json:"question"`
	Options  []rawOptNew `json:"options"`

	// Legacy format
	Title   string   `json:"title"`
	IsMulti bool     `json:"isMulti"`
	Images  []string `json:"images,omitempty"`
	Opts    []rawOpt `json:"opts"`
}

// isNewFormat reports whether this rawQuestion was decoded from the new
// NDJSON schema. We treat any record with a non-empty Question, non-empty
// Options, or non-empty Answer as new format. This keeps mixed files working.
func (r rawQuestion) isNewFormat() bool {
	return r.Question != "" || len(r.Options) > 0 || len(r.Answer) > 0
}

// Parse reads NDJSON from r and returns one rawQuestion per line.
// Blank lines are skipped; malformed lines report their line number.
// It accepts both the legacy schema (title/opts) and the new schema
// (question/options/answer) in the same file.
func Parse(r io.Reader) ([]rawQuestion, error) {
	var questions []rawQuestion

	scanner := bufio.NewScanner(r)
	// Allow long lines (embedded images, long stems): up to 8 MiB per line.
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	for lineNo := 1; scanner.Scan(); lineNo++ {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}

		var q rawQuestion
		if err := json.Unmarshal(line, &q); err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		questions = append(questions, q)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return questions, nil
}

// toModel converts a raw NDJSON question into the exam XML model.
// For legacy records, id comes from the question counter, score is always 1,
// and the type is derived from IsMulti. For new-format records, id is taken
// from raw.ID when present (falling back to the counter), the stem is
// raw.Question, options are raw.Options, and correct answers are those whose
// option_id appears in raw.Answer. Type is single-choice unless Answer has
// more than one entry.
func toModel(raw rawQuestion, id int) question.Question {
	if raw.isNewFormat() {
		// New format: {"id","section","answer","question","options"}
		qID := raw.ID
		if qID == "" {
			qID = strconv.Itoa(id)
		}

		typ := question.QuestionTypeSingleChoice
		if len(raw.Answer) > 1 {
			typ = question.QuestionTypeMultipleChoice
		}

		q := question.Question{
			Id:          qID,
			Type:        typ,
			Score:       1,
			Description: question.QuestionDescription{Text: question.PlainText(raw.Question)},
		}

		// New format has no images, but keep the field for forward compat
		// (if a future producer adds images alongside the new schema).
		for _, src := range raw.Images {
			q.Exhibits = append(q.Exhibits, question.Exhibit{Image: question.Image{Src: src}})
		}

		// Build a set of correct option_ids for fast lookup.
		correctSet := make(map[string]bool, len(raw.Answer))
		for _, a := range raw.Answer {
			correctSet[a] = true
			// Also accept numeric strings without extra whitespace.
			correctSet[string(bytes.TrimSpace([]byte(a)))] = true
		}

		for _, o := range raw.Options {
			oid := o.OptionID
			if oid == "" {
				// Fallback: use positional index if option_id missing (defensive).
				oid = strconv.Itoa(len(q.Options) + 1)
			}
			opt := question.Option{Id: oid, Content: question.PlainText(o.Option)}
			q.Options = append(q.Options, opt)
			if correctSet[oid] {
				q.CorrectAnswer.Options = append(q.CorrectAnswer.Options, opt)
			}
		}

		// Fallback: if Answer contained values that didn't match option_id
		// (e.g. answer is ["4"] but options use "4" with different whitespace),
		// try matching by position as well.
		if len(q.CorrectAnswer.Options) == 0 && len(raw.Answer) > 0 {
			for _, a := range raw.Answer {
				if idx, err := strconv.Atoi(a); err == nil && idx >= 1 && idx <= len(q.Options) {
					q.CorrectAnswer.Options = append(q.CorrectAnswer.Options, q.Options[idx-1])
				}
			}
		}

		return q
	}

	// Legacy format: {"title","isMulti","opts":[{"isCorrect","text"}]}
	typ := question.QuestionTypeSingleChoice
	if raw.IsMulti {
		typ = question.QuestionTypeMultipleChoice
	}

	q := question.Question{
		Id:          strconv.Itoa(id),
		Type:        typ,
		Score:       1,
		Description: question.QuestionDescription{Text: question.PlainText(raw.Title)},
	}

	for _, src := range raw.Images {
		q.Exhibits = append(q.Exhibits, question.Exhibit{Image: question.Image{Src: src}})
	}

	for i, o := range raw.Opts {
		opt := question.Option{Id: strconv.Itoa(i + 1), Content: question.PlainText(o.Text)}
		q.Options = append(q.Options, opt)
		if o.IsCorrect {
			q.CorrectAnswer.Options = append(q.CorrectAnswer.Options, opt)
		}
	}

	return q
}

func main() {
	if len(os.Args) != 2 {
		log.Fatalf("usage: %s <questions.ndjson>", os.Args[0])
	}

	f, err := os.Open(os.Args[1])
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()

	raws, err := Parse(f)
	if err != nil {
		log.Fatal(err)
	}

	enc := xml.NewEncoder(os.Stdout)
	enc.Indent("", "  ")

	counter := 1
	for _, raw := range raws {
		// For new-format records, prefer the embedded id for the XML id,
		// but keep counter as fallback for ordering. toModel already handles
		// the fallback, so we just pass counter.
		if err := enc.Encode(toModel(raw, counter)); err != nil {
			log.Fatal(err)
		}
		counter++
	}
	if err := enc.Flush(); err != nil {
		log.Fatal(err)
	}
}
