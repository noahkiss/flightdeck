package harness

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/detect"
)

// ToolQuestion is Claude Code's question tool. Its permission request is answered with text,
// not allowed or denied (harness-adapters.md, Question answers).
const ToolQuestion = "AskUserQuestion"

// Question is one question of an AskUserQuestion request.
type Question struct {
	Question    string `json:"question"`
	Header      string `json:"header"`
	MultiSelect bool   `json:"multiSelect"`
	Options     []struct {
		Label string `json:"label"`
	} `json:"options"`
}

// QuestionAnswer is the owner's answer to an AskUserQuestion request, ready for the harness.
type QuestionAnswer struct {
	// Input is the request's own tool input with "answers" added: the hook's updatedInput.
	Input json.RawMessage
	// Questions are the request's questions, in order.
	Questions []Question
	// Answers holds each question's answer, by question text.
	Answers map[string]string
}

// AnswerQuestion maps the text of an approval.answer to the questions of the open request.
// Only a Claude Code AskUserQuestion request with its input takes an answer. With one question
// the whole text is its answer: an option's label, labels joined with ", ", or free text. With
// several, each line reads "<header or question>: <answer>", one per question, every question
// answered once, as the app writes them. Anything else is refused and nothing is sent.
func AnswerQuestion(st api.AgentState, text string) (QuestionAnswer, error) {
	if st.Harness != detect.KindClaude {
		return QuestionAnswer{}, fmt.Errorf("%w: %s sends no questions the hab can answer; answer it in the terminal", ErrUnsupported, st.Harness)
	}
	r := st.Request
	if r == nil || r.Tool != ToolQuestion {
		return QuestionAnswer{}, errors.New("the open request is a permission, not a question: answer it with allow or deny")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return QuestionAnswer{}, errors.New("the answer is empty")
	}
	if r.InputTruncated || len(r.Input) == 0 {
		return QuestionAnswer{}, fmt.Errorf("%w: the hab does not hold the question's input; answer it in the terminal", ErrUnsupported)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(r.Input, &fields); err != nil {
		return QuestionAnswer{}, fmt.Errorf("%w: the question's input does not read as an object (%v); answer it in the terminal", ErrUnsupported, err)
	}
	if _, ok := fields["answers"]; ok {
		return QuestionAnswer{}, fmt.Errorf("%w: the question already carries answers; answer it in the terminal", ErrUnsupported)
	}
	var qs []Question
	if err := json.Unmarshal(fields["questions"], &qs); err != nil || len(qs) == 0 {
		return QuestionAnswer{}, fmt.Errorf("%w: the request holds no questions the hab can read; answer it in the terminal", ErrUnsupported)
	}
	answers := map[string]string{}
	if len(qs) == 1 {
		answers[qs[0].Question] = text
	} else {
		var err error
		if answers, err = lineAnswers(qs, text); err != nil {
			return QuestionAnswer{}, err
		}
	}
	b, err := json.Marshal(answers)
	if err != nil {
		return QuestionAnswer{}, err
	}
	fields["answers"] = b
	in, err := json.Marshal(fields)
	if err != nil {
		return QuestionAnswer{}, err
	}
	return QuestionAnswer{Input: in, Questions: qs, Answers: answers}, nil
}

// lineAnswers reads one "<header or question>: <answer>" line per question. A line names the
// question whose header or text, followed by a colon, starts it; the longest such name wins,
// and a name two questions share fits neither.
func lineAnswers(qs []Question, text string) (map[string]string, error) {
	want := fmt.Errorf("the request asks %d questions: answer each on its own line as \"<header>: <answer>\"", len(qs))
	answers := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		best, n, tie := -1, 0, false
		for i, q := range qs {
			for _, name := range []string{q.Header, q.Question} {
				name = strings.TrimSpace(name)
				if name == "" || !strings.HasPrefix(line, name+":") {
					continue
				}
				switch {
				case len(name) > n:
					best, n, tie = i, len(name), false
				case len(name) == n && i != best:
					tie = true
				}
			}
		}
		if best < 0 || tie {
			return nil, want
		}
		q := qs[best].Question
		a := strings.TrimSpace(line[n+1:])
		if _, dup := answers[q]; dup || a == "" {
			return nil, want
		}
		answers[q] = a
	}
	if len(answers) != len(qs) {
		return nil, want
	}
	return answers, nil
}

// QuestionKey returns the key that picks the answer in Claude Code's question dialog, read by
// its label from the visible rows. The dialog takes a digit as pick and submit at once for one
// question with a single choice (checked against Claude Code 2.1.329). So only that case goes by
// keys, and only when the answer is one option's label, exactly, and one numbered row on screen
// carries it. Free text, several questions or multiple choice report false: nothing is sent.
func QuestionKey(a QuestionAnswer, rows []string) ([]byte, bool) {
	if len(a.Questions) != 1 || a.Questions[0].MultiSelect {
		return nil, false
	}
	q := a.Questions[0]
	text := a.Answers[q.Question]
	found := false
	for _, o := range q.Options {
		if strings.TrimSpace(o.Label) == text {
			found = true
		}
	}
	if !found {
		return nil, false
	}
	var key []byte
	for _, row := range rows {
		m := optionLine.FindStringSubmatch(row)
		if m == nil || strings.TrimSpace(m[2]) != text {
			continue
		}
		if key != nil {
			return nil, false
		}
		key = []byte(m[1])
	}
	return key, key != nil
}
